package graphrag

import "testing"

// TestScopeByDocsGraph 图谱裁剪：只保留引用本人文档的实体与关系，无 refs 的保守丢弃
func TestScopeByDocsGraph(t *testing.T) {
	g := newGraph()
	g.addEntity(Entity{Name: "harness", ChunkRefs: []string{"a-doc#0"}})
	g.addEntity(Entity{Name: "milvus", ChunkRefs: []string{"b-doc#1"}})
	g.addEntity(Entity{Name: "ghost"}) // 无 refs：保守丢弃
	rels := []Relation{
		{Source: "harness", Target: "milvus", ChunkRefs: []string{"a-doc#0"}},
		{Source: "milvus", Target: "ghost", ChunkRefs: []string{"b-doc#1"}},
	}

	scoped, keptRels := scopeByDocs(g, rels, []string{"a-doc"})
	if len(scoped.entities) != 1 {
		t.Fatalf("应只保留 1 个实体，实际 %d", len(scoped.entities))
	}
	if _, ok := scoped.entities["harness"]; !ok {
		t.Fatalf("harness 实体应保留: %+v", scoped.entities)
	}
	if len(keptRels) != 1 || keptRels[0].Source != "harness" {
		t.Fatalf("关系裁剪不符: %+v", keptRels)
	}

	// 空白名单（用户无文档）：全部裁掉
	empty, noRels := scopeByDocs(g, rels, nil)
	if len(empty.entities) != 0 || len(noRels) != 0 {
		t.Fatalf("空白名单应裁掉全部内容")
	}
}
