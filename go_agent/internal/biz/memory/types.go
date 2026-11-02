// Package memory 记忆系统：
//
//	短期记忆：会话消息存 Redis（List + TTL，写入续期），撑起多轮对话上下文
//	长期记忆：MySQL 权威存储（过期时间、checksum 去重）+ Milvus 向量召回
//	          + GraphRAG 图谱（实体/关系抽取，复用 RAG 的图集合，跨记忆共享节点）
//
// 本包只依赖接口，存储与模型的实现在 data 与 biz 其他子包中。
package memory

import (
	"errors"
	"time"

	"go_agent/internal/biz/rag/graphrag"
)

// 领域错误
var (
	ErrNotFound = errors.New("memory: 记忆不存在")
)

// 记忆种类
const (
	KindFact       = "fact"       // 事实
	KindPreference = "preference" // 偏好
	KindEvent      = "event"      // 事件
	KindInsight    = "insight"    // 洞察/结论
	KindTask       = "task"       // 待办/承诺
	KindProfile    = "profile"    // 用户画像（百炼式：身份/职业/目标/偏好结构画像）
	KindSkill      = "skill"      // 技能记忆（用户教授的方法/流程/操作规程）
)

// Message 短期记忆中的一条会话消息
type Message struct {
	Role    string    `json:"role"` // user | assistant | system | tool
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

// LongTermMemory 长期记忆（MySQL 权威记录）
type LongTermMemory struct {
	ID         string   `json:"id"`
	UserID     string   `json:"user_id"`
	Kind       string   `json:"kind"`
	Content    string   `json:"content"`
	Importance int      `json:"importance"` // 1~5，数值越高越重要
	Tags       []string `json:"tags"`
	Source     string   `json:"source"` // 来源描述（会话、文档、人工输入…）
	RuleName   string   `json:"rule_name,omitempty"`
	Entities   int      `json:"entities"`
	Relations  int      `json:"relations"`
	// 召回统计（每次召回命中后回填）
	RecallCount    int        `json:"recall_count"`
	LastRecalledAt *time.Time `json:"last_recalled_at,omitempty"`
	Checksum       string     `json:"-"`                    // 内容指纹（同用户同内容去重）
	ExpiresAt      *time.Time `json:"expires_at,omitempty"` // nil = 永不过期
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	// 运行时附加字段（不入库）
	Deduped bool `json:"deduped,omitempty"` // 内容未变，复用已有记忆
	Expired bool `json:"expired,omitempty"` // 已过失效时间
}

// Expired 判断是否已过失效时间
func (m *LongTermMemory) ExpiredAt(now time.Time) bool {
	return m.ExpiresAt != nil && now.After(*m.ExpiresAt)
}

// RecalledMemory 召回结果：记忆原文 + 相关度得分
type RecalledMemory struct {
	*LongTermMemory
	Score float32 `json:"score"`
}

// GraphContext 图谱召回上下文（与 RAG 的图集合共享节点，能跨记忆/文档推理）
type GraphContext struct {
	Entities  []graphrag.Entity   `json:"entities"`
	Relations []graphrag.Relation `json:"relations"`
}

// RecallResult 召回结果：向量召回的记忆 + 图谱召回的子图
type RecallResult struct {
	Query    string           `json:"query"`
	UserID   string           `json:"user_id"`
	Memories []RecalledMemory `json:"memories"`
	Graph    *GraphContext    `json:"graph,omitempty"`
}

// RememberInput 写入长期记忆的入参
type RememberInput struct {
	ID         string        `json:"id"`      // 留空自动生成
	UserID     string        `json:"user_id"` // 留空为 default
	Kind       string        `json:"kind"`    // 留空为 fact
	Content    string        `json:"content"`
	Importance int           `json:"importance"`
	Tags       []string      `json:"tags"`
	Source     string        `json:"source"`
	RuleName   string        `json:"rule_name"`  // 提取来源规则名（空 = 手动写入）
	ExpiresAt  *time.Time    `json:"expires_at"` // nil = 永不过期
	TTL        time.Duration `json:"-"`          // 与 ExpiresAt 二选一：>0 时按 now+TTL 计算
	Graph      *bool         `json:"graph"`      // 留空用配置默认值
}

// RememberResult 写入结果
type RememberResult struct {
	Memory       *LongTermMemory `json:"memory"`
	Deduped      bool            `json:"deduped"`
	Entities     int             `json:"entities"`
	Relations    int             `json:"relations"`
	GraphWarning string          `json:"graph_warning,omitempty"`
}

// MemoryAnswer 基于记忆的回答（短期历史 + 长期召回 + LLM）
type MemoryAnswer struct {
	SessionID string        `json:"session_id"`
	Question  string        `json:"question"`
	Answer    string        `json:"answer"`
	Recall    *RecallResult `json:"recall,omitempty"`
}

// ---------- 记忆规则（参考百炼：事实 / 用户画像 / 技能 + 自定义） ----------

// 规则类型
const (
	RuleTypeFact    = "fact"    // 事实记忆规则（内置）
	RuleTypeProfile = "profile" // 用户画像规则（内置）
	RuleTypeSkill   = "skill"   // 技能记忆规则（内置）
	RuleTypeCustom  = "custom"  // 自定义规则
)

// MemoryRule 记忆提取规则
type MemoryRule struct {
	ID          uint64    `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"` // fact|profile|skill|custom
	Kind        string    `json:"kind"` // 提取出的记忆类型
	Instruction string    `json:"instruction"`
	Builtin     bool      `json:"builtin"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ExtractedItem LLM 提取出的单条候选记忆
type ExtractedItem struct {
	Content    string   `json:"content"`
	Importance int      `json:"importance"`
	Tags       []string `json:"tags"`
}

// ExtractedMemory 提取并写入结果
type ExtractedMemory struct {
	Content    string   `json:"content"`
	Kind       string   `json:"kind"`
	Importance int      `json:"importance"`
	Tags       []string `json:"tags"`
	Deduped    bool     `json:"deduped"`
	MemoryID   string   `json:"memory_id"`
	RuleID     uint64   `json:"rule_id"`
	RuleName   string   `json:"rule_name"`
	Error      string   `json:"error,omitempty"`
}

// RuleExtractResult 单条规则的提取结果
type RuleExtractResult struct {
	RuleID   uint64            `json:"rule_id"`
	RuleName string            `json:"rule_name"`
	Error    string            `json:"error,omitempty"`
	Memories []ExtractedMemory `json:"memories"`
}

// ExtractResult 一次规则提取的总结果
type ExtractResult struct {
	SessionID string              `json:"session_id"`
	Results   []RuleExtractResult `json:"results"`
	Written   int                 `json:"written"`
	Deduped   int                 `json:"deduped"`
}

// RecallLog 记忆检索历史（一次召回的流水）
type RecallLog struct {
	ID        uint64    `json:"id"`
	UserID    string    `json:"user_id"`
	Query     string    `json:"query"`
	Source    string    `json:"source"` // chat|api|answer
	TopK      int       `json:"top_k"`
	Hits      int       `json:"hits"`
	MemoryIDs []string  `json:"memory_ids"`
	LatencyMS int64     `json:"latency_ms"`
	CreatedAt time.Time `json:"created_at"`
}

// 召回来源
const (
	RecallSourceChat   = "chat"
	RecallSourceAPI    = "api"
	RecallSourceAnswer = "answer"
)
