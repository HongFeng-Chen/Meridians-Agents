// Package graphrag 提供基于知识图谱的 RAG 业务线。
//
// 与主业务线（纯向量混合检索）并行：
//
//	Ingest: 分块 -> LLM 抽取实体/关系 -> 合并去重 -> 向量化入 Milvus 图集合
//	Query : 实体混合检索(BM25+Dense) -> 沿关系边多跳扩展 -> 子图+原文证据 -> LLM 生成
//
// 本包只依赖接口，向量库/模型的具体实现在 data 与 biz 其他子包中。
package graphrag

import "context"

// Entity 实体节点
type Entity struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Freq        int      `json:"freq"`
	ChunkRefs   []string `json:"chunk_refs"` // 形如 "doc_id#chunk_idx"
}

// Relation 有向关系边
type Relation struct {
	Source      string   `json:"source"`
	Target      string   `json:"target"`
	Relation    string   `json:"relation"`
	Description string   `json:"description"`
	Weight      float64  `json:"weight"`
	ChunkRefs   []string `json:"chunk_refs"`
}

// Extraction 单块抽取结果
type Extraction struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

// GraphAnswer 图谱问答结果
type GraphAnswer struct {
	Question  string     `json:"question"`
	Answer    string     `json:"answer"`
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
	Evidence  []string   `json:"evidence"` // 引用的原文片段
	Cached    bool       `json:"cached"`
}

// Store 图集合存储（Milvus 实现位于 internal/data/milvusdb/repo）
type Store interface {
	EnsureCollections(ctx context.Context) error
	InsertEntities(ctx context.Context, ents []Entity, contents []string, vectors [][]float32) error
	InsertRelations(ctx context.Context, rels []Relation, vectors [][]float32) error
	ExistingEntities(ctx context.Context, names []string) (map[string]Entity, error)
	SearchEntities(ctx context.Context, question string, qvec []float32, topK int) ([]Entity, error)
	RelationsOf(ctx context.Context, names []string, limit int) ([]Relation, error)
	EntitiesByNames(ctx context.Context, names []string) ([]Entity, error)
	Counts(ctx context.Context) (entities int64, relations int64, err error)
	// DeleteOrphanNodes 删除仅被该文档引用的节点与边（prefix 形如 "doc_id#"）
	DeleteOrphanNodes(ctx context.Context, prefix string) (entities int, relations int, err error)
}

// Embedder 文本向量化
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	EmbedOne(ctx context.Context, text string) ([]float32, error)
	Dim() int
}

// Chatter 大模型对话
type Chatter interface {
	Chat(ctx context.Context, system, user string) (string, error)
	ChatTemp(ctx context.Context, system, user string, temperature float64) (string, error)
}

// Prompter 提示词模板渲染（实现位于 biz/prompt）。
// 抽取与问答的提示词都走模板管理，改词不需要改代码、不需要重启。
type Prompter interface {
	Render(ctx context.Context, key string, vars map[string]string) (string, error)
}

// Splitter 文本分块器（由 rag 包注入，避免循环依赖）
type Splitter interface {
	Chunk(text string) []string
}

// Evidence 原文证据来源（由 rag 用例注入，按 chunk 引用取回原文）
type Evidence interface {
	Texts(ctx context.Context, docID string, chunkIdxs []int64) ([]string, error)
}

// Options 图谱业务参数
type Options struct {
	TopK         int // 实体检索数量
	Hops         int // 关系扩展跳数
	MaxEdges     int // 每跳最多取回边数
	MaxEntities  int // 单块最多抽取实体数
	MaxRelations int // 单块最多抽取关系数
}

func normName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out = append(out, c)
	}
	return string(out)
}

func mergeRefs(a, b []string) []string {
	seen := make(map[string]struct{}, len(a))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	out := a
	for _, s := range b {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}
