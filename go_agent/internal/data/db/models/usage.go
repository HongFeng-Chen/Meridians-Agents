package models

import "time"

// UsageRecordPO usage_records：成本中心的追加式用量记录（V2 第十二章）。
//
// 只追加不修改：更正走负值调整行，历史账单永远可复现。
// cost_micros 用整数微单位（1 元 = 1_000_000），避免浮点累加误差。
type UsageRecordPO struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`

	TenantID  string `gorm:"column:tenant_id;size:64;not null;default:'';index:idx_usage_tenant" json:"tenant_id"`
	SubjectID string `gorm:"column:subject_id;size:128;not null;default:'';index:idx_usage_subject" json:"subject_id"`
	AgentID   string `gorm:"column:agent_id;size:64;not null;default:'';index:idx_usage_agent" json:"agent_id"`
	TaskID    string `gorm:"column:task_id;size:64;not null;default:'';index:idx_usage_task" json:"task_id"`

	// RefKind / RefID 通用归属（评测运行 / 会话等没有任务行的场景）。
	// 复合索引按「先类型后标识」：查 ref 一定是确定类型下的某个 ID。
	RefKind string `gorm:"column:ref_kind;size:32;not null;default:'';index:idx_usage_ref,priority:1" json:"ref_kind"`
	RefID   string `gorm:"column:ref_id;size:64;not null;default:'';index:idx_usage_ref,priority:2" json:"ref_id"`

	Provider string `gorm:"column:provider;size:64;not null;default:''" json:"provider"`
	Model    string `gorm:"column:model;size:128;not null;default:'';index:idx_usage_model" json:"model"`
	Kind     string `gorm:"column:kind;size:32;not null;default:'llm';index:idx_usage_kind" json:"kind"`

	InputTokens  int64 `gorm:"column:input_tokens;not null;default:0" json:"input_tokens"`
	OutputTokens int64 `gorm:"column:output_tokens;not null;default:0" json:"output_tokens"`
	TotalTokens  int64 `gorm:"column:total_tokens;not null;default:0" json:"total_tokens"`

	CostMicros   int64  `gorm:"column:cost_micros;not null;default:0" json:"cost_micros"`
	Currency     string `gorm:"column:currency;size:8;not null;default:'CNY'" json:"currency"`
	PriceVersion string `gorm:"column:price_version;size:64;not null;default:''" json:"price_version"`

	CreatedAt time.Time `gorm:"column:created_at;not null;index:idx_usage_created" json:"created_at"`
}

// TableName 表名
func (UsageRecordPO) TableName() string { return "usage_records" }
