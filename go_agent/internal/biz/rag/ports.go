package rag

import (
	"context"
	"time"
)

// Hit 检索命中结果
type Hit struct {
	ID       int64   `json:"id"`
	DocID    string  `json:"doc_id"`
	ChunkIdx int64   `json:"chunk_idx"`
	Source   string  `json:"source"`
	Text     string  `json:"text"`
	Score    float32 `json:"score"`
}

// Answer RAG 问答结果
type Answer struct {
	Question  string `json:"question"`
	Answer    string `json:"answer"`
	Citations []Hit  `json:"citations"`
	Cached    bool   `json:"cached"`
}

// Document 文档元数据（MySQL 落库，作为文档管理的权威数据源）
type Document struct {
	DocID string `json:"doc_id"`
	// UserID 归属用户（隔离键）：检索只返回本人文档，列表亦按此过滤
	UserID    string    `json:"user_id"`
	Source    string    `json:"source"`
	Mode      string    `json:"mode"` // rag | graph
	Status    string    `json:"status"`
	Chars     int       `json:"chars"`
	Chunks    int       `json:"chunks"`
	Entities  int       `json:"entities"`
	Relations int       `json:"relations"`
	Checksum  string    `json:"checksum"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Stats 系统概览
type Stats struct {
	Chunks    int64 `json:"chunks"`
	Docs      int64 `json:"docs"`
	Entities  int64 `json:"entities"`
	Relations int64 `json:"relations"`
}

// Embedder 文本向量化（实现位于 biz/embedder）
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	EmbedOne(ctx context.Context, text string) ([]float32, error)
	Dim() int
	Model() string
}

// Chatter 大模型对话（实现位于 biz/llm）
type Chatter interface {
	Chat(ctx context.Context, system, user string) (string, error)
	ChatTemp(ctx context.Context, system, user string, temperature float64) (string, error)
	Model() string
}

// Prompter 提示词模板渲染（实现位于 biz/prompt）。
// 提示词由管理端维护并版本化，RAG 只按 key 取当前生效版本。
type Prompter interface {
	Render(ctx context.Context, key string, vars map[string]string) (string, error)
}

// ChunkStore 分块向量库（Milvus 实现，BM25 + Dense 混合检索）
type ChunkStore interface {
	EnsureCollection(ctx context.Context) error
	Insert(ctx context.Context, docID, source string, chunks []string, vectors [][]float32) error
	HybridSearch(ctx context.Context, queryText string, queryVec []float32, topK int, filter string) ([]Hit, error)
	GetTexts(ctx context.Context, filter string, limit int) ([]Hit, error)
	Count(ctx context.Context) (int64, error)
	DeleteDoc(ctx context.Context, docID string) (int64, error)
	// IDsOfDoc 该文档当前全部分块主键（更新时先记下旧版本，写完新版本再删）
	IDsOfDoc(ctx context.Context, docID string) ([]int64, error)
	// DeleteByIDs 按主键删除分块（旧版本清理；只删自己记下的那些 ID，
	// 不会误伤同一文档刚写入的新分块）
	DeleteByIDs(ctx context.Context, ids []int64) (int64, error)
}

// DocumentRepo 文档元数据仓库（MySQL 实现）
type DocumentRepo interface {
	Upsert(ctx context.Context, doc *Document) error
	Get(ctx context.Context, docID string) (*Document, error)
	// List 文档列表；userID 非空时只返回该用户的文档
	List(ctx context.Context, userID string, limit int) ([]Document, error)
	// ListDocIDs 该用户全部文档 ID（检索时拼 Milvus 白名单过滤用）
	ListDocIDs(ctx context.Context, userID string) ([]string, error)
	Delete(ctx context.Context, docID string) error
	Count(ctx context.Context) (int64, error)
}

// AnswerCache 问答缓存（Redis 实现）
type AnswerCache interface {
	GetAnswer(ctx context.Context, key string) (*Answer, bool)
	PutAnswer(ctx context.Context, key string, ans *Answer, ttl time.Duration)
	// Invalidate 清理全部问答缓存（文档增删后调用）
	Invalidate(ctx context.Context) error
}

// Locker 分布式锁（Redis 实现）：防止同一文档被并发重复入库
type Locker interface {
	TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error)
	Unlock(ctx context.Context, key string) error
}
