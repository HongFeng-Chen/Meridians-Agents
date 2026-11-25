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
	// FilteredOut 被分数线滤掉的命中数（RRF 绝对线 + 预审 + rerank 下限累计）：
	// 让前端能解释「为什么引用变少了」——低分片段不进 prompt 也不进引用。
	FilteredOut int `json:"filtered_out"`
	// NoContext 候选全被过滤或判定范围外：Answer 固定为「知识库暂无相关资料」，
	// 前端据此展示空态而不是把兜底话术当成正常回答。
	NoContext bool `json:"no_context"`
}

// Document 文档元数据（MySQL 落库，作为文档管理的权威数据源）
type Document struct {
	DocID string `json:"doc_id"`
	// UserID 归属用户（隔离键）：检索只返回本人文档，列表亦按此过滤
	UserID string `json:"user_id"`
	// TenantID 归属租户：租户之间互不可见（即使 user_id 相同）
	TenantID string `json:"tenant_id"`
	// Scope 知识作用域：system（全平台）/ tenant（同租户）/ user（仅本人）。
	// 可见性由它与人、租户共同决定，见 scope.go 的 Visible。
	Scope string `json:"scope"`
	// SpaceID 所属知识空间（V2）。迁移期与 Scope 双写：
	// system → platform、tenant → tenant:<租户>、user → personal:<租户>:<用户>。
	// 新数据按空间授权检索；空值表示尚未迁移，仍按三层作用域判定。
	SpaceID   string    `json:"space_id"`
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

// DocumentTextStore 可选能力：文档原文持久化（MySQL 实现）。
//
// 以窄接口挂在 DocumentRepo 实现对象上（类型断言使用），不进 DocumentRepo
// 主接口——避免 fake / 内存实现被迫跟着实现原文存取。
// 用途：仅图谱文档「同步索引」补建向量时需要一份原文重放分块；
// 向量文档可从 Milvus 分块重建文本，不依赖这里。
type DocumentTextStore interface {
	SaveText(ctx context.Context, docID, text string) error
	GetText(ctx context.Context, docID string) (string, error)
}

// DocumentRepo 文档元数据仓库（MySQL 实现）
type DocumentRepo interface {
	Upsert(ctx context.Context, doc *Document) error
	Get(ctx context.Context, docID string) (*Document, error)
	// List 文档列表：按「系统 ∪ 本租户共享 ∪ 本人私有」可见性过滤，
	// userID 为空表示不限用户（管理视角），scope 非空时再按作用域收窄。
	List(ctx context.Context, tenantID, userID, scope string, limit int) ([]Document, error)
	// ListDocIDs 当前身份可见的全部文档 ID（检索时拼 Milvus 白名单过滤用，
	// 同时决定 GraphRAG 的召回边界：图谱节点靠 chunk_refs 里的 doc_id 归属）
	ListDocIDs(ctx context.Context, tenantID, userID string) ([]string, error)
	// ListDocIDsBySpaces V2 空间授权白名单：space_id 命中允许空间，
	// 或历史数据（space_id 为空）回落到三层作用域。
	ListDocIDsBySpaces(ctx context.Context, tenantID, userID string, spaceIDs []string) ([]string, error)
	// ListBySpace 按空间列出文档（知识空间树里按节点查看文档清单）。
	// 只做精确匹配：子空间文档在树上逐节点查看，调用方需要子树时自己拼 ID 集合。
	ListBySpace(ctx context.Context, spaceID string, limit int) ([]Document, error)
	// UpdateSpaceID 移动文档到目标空间（知识空间整理用）。
	UpdateSpaceID(ctx context.Context, docID, spaceID string) error
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
