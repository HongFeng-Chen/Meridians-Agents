package org

import "context"

// Repo 系统管理域仓储（MySQL：tenants / users 两表）。
//
// 只做存取与唯一键冲突翻译，业务规则（谁能改谁、public 不可删、最后一个管理员）
// 全部在 UseCase 里，便于单测用内存实现覆盖。
type Repo interface {
	// ListTenants 租户列表；实现侧负责「内置租户在前、其余按创建时间」
	ListTenants(ctx context.Context, f TenantFilter) ([]Tenant, error)
	// GetTenant 按 key 取租户；不存在返回 ErrTenantNotFound
	GetTenant(ctx context.Context, key string) (*Tenant, error)
	// CreateTenant 新建；key 冲突返回 ErrTenantExists
	CreateTenant(ctx context.Context, t *Tenant) error
	// UpdateTenant 按 key 更新名称/状态/备注（零值字段不写）
	UpdateTenant(ctx context.Context, t *Tenant) error
	// DeleteTenant 按 key 删除
	DeleteTenant(ctx context.Context, key string) error
	// Stats 批量取租户计数（成员数、任务数）
	Stats(ctx context.Context, keys []string) (map[string]Stat, error)

	// ListUsers 成员列表；total 为过滤后的总数（不受分页影响）
	ListUsers(ctx context.Context, f UserFilter) ([]User, int, error)
	// GetUser 按 id 取成员；不存在返回 ErrUserNotFound
	GetUser(ctx context.Context, id int64) (*User, error)
	// CreateUser 新建；同租户 username 冲突返回 ErrUserExists
	CreateUser(ctx context.Context, u *User) error
	// UpdateUser 按 id 更新资料/角色/状态（零值字段不写）
	UpdateUser(ctx context.Context, u *User) error
	// DeleteUser 按 id 删除
	DeleteUser(ctx context.Context, id int64) error
	// CountActiveAdmins 统计租户内启用状态的 tenant_admin 数（excludeID 用于「排除自己」）
	CountActiveAdmins(ctx context.Context, tenantID string, excludeID int64) (int, error)
	// CountUsers 租户内成员数（删除租户前的守卫）
	CountUsers(ctx context.Context, tenantID string) (int, error)

	// FindUserByUsername 按（租户, 用户名）取成员——登录链路的入口查询。
	// 返回的 User 不带密码哈希（列表/详情不该有它），验密用下面的 AuthenticatePassword。
	// 不存在返回 ErrUserNotFound。
	FindUserByUsername(ctx context.Context, tenantID, username string) (*User, error)
	// GetUserCredentials 取用户的密码哈希与状态（登录验密专用，走独立方法避免
	// 密码材料混进通用 User 结构）。不存在返回 ErrUserNotFound。
	GetUserCredentials(ctx context.Context, tenantID, username string) (passwordHash string, status string, err error)
	// SetPasswordHash 写入/重置密码哈希（bcrypt 之后的结果，长度有限校验在 biz 层）
	SetPasswordHash(ctx context.Context, id int64, passwordHash string) error
	// SyncLDAPUser LDAP 登录后的档案同步：存在则更新档案与角色，不存在且
	// provision=true 时建号（source=ldap）。返回最终用户。
	SyncLDAPUser(ctx context.Context, tenantID, username, displayName, email, role string, provision bool) (*User, error)
}
