package models

import "time"

// TenantPO tenants：租户（隔离域）。
//
// tenant_key 直接做主键：它同时是 agent_tasks / users 等表的 tenant_id 取值，
// 用一个稳定、可读、可加外键的短串，比自增 id 更适合做隔离键。
// 创建后不可修改（改名 = 让历史数据变孤儿），需要改就新建租户再迁成员。
type TenantPO struct {
	Key    string `gorm:"column:tenant_key;primaryKey;size:64" json:"key"`
	Name   string `gorm:"column:name;size:128;not null;default:''" json:"name"`
	Status string `gorm:"column:status;size:16;not null;default:'active';index:idx_tenants_status" json:"status"`
	Remark string `gorm:"column:remark;size:512;not null;default:''" json:"remark"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 表名
func (TenantPO) TableName() string { return "tenants" }

// UserPO users：成员名册（租户内的用户名 → 角色）。
//
// 只做管理，不参与登录：身份仍来自 JWT / 静态令牌，这里回答的是
// 「这个租户下应该有谁、谁是什么角色、谁被停用了」。
// username 对应令牌的 sub，同租户内唯一（跨租户可以同名，那是不同的人）。
type UserPO struct {
	ID uint64 `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	// TenantID 所属租户键；与鉴权链路的 tenant_id 同一取值
	TenantID string `gorm:"column:tenant_id;size:64;not null;default:'public';index:idx_users_tenant;uniqueIndex:uk_users_tenant_username,priority:1" json:"tenant_id"`
	// Username 登录主体（JWT sub）；创建后不可改
	Username    string `gorm:"column:username;size:128;not null;uniqueIndex:uk_users_tenant_username,priority:2" json:"username"`
	DisplayName string `gorm:"column:display_name;size:128;not null;default:''" json:"display_name"`
	Email       string `gorm:"column:email;size:128;not null;default:''" json:"email"`
	// Role authz 规范角色：platform_admin | tenant_admin | developer | operator | auditor | user
	Role string `gorm:"column:role;size:32;not null;default:'user';index:idx_users_role" json:"role"`
	// Status active | disabled
	Status string `gorm:"column:status;size:16;not null;default:'active';index:idx_users_status" json:"status"`
	// Source 来源：manual（控制台登记）| ldap（AD/LDAP 首次登录自动建号）
	Source string `gorm:"column:source;size:16;not null;default:'manual'" json:"source"`
	// PasswordHash bcrypt 哈希（本地密码登录用）；空 = 该用户只能走 LDAP 验密。
	// 平台永远不存明文密码，重置密码 = 写入新哈希。
	PasswordHash string `gorm:"column:password_hash;size:128;not null;default:''" json:"-"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 表名
func (UserPO) TableName() string { return "users" }
