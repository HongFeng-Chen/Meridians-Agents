package repo

// agent.go Agent 向量注册表存储：只放 agent_id + 向量 + 过滤字段，
// 定义详情由用例层回 MySQL/Redis 补齐（与工具集合同一原则）。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/data/milvusdb"
	models "go_agent/internal/data/milvusdb/models"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// AgentStore Agent 向量注册表（集合名 {base}_agents）
type AgentStore struct {
	*milvusdb.Client
	collection string

	mu      sync.Mutex
	ensured bool
}

var _ agentfactory.VectorStore = (*AgentStore)(nil)

// NewAgentStore 创建 Agent 向量存储（复用主连接）
func NewAgentStore(c *milvusdb.Client) agentfactory.VectorStore {
	return &AgentStore{Client: c, collection: c.Collection() + "_agents"}
}

func (s *AgentStore) EnsureCollection(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ensured {
		return nil
	}
	cli := s.CLI()
	has, err := cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(s.collection))
	if err != nil {
		return fmt.Errorf("检查 Agent 集合 %s: %w", s.collection, err)
	}
	if !has {
		err = cli.CreateCollection(ctx,
			milvusclient.NewCreateCollectionOption(s.collection, models.AgentSchema(s.collection, s.Dim())).
				WithIndexOptions(
					milvusclient.NewCreateIndexOption(s.collection, models.AgentFieldDense,
						models.HNSWCosine(models.AgentFieldDense)),
				))
		if err != nil {
			return fmt.Errorf("create agent collection: %w", err)
		}
	}
	if err := s.loadAndWait(ctx); err != nil {
		return err
	}
	s.ensured = true
	return nil
}

func (s *AgentStore) loadAndWait(ctx context.Context) error {
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
				return fmt.Errorf("Agent 集合 %s 缺索引且补建失败: %w", s.collection, ierr)
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
		return fmt.Errorf("等待 Agent 集合 %s 加载完成（上限 %s）: %w", s.collection, timeout, err)
	}
	return nil
}

func (s *AgentStore) createIndexes(ctx context.Context) error {
	task, err := s.CLI().CreateIndex(ctx, milvusclient.NewCreateIndexOption(
		s.collection, models.AgentFieldDense, models.HNSWCosine(models.AgentFieldDense)))
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

// Upsert 覆盖写入（agent_id 为主键）
func (s *AgentStore) Upsert(ctx context.Context, defs []agentfactory.AgentDef, vecs [][]float32) error {
	if len(defs) == 0 {
		return nil
	}
	if len(defs) != len(vecs) {
		return fmt.Errorf("Agent 向量写入: 条目数不一致 defs=%d vectors=%d", len(defs), len(vecs))
	}
	dim := s.Dim()
	ids := make([]int64, 0, len(defs))
	enabled := make([]bool, 0, len(defs))
	sources := make([]string, 0, len(defs))
	for i := range defs {
		if defs[i].ID == 0 {
			return fmt.Errorf("Agent %s 缺少数据库 ID，无法写入向量", defs[i].AgentKey)
		}
		if len(vecs[i]) != dim {
			return fmt.Errorf("Agent %s 向量维度 %d 与集合维度 %d 不一致", defs[i].AgentKey, len(vecs[i]), dim)
		}
		ids = append(ids, int64(defs[i].ID))
		enabled = append(enabled, defs[i].Enabled)
		src := defs[i].Source
		if len(src) > 16 {
			src = src[:16]
		}
		sources = append(sources, src)
	}
	_, err := s.CLI().Upsert(ctx, milvusclient.NewColumnBasedInsertOption(s.collection,
		column.NewColumnInt64(models.AgentFieldID, ids),
		column.NewColumnBool(models.AgentFieldEnabled, enabled),
		column.NewColumnVarChar(models.AgentFieldSource, sources),
		column.NewColumnFloatVector(models.AgentFieldDense, dim, vecs),
	))
	if err != nil {
		return fmt.Errorf("upsert agent vectors: %w", err)
	}
	return nil
}

// Search 语义召回：只返回 id 与相似度
func (s *AgentStore) Search(ctx context.Context, vec []float32, topK int, f agentfactory.VectorFilter) ([]agentfactory.VectorHit, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	if topK <= 0 {
		topK = 5
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return nil, err
	}
	opt := milvusclient.NewSearchOption(s.collection, topK, []entity.Vector{entity.FloatVector(vec)}).
		WithANNSField(models.AgentFieldDense).
		WithOutputFields(models.AgentFieldID)
	if expr := buildAgentFilter(f); expr != "" {
		opt = opt.WithFilter(expr)
	}
	sets, err := s.CLI().Search(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("search agent vectors: %w", err)
	}
	if len(sets) == 0 {
		return nil, nil
	}
	rs := sets[0]
	if rs.Err != nil {
		return nil, rs.Err
	}
	n := rs.ResultCount
	out := make([]agentfactory.VectorHit, 0, n)
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
		hit := agentfactory.VectorHit{AgentID: uint64(id)}
		if len(rs.Scores) > i {
			hit.Score = float64(rs.Scores[i])
		}
		out = append(out, hit)
	}
	return out, nil
}

// Delete 按 agent_id 批量删除向量
func (s *AgentStore) Delete(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	i64 := make([]int64, 0, len(ids))
	for _, id := range ids {
		i64 = append(i64, int64(id))
	}
	if _, err := s.CLI().Delete(ctx, milvusclient.NewDeleteOption(s.collection).
		WithInt64IDs(models.AgentFieldID, i64)); err != nil {
		return fmt.Errorf("delete agent vectors: %w", err)
	}
	return nil
}

// Count 集合内向量数
func (s *AgentStore) Count(ctx context.Context) (int64, error) {
	has, err := s.CLI().HasCollection(ctx, milvusclient.NewHasCollectionOption(s.collection))
	if err != nil || !has {
		return 0, nil
	}
	return collectionCount(ctx, s.CLI(), s.collection)
}

// buildAgentFilter 组装标量过滤表达式
func buildAgentFilter(f agentfactory.VectorFilter) string {
	conds := make([]string, 0, 2)
	if f.Enabled != nil {
		if *f.Enabled {
			conds = append(conds, models.AgentFieldEnabled+" == true")
		} else {
			conds = append(conds, models.AgentFieldEnabled+" == false")
		}
	}
	if src := strings.TrimSpace(f.Source); src != "" {
		conds = append(conds, fmt.Sprintf("%s == %s", models.AgentFieldSource, quoteStr(src)))
	}
	return strings.Join(conds, " and ")
}
