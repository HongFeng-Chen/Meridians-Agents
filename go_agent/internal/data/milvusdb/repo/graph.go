// graph.go GraphRAG 图集合存储：实体（BM25+Dense）与关系边（Dense）。
package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/data/milvusdb"
	models "go_agent/internal/data/milvusdb/models"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// GraphStore GraphRAG 的两个图集合：实体（BM25+Dense）与关系边（Dense）
type GraphStore struct {
	*milvusdb.Client
	entityColl   string
	relationColl string
}

var _ graphrag.Store = (*GraphStore)(nil)

// NewGraphStore 创建图存储（复用主集合的连接）
func NewGraphStore(c *milvusdb.Client) graphrag.Store {
	base := c.Collection()
	return &GraphStore{
		Client:       c,
		entityColl:   base + "_graph_entities",
		relationColl: base + "_graph_relations",
	}
}

// EnsureCollections 创建实体/关系集合（若不存在）
func (g *GraphStore) EnsureCollections(ctx context.Context) error {
	cli := g.CLI()
	has, err := cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(g.entityColl))
	if err != nil {
		return fmt.Errorf("检查实体集合: %w", err)
	}
	if !has {
		err = cli.CreateCollection(ctx,
			milvusclient.NewCreateCollectionOption(g.entityColl, models.EntitySchema(g.entityColl, g.Dim())).
				WithIndexOptions(
					milvusclient.NewCreateIndexOption(g.entityColl, models.EntityFieldDense, models.HNSWCosine(models.EntityFieldDense)),
					milvusclient.NewCreateIndexOption(g.entityColl, models.EntityFieldSparse, models.BM25Index()),
				))
		if err != nil {
			return fmt.Errorf("create entity collection: %w", err)
		}
	}
	if err := g.load(ctx, g.entityColl); err != nil {
		return err
	}

	has, err = cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(g.relationColl))
	if err != nil {
		return fmt.Errorf("检查关系集合: %w", err)
	}
	if !has {
		err = cli.CreateCollection(ctx,
			milvusclient.NewCreateCollectionOption(g.relationColl, models.RelationSchema(g.relationColl, g.Dim())).
				WithIndexOptions(
					milvusclient.NewCreateIndexOption(g.relationColl, models.RelationFieldDense, models.HNSWCosine(models.RelationFieldDense)),
				))
		if err != nil {
			return fmt.Errorf("create relation collection: %w", err)
		}
	}
	return g.load(ctx, g.relationColl)
}

func (g *GraphStore) load(ctx context.Context, coll string) error {
	timeout := g.LoadTimeout()
	task, err := g.CLI().LoadCollection(ctx, milvusclient.NewLoadCollectionOption(coll))
	if err != nil {
		return fmt.Errorf("load %s: %w", coll, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := task.Await(waitCtx); err != nil {
		return fmt.Errorf("等待集合 %s 加载完成（上限 %s）: %w", coll, timeout, err)
	}
	return nil
}

// InsertEntities 批量写入实体（已合并去重后的结果）
func (g *GraphStore) InsertEntities(ctx context.Context, ents []graphrag.Entity, contents []string, vectors [][]float32) error {
	if len(ents) == 0 {
		return nil
	}
	refs := make([]string, len(ents))
	freqs := make([]int64, len(ents))
	for i, e := range ents {
		b, _ := json.Marshal(e.ChunkRefs)
		refs[i] = string(b)
		freqs[i] = int64(e.Freq)
	}
	_, err := g.CLI().Insert(ctx, milvusclient.NewColumnBasedInsertOption(g.entityColl,
		column.NewColumnVarChar(models.EntityFieldName, stringSlice(len(ents), func(i int) string { return ents[i].Name })),
		column.NewColumnVarChar(models.EntityFieldType, stringSlice(len(ents), func(i int) string { return ents[i].Type })),
		column.NewColumnVarChar(models.EntityFieldDescription, stringSlice(len(ents), func(i int) string { return ents[i].Description })),
		column.NewColumnInt64(models.EntityFieldFreq, freqs),
		column.NewColumnVarChar(models.EntityFieldRefs, refs),
		column.NewColumnVarChar(models.EntityFieldSource, contents),
		column.NewColumnFloatVector(models.EntityFieldDense, g.Dim(), vectors),
	))
	if err != nil {
		return fmt.Errorf("insert entities: %w", err)
	}
	return nil
}

// InsertRelations 批量写入关系边
func (g *GraphStore) InsertRelations(ctx context.Context, rels []graphrag.Relation, vectors [][]float32) error {
	if len(rels) == 0 {
		return nil
	}
	refs := make([]string, len(rels))
	weights := make([]float64, len(rels))
	for i, r := range rels {
		b, _ := json.Marshal(r.ChunkRefs)
		refs[i] = string(b)
		weights[i] = r.Weight
	}
	_, err := g.CLI().Insert(ctx, milvusclient.NewColumnBasedInsertOption(g.relationColl,
		column.NewColumnVarChar(models.RelationFieldSource, stringSlice(len(rels), func(i int) string { return rels[i].Source })),
		column.NewColumnVarChar(models.RelationFieldTarget, stringSlice(len(rels), func(i int) string { return rels[i].Target })),
		column.NewColumnVarChar(models.RelationFieldRelation, stringSlice(len(rels), func(i int) string { return rels[i].Relation })),
		column.NewColumnVarChar(models.RelationFieldDescription, stringSlice(len(rels), func(i int) string { return rels[i].Description })),
		column.NewColumnDouble(models.RelationFieldWeight, weights),
		column.NewColumnVarChar(models.RelationFieldRefs, refs),
		column.NewColumnFloatVector(models.RelationFieldDense, g.Dim(), vectors),
	))
	if err != nil {
		return fmt.Errorf("insert relations: %w", err)
	}
	return nil
}

// ExistingEntities 按名字批量查询已存在实体（跨文档去重合并用）
func (g *GraphStore) ExistingEntities(ctx context.Context, names []string) (map[string]graphrag.Entity, error) {
	out := make(map[string]graphrag.Entity)
	if len(names) == 0 {
		return out, nil
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = quoteStr(n)
	}
	rs, err := g.CLI().Query(ctx, milvusclient.NewQueryOption(g.entityColl).
		WithFilter(fmt.Sprintf("%s in [%s]", models.EntityFieldName, strings.Join(quoted, ","))).
		WithOutputFields(models.EntityFieldName, models.EntityFieldType, models.EntityFieldDescription, models.EntityFieldFreq, models.EntityFieldRefs).
		WithLimit(len(names)*4))
	if err != nil {
		return nil, fmt.Errorf("query existing entities: %w", err)
	}
	for _, e := range parseEntities(rs) {
		out[normKey(e.Name)] = e
	}
	return out, nil
}

// SearchEntities 混合检索实体（BM25 + Dense，RRF 融合）
func (g *GraphStore) SearchEntities(ctx context.Context, question string, qvec []float32, topK int) ([]graphrag.Entity, error) {
	if topK <= 0 {
		topK = 8
	}
	denseReq := milvusclient.NewAnnRequest(models.EntityFieldDense, topK, entity.FloatVector(qvec))
	sparseReq := milvusclient.NewAnnRequest(models.EntityFieldSparse, topK, entity.Text(question))

	resultSets, err := g.CLI().HybridSearch(ctx,
		milvusclient.NewHybridSearchOption(g.entityColl, topK, denseReq, sparseReq).
			WithReranker(milvusclient.NewRRFReranker().WithK(60)).
			WithOutputFields(models.EntityFieldName, models.EntityFieldType, models.EntityFieldDescription, models.EntityFieldFreq, models.EntityFieldRefs))
	if err != nil {
		return nil, fmt.Errorf("hybrid search entities: %w", err)
	}
	if len(resultSets) == 0 {
		return nil, nil
	}
	return parseEntities(resultSets[0]), nil
}

// RelationsOf 查询与给定实体相连的边（1 度扩展）
func (g *GraphStore) RelationsOf(ctx context.Context, names []string, limit int) ([]graphrag.Relation, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = quoteStr(n)
	}
	inList := strings.Join(quoted, ",")
	rs, err := g.CLI().Query(ctx, milvusclient.NewQueryOption(g.relationColl).
		WithFilter(fmt.Sprintf("%s in [%s] || %s in [%s]", models.RelationFieldSource, inList, models.RelationFieldTarget, inList)).
		WithOutputFields(models.RelationFieldSource, models.RelationFieldTarget, models.RelationFieldRelation, models.RelationFieldDescription, models.RelationFieldWeight, models.RelationFieldRefs).
		WithLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("query relations: %w", err)
	}
	return parseRelations(rs), nil
}

// EntitiesByNames 按名字批量取回实体描述（多跳扩展后的节点补全）
func (g *GraphStore) EntitiesByNames(ctx context.Context, names []string) ([]graphrag.Entity, error) {
	if len(names) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = quoteStr(n)
	}
	rs, err := g.CLI().Query(ctx, milvusclient.NewQueryOption(g.entityColl).
		WithFilter(fmt.Sprintf("%s in [%s]", models.EntityFieldName, strings.Join(quoted, ","))).
		WithOutputFields(models.EntityFieldName, models.EntityFieldType, models.EntityFieldDescription, models.EntityFieldFreq, models.EntityFieldRefs).
		WithLimit(len(names)))
	if err != nil {
		return nil, fmt.Errorf("query entities by names: %w", err)
	}
	return parseEntities(rs), nil
}

// Counts 实体与关系总数
func (g *GraphStore) Counts(ctx context.Context) (int64, int64, error) {
	ents, err := collectionCount(ctx, g.CLI(), g.entityColl)
	if err != nil {
		return 0, 0, err
	}
	rels, err := collectionCount(ctx, g.CLI(), g.relationColl)
	if err != nil {
		return 0, 0, err
	}
	return ents, rels, nil
}

// DeleteOrphanNodes 删除仅被该文档引用（prefix 形如 "doc_id#"）的节点与边。
// 跨文档共享的实体/关系会保留，避免误删。
func (g *GraphStore) DeleteOrphanNodes(ctx context.Context, prefix string) (int, int, error) {
	entIDs, err := g.orphanIDs(ctx, g.entityColl, models.EntityFieldID, models.EntityFieldRefs, prefix)
	if err != nil {
		return 0, 0, err
	}
	if len(entIDs) > 0 {
		if _, err := g.CLI().Delete(ctx, milvusclient.NewDeleteOption(g.entityColl).WithInt64IDs(models.EntityFieldID, entIDs)); err != nil {
			return 0, 0, fmt.Errorf("delete entities: %w", err)
		}
	}

	relIDs, err := g.orphanIDs(ctx, g.relationColl, models.RelationFieldID, models.RelationFieldRefs, prefix)
	if err != nil {
		return 0, len(relIDs), err
	}
	if len(relIDs) > 0 {
		if _, err := g.CLI().Delete(ctx, milvusclient.NewDeleteOption(g.relationColl).WithInt64IDs(models.RelationFieldID, relIDs)); err != nil {
			return len(entIDs), 0, fmt.Errorf("delete relations: %w", err)
		}
	}
	return len(entIDs), len(relIDs), nil
}

// orphanIDs 找出所有引用都来自该文档的节点主键
func (g *GraphStore) orphanIDs(ctx context.Context, coll, idField, refsField, prefix string) ([]int64, error) {
	rs, err := g.CLI().Query(ctx, milvusclient.NewQueryOption(coll).
		WithFilter(fmt.Sprintf("%s >= 0", idField)).
		WithOutputFields(idField, refsField).
		WithLimit(16384))
	if err != nil {
		return nil, fmt.Errorf("query refs of %s: %w", coll, err)
	}
	idCol, _ := rs.GetColumn(idField).(*column.ColumnInt64)
	refsCol, _ := rs.GetColumn(refsField).(*column.ColumnVarChar)
	if idCol == nil {
		return nil, nil
	}
	var ids []int64
	for i := 0; i < rs.ResultCount; i++ {
		id, err := idCol.Value(i)
		if err != nil {
			continue
		}
		var refs []string
		if refsCol != nil {
			if v, err := refsCol.Value(i); err == nil {
				refs = parseRefs(v)
			}
		}
		if onlyRefsFrom(refs, prefix) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// onlyRefsFrom 判断引用是否全部来自该文档
func onlyRefsFrom(refs []string, prefix string) bool {
	if len(refs) == 0 {
		return false
	}
	for _, r := range refs {
		if !strings.HasPrefix(r, prefix) {
			return false
		}
	}
	return true
}

// parseEntities 从结果集解析实体（保持结果集顺序）
func parseEntities(rs milvusclient.ResultSet) []graphrag.Entity {
	n := rs.ResultCount
	if n == 0 || rs.Err != nil {
		return nil
	}
	nameCol, _ := rs.GetColumn(models.EntityFieldName).(*column.ColumnVarChar)
	typeCol, _ := rs.GetColumn(models.EntityFieldType).(*column.ColumnVarChar)
	descCol, _ := rs.GetColumn(models.EntityFieldDescription).(*column.ColumnVarChar)
	freqCol, _ := rs.GetColumn(models.EntityFieldFreq).(*column.ColumnInt64)
	refsCol, _ := rs.GetColumn(models.EntityFieldRefs).(*column.ColumnVarChar)

	ents := make([]graphrag.Entity, 0, n)
	for i := 0; i < n; i++ {
		var e graphrag.Entity
		if nameCol != nil {
			if v, err := nameCol.Value(i); err == nil {
				e.Name = v
			}
		}
		if typeCol != nil {
			if v, err := typeCol.Value(i); err == nil {
				e.Type = v
			}
		}
		if descCol != nil {
			if v, err := descCol.Value(i); err == nil {
				e.Description = v
			}
		}
		if freqCol != nil {
			if v, err := freqCol.Value(i); err == nil {
				e.Freq = int(v)
			}
		}
		if refsCol != nil {
			if v, err := refsCol.Value(i); err == nil {
				e.ChunkRefs = parseRefs(v)
			}
		}
		if e.Name != "" {
			ents = append(ents, e)
		}
	}
	return ents
}

// parseRelations 从结果集解析关系边
func parseRelations(rs milvusclient.ResultSet) []graphrag.Relation {
	n := rs.ResultCount
	if n == 0 || rs.Err != nil {
		return nil
	}
	srcCol, _ := rs.GetColumn(models.RelationFieldSource).(*column.ColumnVarChar)
	tgtCol, _ := rs.GetColumn(models.RelationFieldTarget).(*column.ColumnVarChar)
	relCol, _ := rs.GetColumn(models.RelationFieldRelation).(*column.ColumnVarChar)
	descCol, _ := rs.GetColumn(models.RelationFieldDescription).(*column.ColumnVarChar)
	wCol, _ := rs.GetColumn(models.RelationFieldWeight).(*column.ColumnDouble)
	refsCol, _ := rs.GetColumn(models.RelationFieldRefs).(*column.ColumnVarChar)

	rels := make([]graphrag.Relation, 0, n)
	for i := 0; i < n; i++ {
		var r graphrag.Relation
		if srcCol != nil {
			if v, err := srcCol.Value(i); err == nil {
				r.Source = v
			}
		}
		if tgtCol != nil {
			if v, err := tgtCol.Value(i); err == nil {
				r.Target = v
			}
		}
		if relCol != nil {
			if v, err := relCol.Value(i); err == nil {
				r.Relation = v
			}
		}
		if descCol != nil {
			if v, err := descCol.Value(i); err == nil {
				r.Description = v
			}
		}
		if wCol != nil {
			if v, err := wCol.Value(i); err == nil {
				r.Weight = v
			}
		}
		if refsCol != nil {
			if v, err := refsCol.Value(i); err == nil {
				r.ChunkRefs = parseRefs(v)
			}
		}
		if r.Source != "" && r.Target != "" {
			rels = append(rels, r)
		}
	}
	return rels
}
