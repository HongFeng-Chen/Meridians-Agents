package graphrag

import "testing"

// TestScopeByDocsGraph 图谱裁剪：只保留引用本人文档的实体与关系，无 refs 的保守丢弃
func TestScopeByDocsGraph(t *testing.T) {
	g := newGraph()
	g.addEntity(Entity{Name: "harness", ChunkRefs: []string{"a-doc#0"}})
	g.addEntity(Entity{Name: "milvus", ChunkRefs: []string{"b-doc#1"}})
	g.addEntity(Entity{Name: "ghost"}) // 无 refs：保守丢弃
	rels := []Relation{
		// 目标实体（milvus）只属于 b-doc → 关系一并丢弃，否则关系描述会泄漏他人实体
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
	if len(keptRels) != 0 {
		t.Fatalf("两端不都在白名单内的关系应被丢弃，实际 %+v", keptRels)
	}

	// 空白名单（用户无文档）：全部裁掉
	empty, noRels := scopeByDocs(g, rels, nil)
	if len(empty.entities) != 0 || len(noRels) != 0 {
		t.Fatalf("空白名单应裁掉全部内容")
	}
}

// TestScopeByDocsPrunesForeignRefs 跨用户引用裁剪（SEC-008）：
// 实体因自有文档命中而被保留时，ChunkRefs 里别人的引用必须一起删掉——
// 否则证据解析会按 doc_id 去读别人的原文，泄漏的是文档内容，不是计数。
func TestScopeByDocsPrunesForeignRefs(t *testing.T) {
	g := newGraph()
	g.addEntity(Entity{
		Name:      "milvus",
		ChunkRefs: []string{"a-doc#0", "b-doc#1", "b-doc#2"},
	})
	g.addEntity(Entity{
		Name:      "harness",
		ChunkRefs: []string{"a-doc#3"},
	})
	rels := []Relation{
		{Source: "harness", Target: "milvus", ChunkRefs: []string{"a-doc#3", "b-doc#1"}},
	}

	scoped, keptRels := scopeByDocs(g, rels, []string{"a-doc"})

	// Bob（b-doc 属主）视角：本用例模拟的是「a-doc 用户查询」，b-doc 引用必须消失
	milvus := scoped.entities["milvus"]
	if milvus == nil {
		t.Fatal("milvus 因 a-doc 命中应保留")
	}
	if len(milvus.ChunkRefs) != 1 || milvus.ChunkRefs[0] != "a-doc#0" {
		t.Fatalf("实体引用未裁剪，仍含他人文档: %v", milvus.ChunkRefs)
	}
	if len(keptRels) != 1 {
		t.Fatalf("关系应保留 1 条，实际 %d", len(keptRels))
	}
	for _, ref := range keptRels[0].ChunkRefs {
		if ref != "a-doc#3" {
			t.Fatalf("关系引用未裁剪，仍含他人文档: %v", keptRels[0].ChunkRefs)
		}
	}

	// 原图不能被就地修改（裁剪结果是新对象，避免污染同一次请求里的其他视图）
	orig := g.entities["milvus"]
	if len(orig.ChunkRefs) != 3 {
		t.Fatalf("裁剪不应修改原图，实际 %v", orig.ChunkRefs)
	}
}

// TestScopeByDocsKeepsSharedEntity 双方都引用的实体：各自只看到自己的引用
func TestScopeByDocsKeepsSharedEntity(t *testing.T) {
	g := newGraph()
	g.addEntity(Entity{Name: "shared", ChunkRefs: []string{"a-doc#0", "b-doc#1"}})

	alice, _ := scopeByDocs(g, nil, []string{"a-doc"})
	bob, _ := scopeByDocs(g, nil, []string{"b-doc"})

	if got := alice.entities["shared"].ChunkRefs; len(got) != 1 || got[0] != "a-doc#0" {
		t.Fatalf("alice 视角引用错误: %v", got)
	}
	if got := bob.entities["shared"].ChunkRefs; len(got) != 1 || got[0] != "b-doc#1" {
		t.Fatalf("bob 视角引用错误: %v", got)
	}
}
