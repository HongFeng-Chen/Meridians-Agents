// tool.go 工具向量集合存储：只放 tool_id + 向量 + 过滤字段。
//
// 召回只取 id 与相似度，详情由用例层回 MySQL/Redis 补齐——
// 向量库不存 schema 与描述全文，避免出现第二份真相。
package repo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/data/milvusdb"
	models "go_agent/internal/data/milvusdb/models"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// ToolStore MCP 工具向量集合
type ToolStore struct {
	*milvusdb.Client
	collection string

	mu      sync.Mutex
	ensured bool
}

var _ mcp.VectorStore = (*ToolStore)(nil)

// NewToolStore 创建工具向量存储（复用主连接，集合名 {base}_tools）
func NewToolStore(c *milvusdb.Client) mcp.VectorStore {
	return &ToolStore{Client: c, collection: c.Collection() + "_tools"}
}

// EnsureCollection 创建集合（若不存在）并加载；成功后置位，后续调用直接返回。
func (s *ToolStore) EnsureCollection(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ensured {
		return nil
	}
	cli := s.CLI()
	has, err := cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(s.collection))
	if err != nil {
		return fmt.Errorf("检查工具集合 %s: %w", s.collection, err)
	}
	if !has {
		err = cli.CreateCollection(ctx,
			milvusclient.NewCreateCollectionOption(s.collection, models.ToolSchema(s.collection, s.Dim())).
				WithIndexOptions(
					milvusclient.NewCreateIndexOption(s.collection, models.ToolFieldDense,
						models.HNSWCosine(models.ToolFieldDense)),
				))
		if err != nil {
			return fmt.Errorf("create tool collection: %w", err)
		}
	}
	if err := s.loadAndWait(ctx); err != nil {
		return err
	}
	s.ensured = true
	return nil
}

// loadAndWait 加载集合；报 index not found 时补建索引（自愈残缺集合）
func (s *ToolStore) loadAndWait(ctx context.Context) error {
	cli := s.CLI()
	timeout := s.LoadTimeout()
	var task milvusclient.LoadTask
	var err error
	healed := false
	for attempt := 0; attempt < 5; attempt++ {
		task, err = cli.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(s.collection))
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "index not found") {
			return fmt.Errorf("load collection %s: %w", s.collection, err)
		}
		if !healed {
			healed = true
			if ierr := s.createIndexes(ctx); ierr != nil {
				return fmt.Errorf("工具集合 %s 缺索引且补建失败: %w", s.collection, ierr)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("load collection %s: 索引构建超时: %w", s.collection, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := task.Await(waitCtx); err != nil {
		return fmt.Errorf("等待工具集合 %s 加载完成（上限 %s）: %w", s.collection, timeout, err)
	}
	return nil
}

func (s *ToolStore) createIndexes(ctx context.Context) error {
	task, err := s.CLI().CreateIndex(ctx, milvusclient.NewCreateIndexOption(
		s.collection, models.ToolFieldDense, models.HNSWCosine(models.ToolFieldDense)))
	if err != nil {
		return fmt.Errorf("create dense index: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := task.Await(waitCtx); err != nil {
		return fmt.Errorf("等待 dense 索引: %w", err)
	}
	return nil
}

// Upsert 覆盖写入（tool_id 为主键）：重复写入即更新，含 is_enabled 等标量。
func (s *ToolStore) Upsert(ctx context.Context, tools []mcp.Tool, vecs [][]float32) error {
	if len(tools) == 0 {
		return nil
	}
	if len(tools) != len(vecs) {
		return fmt.Errorf("工具向量写入: 条目数不一致 tools=%d vectors=%d", len(tools), len(vecs))
	}
	dim := s.Dim()
	ids := make([]int64, 0, len(tools))
	serverIDs := make([]int64, 0, len(tools))
	namespaces := make([]string, 0, len(tools))
	enabled := make([]bool, 0, len(tools))
	for i := range tools {
		if tools[i].ID == 0 {
			return fmt.Errorf("工具 %s 缺少数据库 ID，无法写入向量", tools[i].UniqueName)
		}
		if len(vecs[i]) != dim {
			return fmt.Errorf("工具 %s 向量维度 %d 与集合维度 %d 不一致", tools[i].UniqueName, len(vecs[i]), dim)
		}
		ids = append(ids, int64(tools[i].ID))
		serverIDs = append(serverIDs, int64(tools[i].ServerID))
		namespaces = append(namespaces, clipNamespace(tools[i].Namespace))
		enabled = append(enabled, tools[i].Enabled)
	}
	_, err := s.CLI().Upsert(ctx, milvusclient.NewColumnBasedInsertOption(s.collection,
		column.NewColumnInt64(models.ToolFieldID, ids),
		column.NewColumnInt64(models.ToolFieldServerID, serverIDs),
		column.NewColumnVarChar(models.ToolFieldNamespace, namespaces),
		column.NewColumnBool(models.ToolFieldEnabled, enabled),
		column.NewColumnFloatVector(models.ToolFieldDense, dim, vecs),
	))
	if err != nil {
		return fmt.Errorf("upsert tool vectors: %w", err)
	}
	return nil
}

// Search 语义召回：只返回 id 与相似度
func (s *ToolStore) Search(ctx context.Context, vec []float32, topK int, f mcp.VectorFilter) ([]mcp.VectorHit, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	if topK <= 0 {
		topK = 8
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return nil, err
	}
	opt := milvusclient.NewSearchOption(s.collection, topK, []entity.Vector{entity.FloatVector(vec)}).
		WithANNSField(models.ToolFieldDense).
		WithOutputFields(models.ToolFieldID)
	if expr := buildToolFilter(f); expr != "" {
		opt = opt.WithFilter(expr)
	}
	sets, err := s.CLI().Search(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("search tool vectors: %w", err)
	}
	if len(sets) == 0 {
		return nil, nil
	}
	rs := sets[0]
	if rs.Err != nil {
		return nil, rs.Err
	}
	n := rs.ResultCount
	out := make([]mcp.VectorHit, 0, n)
	idCol, _ := rs.IDs.(*column.ColumnInt64)
	for i := 0; i < n; i++ {
		var id int64
		if idCol != nil {
			if v, err := idCol.Value(i); err == nil {
				id = v
			}
		}
		if id == 0 {
			continue
		}
		hit := mcp.VectorHit{ToolID: uint64(id)}
		if len(rs.Scores) > i {
			hit.Score = float64(rs.Scores[i])
		}
		out = append(out, hit)
	}
	return out, nil
}

// Delete 按 tool_id 批量删除向量
func (s *ToolStore) Delete(ctx context.Context, toolIDs []uint64) error {
	if len(toolIDs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(toolIDs))
	for _, id := range toolIDs {
		ids = append(ids, int64(id))
	}
	if _, err := s.CLI().Delete(ctx, milvusclient.NewDeleteOption(s.collection).
		WithInt64IDs(models.ToolFieldID, ids)); err != nil {
		return fmt.Errorf("delete tool vectors: %w", err)
	}
	return nil
}

// Count 集合内向量数
func (s *ToolStore) Count(ctx context.Context) (int64, error) {
	has, err := s.CLI().HasCollection(ctx, milvusclient.NewHasCollectionOption(s.collection))
	if err != nil || !has {
		return 0, nil
	}
	return collectionCount(ctx, s.CLI(), s.collection)
}

// buildToolFilter 组装标量过滤表达式
func buildToolFilter(f mcp.VectorFilter) string {
	conds := make([]string, 0, 3)
	if f.EnabledOnly {
		conds = append(conds, models.ToolFieldEnabled+" == true")
	}
	if ns := strings.TrimSpace(f.Namespace); ns != "" {
		conds = append(conds, fmt.Sprintf("%s == %s", models.ToolFieldNamespace, quoteStr(ns)))
	}
	if f.ServerID > 0 {
		conds = append(conds, fmt.Sprintf("%s == %d", models.ToolFieldServerID, f.ServerID))
	}
	return strings.Join(conds, " and ")
}

// clipNamespace 命名空间字段上限 128，超长截断避免写入失败
func clipNamespace(ns string) string {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return "default"
	}
	r := []rune(ns)
	if len(r) > 128 {
		return string(r[:128])
	}
	return ns
}
