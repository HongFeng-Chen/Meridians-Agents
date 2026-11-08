package models

import "time"

// AuditEventPO 审计事件表（audit_events）。
//
// 与 op_histories 的区别：op_histories 是「业务用过什么」（MCP 调用、技能使用、
// 记忆召回，给人回看操作轨迹）；audit_events 是「谁对平台做了什么、是否被允许」
// （授权拒绝、管理面变更、高危工具调用），面向追溯与合规。
//
// 设计要点：
//   - 只追加，不更新不删除（过期由保留策略批量清理）；
//   - 身份字段（user/tenant/auth/admin/local）随事件快照落库，不靠 join——
//     用户改名或租户调整后，历史事件仍应还原当时的事实；
//   - detail 存 JSON 文本，字段自由但要短（写入侧已截断到 2000 字符）。
type AuditEventPO struct {
	ID         uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	Ts         time.Time `gorm:"column:ts;not null;index:idx_audit_events_ts" json:"ts"`
	Action     string    `gorm:"column:action;size:128;not null;index:idx_audit_events_action" json:"action"`
	Result     string    `gorm:"column:result;size:16;not null;index:idx_audit_events_result" json:"result"` // allowed | denied | error
	Resource   string    `gorm:"column:resource;size:32;not null;default:''" json:"resource"`
	ResourceID string    `gorm:"column:resource_id;size:128;not null;default:''" json:"resource_id"`
	Risk       string    `gorm:"column:risk;size:16;not null;default:''" json:"risk"`

	UserID   string `gorm:"column:user_id;size:128;not null;default:'';index:idx_audit_events_user" json:"user_id"`
	TenantID string `gorm:"column:tenant_id;size:128;not null;default:''" json:"tenant_id"`
	AuthType string `gorm:"column:auth_type;size:16;not null;default:''" json:"auth_type"`
	Admin    bool   `gorm:"column:is_admin;not null;default:false" json:"is_admin"`
	Local    bool   `gorm:"column:is_local;not null;default:false" json:"is_local"`

	IP        string `gorm:"column:ip;size:64;not null;default:''" json:"ip"`
	Detail    string `gorm:"column:detail;type:text" json:"detail"`
	TraceID   string `gorm:"column:trace_id;size:64;not null;default:''" json:"trace_id"`
	LatencyMS int64  `gorm:"column:latency_ms;not null;default:0" json:"latency_ms"`
}

// TableName 表名
func (AuditEventPO) TableName() string { return "audit_events" }
