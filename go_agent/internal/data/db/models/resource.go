package models

import "time"

// ResourceMetadataPO resource_metadata：统一资源元数据（V2 第四章）。
//
// 只存跨类型一致的「归属 / 可见性 / 版本」，业务配置仍留在各业务表
// （agent_definitions / prompt_templates / …），避免一张宽表塞满稀疏字段。
// resource_id 由业务自然键提供，例如 agent_key、prompt 模板 ID。
type ResourceMetadataPO struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	ResourceID string `gorm:"column:resource_id;size:64;not null;uniqueIndex:uk_resource_type_id,priority:2" json:"resource_id"`
	// ResourceType 参与唯一键：不同类型可以有同名 ID（agent 与 prompt 各有一个 "default"）
	ResourceType string `gorm:"column:resource_type;size:32;not null;uniqueIndex:uk_resource_type_id,priority:1;index:idx_resource_type" json:"resource_type"`

	TenantID string `gorm:"column:tenant_id;size:64;not null;default:'';index:idx_resource_tenant" json:"tenant_id"`
	SpaceID  string `gorm:"column:space_id;size:64;not null;default:'';index:idx_resource_space" json:"space_id"`
	OwnerID  string `gorm:"column:owner_id;size:128;not null;default:'';index:idx_resource_owner" json:"owner_id"`

	Name        string `gorm:"column:name;size:128;not null;default:''" json:"name"`
	Description string `gorm:"column:description;type:text" json:"description"`

	// Visibility private | tenant | platform
	Visibility string `gorm:"column:visibility;size:16;not null;default:'private';index:idx_resource_visibility" json:"visibility"`

	Version int64  `gorm:"column:version;not null;default:1" json:"version"`
	Status  string `gorm:"column:status;size:32;not null;default:'active';index:idx_resource_status" json:"status"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 表名
func (ResourceMetadataPO) TableName() string { return "resource_metadata" }

// ResourcePermissionPO resource_permissions：资源级显式授权。
//
// 一个「主体 + 动作」一行，带 allow/deny：判权时直接命中唯一索引，
// 撤销单个动作也不需要读改写整行 JSON。
type ResourcePermissionPO struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	ResourceID string `gorm:"column:resource_id;size:64;not null;uniqueIndex:uk_resource_perm,priority:1;index:idx_resource_perm_res" json:"resource_id"`

	PrincipalType string `gorm:"column:principal_type;size:32;not null;uniqueIndex:uk_resource_perm,priority:2" json:"principal_type"`
	PrincipalID   string `gorm:"column:principal_id;size:128;not null;uniqueIndex:uk_resource_perm,priority:3" json:"principal_id"`
	Action        string `gorm:"column:action;size:32;not null;uniqueIndex:uk_resource_perm,priority:4" json:"action"`

	// Effect allow | deny：Deny 永远优先于 Allow
	Effect string `gorm:"column:effect;size:8;not null;default:'allow'" json:"effect"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 表名
func (ResourcePermissionPO) TableName() string { return "resource_permissions" }

// ResourceVersionPO resource_versions：不可变版本快照（发布/更新时追加）。
//
// snapshot 保存当时完整的资源元数据与配置摘要；历史版本只读，
// 不能用「当前配置」回填历史——那会让回滚和评估复现失去意义。
type ResourceVersionPO struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	ResourceID string `gorm:"column:resource_id;size:64;not null;uniqueIndex:uk_resource_version,priority:1;index:idx_resource_ver_res" json:"resource_id"`
	Version    int64  `gorm:"column:version;not null;uniqueIndex:uk_resource_version,priority:2" json:"version"`

	Snapshot string `gorm:"column:snapshot;type:mediumtext;not null" json:"snapshot"`
	// Checksum 配置校验摘要（sha256），用于比对「两次发布是否等价」
	Checksum  string    `gorm:"column:checksum;size:64;not null;default:''" json:"checksum"`
	CreatedBy string    `gorm:"column:created_by;size:128;not null;default:''" json:"created_by"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 表名
func (ResourceVersionPO) TableName() string { return "resource_versions" }
