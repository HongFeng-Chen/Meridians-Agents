// memory.go 长期记忆向量集合存储：content 中文分词 + BM25 Function，
// dense 存外部 Embedding，检索时双路召回 + RRF 融合，可按 user_id 过滤。
package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/memory"
	"go_agent/internal/data/milvusdb"
	models "go_agent/internal/data/milvusdb/models"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// MemoryStore 长期记忆向量集合
type MemoryStore struct {
	*milvusdb.Client
	collection string
}

var _ memory.VectorStore = (*MemoryStore)(nil)

// NewMemoryStore 创建记忆向量存储（复用主连接，集合名 {base}_memories）
func NewMemoryStore(c *milvusdb.Client) memory.VectorStore {
	return &MemoryStore{Client: c, collection: c.Collection() + "_memories"}
}

// EnsureCollection 创建集合（若不存在）并加载；残缺集合（有集合无索引）自愈补建。
func (s *MemoryStore) EnsureCollection(ctx context.Context) error {
	cli := s.CLI()
	has, err := cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(s.collection))
	if err != nil {
		return fmt.Errorf("检查记忆集合 %s: %w", s.collection, err)
	}
	if !has {
		err = cli.CreateCollection(ctx,
			milvusclient.NewCreateCollectionOption(s.collection, models.MemorySchema(s.collection, s.Dim())).
				WithIndexOptions(
					milvusclient.NewCreateIndexOption(s.collection, models.MemoryFieldDense, models.HNSWCosine(models.MemoryFieldDense)),
					milvusclient.NewCreateIndexOption(s.collection, models.MemoryFieldSparse, models.BM25Index()),
				))
		if err != nil {
			return fmt.Errorf("create memory collection: %w", err)
		}
	}
	return s.loadAndWait(ctx)
}

// loadAndWait 加载集合；报 index not found 时补建索引（自愈残缺集合）
func (s *MemoryStore) loadAndWait(ctx context.Context) error {
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
				return fmt.Errorf("记忆集合 %s 缺索引且补建失败: %w", s.collection, ierr)
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
		return fmt.Errorf("等待记忆集合 %s 加载完成（上限 %s）: %w", s.collection, timeout, err)
	}
	return nil
}

// createIndexes 补建 dense/sparse 索引
func (s *MemoryStore) createIndexes(ctx context.Context) error {
	cli := s.CLI()
	denseTask, err := cli.CreateIndex(ctx, milvusclient.NewCreateIndexOption(
		s.collection, models.MemoryFieldDense, models.HNSWCosine(models.MemoryFieldDense)))
	if err != nil {
		return fmt.Errorf("create dense index: %w", err)
	}
	sparseTask, err := cli.CreateIndex(ctx, milvusclient.NewCreateIndexOption(
		s.collection, models.MemoryFieldSparse, models.BM25Index()))
	if err != nil {
		return fmt.Errorf("create sparse index: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := denseTask.Await(waitCtx); err != nil {
		return fmt.Errorf("等待 dense 索引: %w", err)
	}
	if err := sparseTask.Await(waitCtx); err != nil {
		return fmt.Errorf("等待 sparse 索引: %w", err)
	}
	return nil
}

// Insert 写入一条记忆向量
func (s *MemoryStore) Insert(ctx context.Context, m *memory.LongTermMemory, vec []float32) error {
	if m == nil || m.ID == "" {
		return fmt.Errorf("memory insert: 记忆为空")
	}
	_, err := s.CLI().Insert(ctx, milvusclient.NewColumnBasedInsertOption(s.collection,
		column.NewColumnVarChar(models.MemoryFieldMemoryID, []string{m.ID}),
		column.NewColumnVarChar(models.MemoryFieldUserID, []string{m.UserID}),
		column.NewColumnVarChar(models.MemoryFieldKind, []string{m.Kind}),
		column.NewColumnVarChar(models.MemoryFieldTags, []string{tagsJSON(m.Tags)}),
		column.NewColumnVarChar(models.MemoryFieldContent, []string{clipContent(m.Content)}),
		column.NewColumnFloatVector(models.MemoryFieldDense, s.Dim(), [][]float32{vec}),
	))
	if err != nil {
		return fmt.Errorf("insert memory vector: %w", err)
	}
	return nil
}

// Search 混合召回（BM25 + Dense，RRF 融合）；userID 非空时按用户过滤。
// 只返回 ID 与得分，内容由 MySQL 权威记录补全（biz 层负责过滤过期）。
func (s *MemoryStore) Search(ctx context.Context, query string, qvec []float32, topK int, userID string) ([]memory.RecalledMemory, error) {
	if topK <= 0 {
		topK = 8
	}
	denseReq := milvusclient.NewAnnRequest(models.MemoryFieldDense, topK, entity.FloatVector(qvec))
	sparseReq := milvusclient.NewAnnRequest(models.MemoryFieldSparse, topK, entity.Text(query))
	if strings.TrimSpace(userID) != "" && userID != "default" {
		filter := fmt.Sprintf("%s == %s", models.MemoryFieldUserID, quoteStr(userID))
		denseReq = denseReq.WithFilter(filter)
		sparseReq = sparseReq.WithFilter(filter)
	}

	opt := milvusclient.NewHybridSearchOption(s.collection, topK, denseReq, sparseReq).
		WithReranker(milvusclient.NewRRFReranker().WithK(60)).
		WithOutputFields(models.MemoryFieldMemoryID)

	resultSets, err := s.CLI().HybridSearch(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("hybrid search memories: %w", err)
	}
	if len(resultSets) == 0 {
		return nil, nil
	}
	rs := resultSets[0]
	n := rs.ResultCount
	out := make([]memory.RecalledMemory, 0, n)
	idCol, _ := rs.GetColumn(models.MemoryFieldMemoryID).(*column.ColumnVarChar)
	for i := 0; i < n; i++ {
		var id string
		if idCol != nil {
			if v, err := idCol.Value(i); err == nil {
				id = v
			}
		}
		if id == "" {
			continue
		}
		out = append(out, memory.RecalledMemory{
			LongTermMemory: &memory.LongTermMemory{ID: id},
			Score:          rs.Scores[i],
		})
	}
	return out, nil
}

// DeleteByMemory 按 memory_id 删除向量
func (s *MemoryStore) DeleteByMemory(ctx context.Context, memoryID string) (int64, error) {
	if strings.TrimSpace(memoryID) == "" {
		return 0, nil
	}
	res, err := s.CLI().Delete(ctx, milvusclient.NewDeleteOption(s.collection).
		WithExpr(fmt.Sprintf("%s == %s", models.MemoryFieldMemoryID, quoteStr(memoryID))))
	if err != nil {
		return 0, fmt.Errorf("delete memory vector: %w", err)
	}
	return res.DeleteCount, nil
}

// Count 记忆向量总数
func (s *MemoryStore) Count(ctx context.Context) (int64, error) {
	return collectionCount(ctx, s.CLI(), s.collection)
}
