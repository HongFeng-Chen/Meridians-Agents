// helper.go milvusdb/repo 包内共用的小工具：结果集解析、表达式转义与列构造。
package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go_agent/internal/biz/rag"
	"go_agent/internal/data/milvusdb/models"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// parseHits 解析主集合检索结果集
func parseHits(rs *milvusclient.ResultSet) ([]rag.Hit, error) {
	if rs.Err != nil {
		return nil, rs.Err
	}
	n := rs.ResultCount
	if n == 0 {
		return []rag.Hit{}, nil
	}
	hits := make([]rag.Hit, 0, n)
	var idCol *column.ColumnInt64
	if rs.IDs != nil {
		idCol, _ = rs.IDs.(*column.ColumnInt64)
	}
	docIDCol, _ := rs.GetColumn(models.ChunkFieldDocID).(*column.ColumnVarChar)
	chunkIDCol, _ := rs.GetColumn(models.ChunkFieldChunkID).(*column.ColumnInt64)
	srcCol, _ := rs.GetColumn(models.ChunkFieldSource).(*column.ColumnVarChar)
	txtCol, _ := rs.GetColumn(models.ChunkFieldText).(*column.ColumnVarChar)

	for i := 0; i < n; i++ {
		h := rag.Hit{}
		if len(rs.Scores) > i {
			h.Score = rs.Scores[i]
		}
		if idCol != nil {
			if v, err := idCol.Value(i); err == nil {
				h.ID = v
			}
		}
		if docIDCol != nil {
			if v, err := docIDCol.Value(i); err == nil {
				h.DocID = v
			}
		}
		if chunkIDCol != nil {
			if v, err := chunkIDCol.Value(i); err == nil {
				h.ChunkIdx = v
			}
		}
		if srcCol != nil {
			if v, err := srcCol.Value(i); err == nil {
				h.Source = v
			}
		}
		if txtCol != nil {
			if v, err := txtCol.Value(i); err == nil {
				h.Text = v
			}
		}
		hits = append(hits, h)
	}
	return hits, nil
}

// collectionCount 取集合行数。
// 不能用 GetCollectionStats：它的 row_count 只统计已 flush 的 segment，
// 刚写入的小批量数据还停在 growing segment 里，会长期显示 0。
// 这里用 count(*) 查询（覆盖 growing + sealed），失败时再退回 row_count。
func collectionCount(ctx context.Context, cli *milvusclient.Client, collection string) (int64, error) {
	rs, err := cli.Query(ctx, milvusclient.NewQueryOption(collection).
		WithFilter("").WithOutputFields("count(*)"))
	if err == nil && rs.ResultCount > 0 {
		if v, gerr := rs.GetColumn("count(*)").Get(0); gerr == nil {
			switch n := v.(type) {
			case int64:
				return n, nil
			case int32:
				return int64(n), nil
			case string:
				var parsed int64
				if _, serr := fmt.Sscanf(n, "%d", &parsed); serr == nil {
					return parsed, nil
				}
			}
		}
	}
	// 兜底：退回统计接口（只含已 flush 数据）
	stats, serr := cli.GetCollectionStats(ctx, milvusclient.NewGetCollectionStatsOption(collection))
	if serr != nil {
		if err != nil {
			return 0, fmt.Errorf("count(*) 与 GetCollectionStats 均失败: %w", err)
		}
		return 0, serr
	}
	var n int64
	_, _ = fmt.Sscanf(stats["row_count"], "%d", &n)
	return n, nil
}

func repeatStr(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func stringSlice(n int, f func(int) string) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = f(i)
	}
	return out
}

// quoteStr 转义 Milvus 过滤表达式中的字符串字面量
func quoteStr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func parseRefs(s string) []string {
	var refs []string
	_ = json.Unmarshal([]byte(s), &refs)
	return refs
}

// normKey 实体名归一化（与 biz 层一致：ASCII 转小写）
func normKey(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

// tagsJSON 标签序列化
func tagsJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(tags)
	return string(b)
}

// clipContent 截断到集合字段上限（VarChar 16384 按字节）
func clipContent(s string) string {
	const max = 16000
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	// 避免切断多字节字符
	for len(cut) > 0 && !isUTF8Boundary(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func isUTF8Boundary(b byte) bool { return b&0xC0 != 0x80 }
