// Package repo Milvus 存储实现层：依赖 milvusdb（连接）与 models（集合 Schema），
// 实现 biz 层声明的 ChunkStore / graphrag.Store / memory.VectorStore 接口。
package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/rag"
	"go_agent/internal/data/milvusdb"
	models "go_agent/internal/data/milvusdb/models"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// ChunkStore 主集合：BM25 + Dense 混合检索
type ChunkStore struct {
	*milvusdb.Client
	collection string
}

var _ rag.ChunkStore = (*ChunkStore)(nil)

// NewChunkStore 创建分块向量存储
func NewChunkStore(c *milvusdb.Client) rag.ChunkStore {
	return &ChunkStore{Client: c, collection: c.Collection()}
}

// EnsureCollection 创建集合（若不存在）：Schema + BM25 Function + 双索引，
// 并确保集合已加载（Milvus 重启后需重新 Load，否则检索报未加载）。
func (s *ChunkStore) EnsureCollection(ctx context.Context) error {
	cli := s.CLI()
	exists, err := cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(s.collection))
	if err != nil {
		return fmt.Errorf("检查集合 %s: %w", s.collection, err)
	}
	if exists {
		return s.loadAndWait(ctx)
	}

	err = cli.CreateCollection(ctx,
		milvusclient.NewCreateCollectionOption(s.collection, models.ChunkSchema(s.collection, s.Dim())).
			WithIndexOptions(
				milvusclient.NewCreateIndexOption(s.collection, models.ChunkFieldDense, models.HNSWCosine(models.ChunkFieldDense)),
				milvusclient.NewCreateIndexOption(s.collection, models.ChunkFieldSparse, models.BM25Index()),
			))
	if err != nil {
		return fmt.Errorf("create collection: %w", err)
	}
	return s.loadAndWait(ctx)
}

// loadAndWait 加载集合并等待完成；集合存在但索引缺失（上次崩溃留下的残缺集合）
// 时自动补建索引再加载。
func (s *ChunkStore) loadAndWait(ctx context.Context) error {
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
				return fmt.Errorf("集合 %s 缺索引且补建失败: %w", s.collection, ierr)
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
		return fmt.Errorf("等待集合 %s 加载完成（上限 %s）: %w", s.collection, timeout, err)
	}
	return nil
}

// createIndexes 为已存在的集合补建 dense/sparse 索引（自愈残缺集合）
func (s *ChunkStore) createIndexes(ctx context.Context) error {
	cli := s.CLI()
	denseTask, err := cli.CreateIndex(ctx, milvusclient.NewCreateIndexOption(
		s.collection, models.ChunkFieldDense, models.HNSWCosine(models.ChunkFieldDense)))
	if err != nil {
		return fmt.Errorf("create dense index: %w", err)
	}
	sparseTask, err := cli.CreateIndex(ctx, milvusclient.NewCreateIndexOption(
		s.collection, models.ChunkFieldSparse, models.BM25Index()))
	if err != nil {
		return fmt.Errorf("create sparse index: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := denseTask.Await(waitCtx); err != nil {
		return fmt.Errorf("await dense index: %w", err)
	}
	if err := sparseTask.Await(waitCtx); err != nil {
		return fmt.Errorf("await sparse index: %w", err)
	}
	return nil
}

// Insert 批量写入分块。sparse 由 Milvus 服务端 BM25 Function 自动生成。
func (s *ChunkStore) Insert(ctx context.Context, docID, source string, chunks []string, vectors [][]float32) error {
	if len(chunks) != len(vectors) {
		return fmt.Errorf("chunks(%d) != vectors(%d)", len(chunks), len(vectors))
	}
	if len(chunks) == 0 {
		return nil
	}
	idxs := make([]int64, len(chunks))
	for i := range idxs {
		idxs[i] = int64(i)
	}
	_, err := s.CLI().Insert(ctx, milvusclient.NewColumnBasedInsertOption(s.collection,
		column.NewColumnVarChar(models.ChunkFieldDocID, repeatStr(docID, len(chunks))),
		column.NewColumnInt64(models.ChunkFieldChunkID, idxs),
		column.NewColumnVarChar(models.ChunkFieldSource, repeatStr(source, len(chunks))),
		column.NewColumnVarChar(models.ChunkFieldText, chunks),
		column.NewColumnFloatVector(models.ChunkFieldDense, s.Dim(), vectors),
	))
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return nil
}

// HybridSearch 混合检索：Dense 语义召回 + BM25 关键词召回，RRF 融合。
func (s *ChunkStore) HybridSearch(ctx context.Context, queryText string, queryVec []float32, topK int, filter string) ([]rag.Hit, error) {
	if len(queryVec) != s.Dim() {
		return nil, fmt.Errorf("query vector dim %d != %d", len(queryVec), s.Dim())
	}
	if topK <= 0 {
		topK = 10
	}
	denseReq := milvusclient.NewAnnRequest(models.ChunkFieldDense, topK, entity.FloatVector(queryVec))
	sparseReq := milvusclient.NewAnnRequest(models.ChunkFieldSparse, topK, entity.Text(queryText))
	if filter != "" {
		denseReq = denseReq.WithFilter(filter)
		sparseReq = sparseReq.WithFilter(filter)
	}

	resultSets, err := s.CLI().HybridSearch(ctx,
		milvusclient.NewHybridSearchOption(s.collection, topK, denseReq, sparseReq).
			WithReranker(milvusclient.NewRRFReranker().WithK(60)).
			WithOutputFields(models.ChunkFieldDocID, models.ChunkFieldSource, models.ChunkFieldText))
	if err != nil {
		return nil, fmt.Errorf("hybrid search: %w", err)
	}
	if len(resultSets) == 0 {
		return []rag.Hit{}, nil
	}
	return parseHits(&resultSets[0])
}

// GetTexts 按标量过滤表达式取回文本块（GraphRAG 解析 chunk 引用取证）
func (s *ChunkStore) GetTexts(ctx context.Context, filter string, limit int) ([]rag.Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	rs, err := s.CLI().Query(ctx, milvusclient.NewQueryOption(s.collection).
		WithFilter(filter).
		WithOutputFields(models.ChunkFieldDocID, models.ChunkFieldSource, models.ChunkFieldText, models.ChunkFieldChunkID).
		WithLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("query texts: %w", err)
	}
	return parseHits(&rs)
}

// Count 集合内实体总数
func (s *ChunkStore) Count(ctx context.Context) (int64, error) {
	return collectionCount(ctx, s.CLI(), s.collection)
}

// DeleteDoc 按 doc_id 删除该文档的全部分块
func (s *ChunkStore) DeleteDoc(ctx context.Context, docID string) (int64, error) {
	res, err := s.CLI().Delete(ctx, milvusclient.NewDeleteOption(s.collection).
		WithExpr(fmt.Sprintf("%s == %s", models.ChunkFieldDocID, quoteStr(docID))))
	if err != nil {
		return 0, fmt.Errorf("delete doc: %w", err)
	}
	return res.DeleteCount, nil
}
