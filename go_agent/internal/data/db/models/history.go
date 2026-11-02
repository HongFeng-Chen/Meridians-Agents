package models

// op_histories 统一操作历史：MCP 调用 / 技能使用 / 记忆召回 / RAG 召回 / GraphRAG 召回
// 五类「用到了什么」的记录，kind 区分类型，task_id 关联 AgentFactory 任务（0 = 手动调用）。
import "time"

// OpHistoryPO 操作历史（op_histories）
type OpHistoryPO struct {
	ID       uint64 `gorm:"primaryKey;autoIncrement"`
	Kind     string `gorm:"column:kind;size:32;not null;index:idx_op_histories_kind"`
	TaskID   uint64 `gorm:"column:task_id;not null;default:0;index:idx_op_histories_task"`
	AgentKey string `gorm:"column:agent_key;size:64;not null;default:''"`
	// ConvID 来源会话（会话编排内的调用/召回自动写入；会话链路时间线查询用）
	ConvID  string `gorm:"column:conv_id;size:64;not null;default:'';index:idx_op_histories_conv"`
	Ref     string `gorm:"column:ref;size:191;not null;default:'';index:idx_op_histories_ref"`
	Query   string `gorm:"column:query;size:1024;not null;default:''"`
	Summary string `gorm:"column:summary;size:1024;not null;default:''"`
	// Detail 结构化明细（JSON 文本）：召回类记录存逐条命中与得分，供前端展开查看
	Detail    string    `gorm:"column:detail;type:text"`
	OK        bool      `gorm:"column:ok;not null;index:idx_op_histories_ok"`
	LatencyMS int64     `gorm:"column:latency_ms;not null;default:0"`
	CreatedAt time.Time `gorm:"column:created_at;not null;index:idx_op_histories_created"`
}

// TableName 表名
func (OpHistoryPO) TableName() string { return "op_histories" }
