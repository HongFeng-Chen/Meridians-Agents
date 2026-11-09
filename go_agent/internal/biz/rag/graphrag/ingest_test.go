package graphrag

// P1-07：重新入库时必须先摘掉本文档的旧引用，不能把旧版本引用合并回新节点。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// —— 测试替身 ——

type fakeStore struct {
	detachPrefix string
	order        []string // "detach" / "insert"
	existing     map[string]Entity
	insertedE    []Entity
	insertedR    []Relation
}

func (f *fakeStore) EnsureCollections(context.Context) error { return nil }

func (f *fakeStore) InsertEntities(_ context.Context, ents []Entity, _ []string, _ [][]float32) error {
	f.order = append(f.order, "insert")
	f.insertedE = append(f.insertedE, ents...)
	return nil
}

func (f *fakeStore) InsertRelations(_ context.Context, rels []Relation, _ [][]float32) error {
	f.insertedR = append(f.insertedR, rels...)
	return nil
}

func (f *fakeStore) ExistingEntities(context.Context, []string) (map[string]Entity, error) {
	return f.existing, nil
}

func (f *fakeStore) SearchEntities(context.Context, string, []float32, int) ([]Entity, error) {
	return nil, nil
}

func (f *fakeStore) RelationsOf(context.Context, []string, int) ([]Relation, error) { return nil, nil }

func (f *fakeStore) EntitiesByNames(context.Context, []string) ([]Entity, error) { return nil, nil }

func (f *fakeStore) Counts(context.Context) (int64, int64, error) { return 0, 0, nil }

func (f *fakeStore) DetachDocRefs(_ context.Context, prefix string) (int, int, error) {
	f.detachPrefix = prefix
	f.order = append(f.order, "detach")
	return 0, 0, nil
}

type fakeEmbed struct{}

func (fakeEmbed) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 8)
	}
	return out, nil
}
func (fakeEmbed) EmbedOne(context.Context, string) ([]float32, error) { return make([]float32, 8), nil }
func (fakeEmbed) Dim() int                                            { return 8 }

type fakeChat struct{}

func (fakeChat) Chat(context.Context, string, string) (string, error) { return "", nil }
func (fakeChat) ChatTemp(_ context.Context, _, _ string, _ float64) (string, error) {
	ex := Extraction{
		Entities:  []Entity{{Name: "Milvus", Type: "技术", Description: "向量库"}},
		Relations: []Relation{{Source: "Milvus", Target: "Milvus", Relation: "自指"}},
	}
	b, _ := json.Marshal(ex)
	return string(b), nil
}

type fakeSplit struct{}

func (fakeSplit) Chunk(text string) []string { return []string{text} }

// TestIngestDetachesOldRefsBeforeInsert 旧版本引用既不能残留，也不能被合并回新节点。
func TestIngestDetachesOldRefsBeforeInsert(t *testing.T) {
	store := &fakeStore{existing: map[string]Entity{
		"milvus": {
			Name:      "Milvus",
			Freq:      3,
			ChunkRefs: []string{"d#0", "d#1", "other#5"}, // d#0/d#1 是本文档旧版本
		},
	}}
	svc, err := New(context.Background(), Deps{
		Store: store, Embedder: fakeEmbed{}, LLM: fakeChat{},
		Evidence: nil, Splitter: fakeSplit{},
		Options: Options{MaxEntities: 5, MaxRelations: 5},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := svc.Ingest(context.Background(), "d", "d.txt", "Milvus 是向量数据库"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if store.detachPrefix != "d#" {
		t.Fatalf("应按文档前缀摘引用，实际 %q", store.detachPrefix)
	}
	// 必须先摘旧引用再写新节点：反过来的话新节点会被自己的 detach 清掉
	if len(store.order) < 2 || store.order[0] != "detach" {
		t.Fatalf("detach 应发生在写入之前，实际顺序 %v", store.order)
	}

	var got *Entity
	for i := range store.insertedE {
		if normName(store.insertedE[i].Name) == "milvus" {
			got = &store.insertedE[i]
		}
	}
	if got == nil {
		t.Fatalf("没有写入 Milvus 实体: %+v", store.insertedE)
	}
	for _, ref := range got.ChunkRefs {
		if ref == "d#1" {
			t.Fatalf("旧版本引用 d#1 未被摘掉: %v", got.ChunkRefs)
		}
	}
	hasOwn, hasOther := false, false
	for _, ref := range got.ChunkRefs {
		if ref == "d#0" {
			hasOwn = true
		}
		if ref == "other#5" {
			hasOther = true
		}
	}
	if !hasOwn {
		t.Fatalf("新版本引用应保留: %v", got.ChunkRefs)
	}
	if !hasOther {
		t.Fatalf("其他文档的引用不应被误删: %v", got.ChunkRefs)
	}
	if strings.Count(strings.Join(got.ChunkRefs, ","), "d#0") != 1 {
		t.Fatalf("本文档引用不应重复出现: %v", got.ChunkRefs)
	}
	// freq = 本次 1 + 其他来源 1（旧版本的两条不计入，否则规模会被虚增）
	if got.Freq != 2 {
		t.Fatalf("freq = %d, want 2（只累计非本文档的既有引用）", got.Freq)
	}
}
