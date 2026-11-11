package models

import "time"

// KnowledgeSpacePO knowledge_spaces：知识空间（V2 第五章）。
//
// 邻接表（parent_space_id）是权威关系，path / depth 是查询加速：
// 子树查询用 path 前缀，移动时整棵子树一起平移，避免递归 CTE。
// tenant_id 为空表示平台空间（不归属任何租户）。
type KnowledgeSpacePO struct {
	ID            uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	SpaceID       string `gorm:"column:space_id;size:64;not null;uniqueIndex:uk_spaces_space_id" json:"space_id"`
	ParentSpaceID string `gorm:"column:parent_space_id;size:64;not null;default:'';index:idx_spaces_parent" json:"parent_space_id"`
	TenantID      string `gorm:"column:tenant_id;size:64;not null;default:'';index:idx_spaces_tenant" json:"tenant_id"`
	// OwnerID 归属主体：个人空间=用户名，部门空间=部门 ID，平台/租户空间为空
	OwnerID string `gorm:"column:owner_id;size:128;not null;default:'';index:idx_spaces_owner" json:"owner_id"`

	Name        string `gorm:"column:name;size:128;not null" json:"name"`
	Description string `gorm:"column:description;type:text" json:"description"`

	// SpaceType platform | tenant | department | personal | custom
	SpaceType string `gorm:"column:space_type;size:32;not null;index:idx_spaces_type" json:"space_type"`
	// InheritanceMode inherit | restricted | isolated
	InheritanceMode string `gorm:"column:inheritance_mode;size:32;not null;default:'inherit'" json:"inheritance_mode"`

	// Path 物化路径（不含自身），形如 "" / "platform" / "tenant:public/dept:public:rd"
	// 长度 700：utf8mb4 下索引 2800 字节，仍在 InnoDB 3072 字节上限内；
	// 超过 8 层的路径会被 MaxDepth 拦住，不会写进来。
	Path  string `gorm:"column:path;size:700;not null;default:'';index:idx_spaces_path" json:"path"`
	Depth int    `gorm:"column:depth;not null;default:0" json:"depth"`

	Status string `gorm:"column:status;size:32;not null;default:'active';index:idx_spaces_status" json:"status"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 表名
func (KnowledgeSpacePO) TableName() string { return "knowledge_spaces" }

// KnowledgeSpaceSharePO knowledge_space_shares：空间显式共享。
//
// actions 用 JSON 数组保存（一个主体一行，而不是一个动作一行）：
// 空间的授权动作集合小且经常整体调整，按行拆反而让「查看某人有哪些权限」
// 需要聚合。资源级的 resource_permissions 则按动作拆行——那边判权是热路径。
type KnowledgeSpaceSharePO struct {
	ID            uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	SpaceID       string `gorm:"column:space_id;size:64;not null;index:idx_space_shares_space" json:"space_id"`
	PrincipalType string `gorm:"column:principal_type;size:32;not null;index:idx_space_shares_principal,priority:1" json:"principal_type"`
	PrincipalID   string `gorm:"column:principal_id;size:128;not null;index:idx_space_shares_principal,priority:2" json:"principal_id"`

	Actions   string    `gorm:"column:actions;type:json;not null" json:"actions"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 表名
func (KnowledgeSpaceSharePO) TableName() string { return "knowledge_space_shares" }
