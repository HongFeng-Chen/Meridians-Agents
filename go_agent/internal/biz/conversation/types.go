package conversation

// 会话域：用户与平台的一次多轮对话目录。
//
// 设计要点：
//   - 会话 ID 优先复用 AgentFactory 任务 ID（会话 id = task id），
//     任务 trace 链路与会话直接互通；普通检索 / 工具会话自动生成 ID。
//   - 会话内允许调用 Agent、MCP 工具，以及 RAG / GraphRAG / Memory 检索召回；
//     每轮交互落一条消息，meta JSON 记录本轮能力调用链路，
//     各能力自身仍会写统一操作历史（op_histories）。

import "time"

// 消息角色
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// ValidRoles 全部合法角色
var ValidRoles = []string{RoleUser, RoleAssistant}

// Conversation 会话
type Conversation struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	AgentKey     string    `json:"agent_key"` // 默认会话 Agent（可空）
	MessageCount int64     `json:"message_count"`
	LastMessage  string    `json:"last_message"` // 最新消息预览（截断）
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	HasDebate    bool      `json:"has_debate"` // 会话内发生过辩论（目录分组用）
}

// Message 会话内一条消息
type Message struct {
	ID             int64     `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"`
	Speaker        string    `json:"speaker"` // 发言者：普通会话为空；辩论为角色名 / 主持人
	Content        string    `json:"content"`
	Meta           string    `json:"meta"`    // 能力调用链路 JSON
	TaskID         uint64    `json:"task_id"` // 关联任务（0 = 无）
	CreatedAt      time.Time `json:"created_at"`
}

// —— 辩论会话 ——

// 辩论状态
const (
	DebateRunning  = "running"  // 进行中
	DebatePaused   = "paused"   // 用户暂停：当前发言结束后挂起，可恢复
	DebateStopping = "stopping" // 用户已请求停止，等当前发言结束后收尾
	DebateFinished = "finished" // 正常结束（轮次满 / 共识收敛），已汇总
	DebateStopped  = "stopped"  // 被用户叫停，已汇总
)

// 结束判定规则
const (
	EndRuleRounds    = "rounds"    // 跑满轮次上限
	EndRuleConsensus = "consensus" // 每轮后由 LLM 判定是否已充分交锋/收敛，可提前结束
)

// DebateRole 辩论角色（临时自定义，不属于 Agent 定义库）
type DebateRole struct {
	Name    string `json:"name"`    // 角色名（会话内唯一）
	Persona string `json:"persona"` // 人设（背景、专业领域、性格）
	Stance  string `json:"stance"`  // 立场（支持/反对/中立评审/某方案倾向）
	Style   string `json:"style"`   // 发言风格（犀利/严谨/口语化…）
}

// Debate 一场辩论
type Debate struct {
	ID             int64        `json:"id"`
	ConversationID string       `json:"conversation_id"`
	Topic          string       `json:"topic"`
	Status         string       `json:"status"`
	Round          int          `json:"round"`
	MaxRounds      int          `json:"max_rounds"`
	EndRule        string       `json:"end_rule"`
	Roles          []DebateRole `json:"roles"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	FinishedAt     *time.Time   `json:"finished_at,omitempty"`
}

// Filter 会话目录查询条件
type Filter struct {
	Kw     string // 标题 / 最近消息关键词，空 = 全部
	Limit  int
	Offset int
}
