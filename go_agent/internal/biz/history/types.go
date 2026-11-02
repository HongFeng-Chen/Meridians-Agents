package history

// 统一操作历史：MCP 工具调用、技能使用、记忆召回、RAG 召回、GraphRAG 召回
// 五类「用到了什么」的记录，统一落 op_histories 表，并尽量关联到 AgentFactory
// 的任务（task_id / agent_key 由调用方通过 WithTask 塞进 context 自动取出）。
//
// 设计原则（与 llm_calls 调用历史一致）：
//   - 写路径异步、失败只告警，绝不阻断业务主流程；
//   - 任务上下文用 context 传递，业务代码零侵入。

import "time"

// 五类历史（kind 字段取值）
const (
	KindMCPCall      = "mcp_call"      // MCP 工具调用
	KindSkillUse     = "skill_use"     // 技能使用
	KindMemoryRecall = "memory_recall" // 记忆召回
	KindRagRecall    = "rag_recall"    // RAG 文档召回
	KindGraphRecall  = "graph_recall"  // GraphRAG 图谱召回
)

// KindLabels kind → 中文标签（前端展示用，proto 透传）
var KindLabels = map[string]string{
	KindMCPCall:      "MCP 调用",
	KindSkillUse:     "技能使用",
	KindMemoryRecall: "记忆召回",
	KindRagRecall:    "RAG 召回",
	KindGraphRecall:  "GraphRAG 召回",
}

// ValidKinds 全部合法 kind
var ValidKinds = []string{KindMCPCall, KindSkillUse, KindMemoryRecall, KindRagRecall, KindGraphRecall}

// Record 一条操作历史
type Record struct {
	ID        uint64    `json:"id"`
	Kind      string    `json:"kind"`       // 五类之一
	TaskID    uint64    `json:"task_id"`    // 关联任务（0 = 手动/控制台调用）
	AgentKey  string    `json:"agent_key"`  // 执行 Agent（任务内调用才有）
	ConvID    string    `json:"conv_id"`    // 来源会话（会话内调用自动带上；空 = 会话外）
	Ref       string    `json:"ref"`        // 操作对象：工具唯一名 / 技能名 / 召回源标识
	Query     string    `json:"query"`      // 输入摘要：查询词或参数 JSON（截断）
	Summary   string    `json:"summary"`    // 结果摘要：命中数 / 结果片段（截断）
	Detail    string    `json:"detail"`     // 结构化明细 JSON：召回类存逐条命中与得分
	OK        bool      `json:"ok"`         // 是否成功
	LatencyMS int64     `json:"latency_ms"` // 耗时毫秒
	CreatedAt time.Time `json:"created_at"`
}

// Filter 查询条件
type Filter struct {
	Kind   string // 五类之一，空 = 全部
	TaskID uint64 // 0 = 不过滤
	ConvID string // 来源会话精确匹配，空 = 不过滤
	Ref    string // 操作对象精确匹配，空 = 不过滤
	OK     *bool  // nil = 不过滤
	Limit  int
	Offset int
}

// KindStat 按 kind 聚合的数量
type KindStat struct {
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

// Stats 全局统计：总量 + 按 kind 聚合
type Stats struct {
	Total  int64      `json:"total"`
	ByKind []KindStat `json:"by_kind"`
}

// ValidateKind 是否合法 kind
func ValidateKind(k string) bool {
	if k == "" {
		return true
	}
	for _, v := range ValidKinds {
		if v == k {
			return true
		}
	}
	return false
}
