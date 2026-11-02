package memory

import (
	"context"
	"time"

	"go_agent/internal/biz/rag/graphrag"
)

// LongTermRepo 长期记忆仓库（MySQL 实现）：权威数据源，负责过期时间与去重判定
type LongTermRepo interface {
	Save(ctx context.Context, m *LongTermMemory) error
	Get(ctx context.Context, id string) (*LongTermMemory, error)
	// GetByIDs 批量取回（保持传入顺序，缺失的 id 不返回）
	GetByIDs(ctx context.Context, ids []string) ([]*LongTermMemory, error)
	// FindByChecksum 按 (user_id, checksum) 查找，用于内容级去重
	FindByChecksum(ctx context.Context, userID, checksum string) (*LongTermMemory, error)
	// List 记忆列表；userID 为空表示全部；includeExpired 控制是否带出已过期记录
	List(ctx context.Context, userID string, includeExpired bool, limit int) ([]*LongTermMemory, error)
	// ListExpired 已过失效时间的记录（清理任务用）
	ListExpired(ctx context.Context, limit int) ([]*LongTermMemory, error)
	Delete(ctx context.Context, id string) error
	// Count 总数与已过期数
	Count(ctx context.Context) (total int64, expired int64, err error)

	// ---------- 记忆规则 ----------

	// ListRules 规则列表（按 id 正序，内置在前）
	ListRules(ctx context.Context) ([]*MemoryRule, error)
	// GetRule 单条规则
	GetRule(ctx context.Context, id uint64) (*MemoryRule, error)
	// SaveRule 新建（ID=0）或更新规则
	SaveRule(ctx context.Context, r *MemoryRule) error
	// DeleteRule 删除规则
	DeleteRule(ctx context.Context, id uint64) error
	// CountBuiltinRules 内置规则数（种子判定：为 0 时写入内置三条）
	CountBuiltinRules(ctx context.Context) (int64, error)

	// ---------- 检索历史 ----------

	// SaveRecallLog 追加一条召回流水
	SaveRecallLog(ctx context.Context, l *RecallLog) error
	// ListRecallLogs 检索历史（时间倒序）；userID 为空表示全部
	ListRecallLogs(ctx context.Context, userID string, limit int) ([]*RecallLog, error)
	// MarkRecalled 召回命中后回填统计（recall_count+1、last_recalled_at）
	MarkRecalled(ctx context.Context, memoryIDs []string, at time.Time) error
}

// ShortTermStore 短期会话记忆（Redis 实现：List + TTL，写入续期）
type ShortTermStore interface {
	Append(ctx context.Context, sessionID string, msg Message, ttl time.Duration) error
	// History 最近 limit 条消息（按时间正序返回）
	History(ctx context.Context, sessionID string, limit int) ([]Message, error)
	Len(ctx context.Context, sessionID string) (int64, error)
	Clear(ctx context.Context, sessionID string) error
}

// VectorStore 长期记忆向量库（Milvus 实现：BM25 + Dense 混合召回）
type VectorStore interface {
	EnsureCollection(ctx context.Context) error
	Insert(ctx context.Context, m *LongTermMemory, vec []float32) error
	// Search 混合召回；userID 非空时按用户过滤
	Search(ctx context.Context, query string, qvec []float32, topK int, userID string) ([]RecalledMemory, error)
	DeleteByMemory(ctx context.Context, memoryID string) (int64, error)
	Count(ctx context.Context) (int64, error)
}

// GraphStore 记忆图谱存储：复用 GraphRAG 的实体/关系集合，
// 记忆与文档共享同一张知识图谱（跨记忆、跨文档推理）。
// 实现位于 internal/data/milvusdb/repo（与 RAG 共用 GraphStore）。
type GraphStore = graphrag.Store
