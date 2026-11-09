// Package org 系统管理域：多租户（tenants）与成员名册（users）。
//
// 为什么需要这一层：租户隔离键早已写进 agent_tasks / memories 等表，但「有哪些租户、
// 每个租户下有哪些人、谁该被停用」一直只存在于令牌签发方。控制台看不到、也改不了，
// 于是停用一个人只能去发令牌的系统里操作，平台侧连审计对象都没有。
//
// 边界（刻意划清）：
//   - 名册只管「记账」，不参与登录认证。身份仍由 JWT / 静态令牌决定；
//   - 租户 key 创建后不可改——它已经是历史数据的隔离键，改名等于制造孤儿数据；
//   - 停用租户/成员只是标记，不阻断既有数据读写（阻断会牵连正在跑的任务）。
package org

import (
	"errors"
	"time"
)

// 内置约定
const (
	// DefaultTenantKey 默认租户：认证关闭时的开发身份、历史数据的归属，不可删除
	DefaultTenantKey = "public"
	// DefaultUser 历史数据与开发态使用的用户名，不可从名册删除
	DefaultUser = "default"

	StatusActive   = "active"
	StatusDisabled = "disabled"

	SourceManual = "manual"
)

// 领域错误：service 层按 errors.Is 映射成 HTTP 状态码。
var (
	ErrTenantNotFound = errors.New("租户不存在")
	ErrTenantExists   = errors.New("租户键已被占用")
	ErrUserNotFound   = errors.New("成员不存在")
	ErrUserExists     = errors.New("该租户下用户名已存在")
	ErrInvalidTenant  = errors.New("租户参数不合法")
	ErrInvalidUser    = errors.New("成员参数不合法")
	ErrBuiltinTenant  = errors.New("内置租户不可删除")
	ErrTenantHasUsers = errors.New("租户下仍有成员，请先移除或迁移")
	ErrLastAdmin      = errors.New("租户至少要保留一名启用状态的租户管理员")
	ErrSelfProtect    = errors.New("不能停用或删除自己")
	ErrForbidden      = errors.New("权限不足")
)

// Tenant 租户（隔离域）。
type Tenant struct {
	Key    string
	Name   string
	Status string
	Remark string
	// Builtin 内置租户（public）：不可删除
	Builtin bool

	CreatedAt time.Time
	UpdatedAt time.Time

	// 派生字段（列表查询时填充）
	UserCount int
	TaskCount int64
}

// User 成员名册条目。
type User struct {
	ID          int64
	TenantID    string
	Username    string
	DisplayName string
	Email       string
	// Role 取值为 authz.Role 的规范写法（platform_admin / tenant_admin / developer /
	// operator / auditor / user），由 UseCase 归一化后入库。
	Role   string
	Status string
	Source string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TenantFilter 租户列表过滤条件。
type TenantFilter struct {
	// Key 非空时只查该租户（非平台管理员的强制作用域）
	Key string
	// Q 关键字：匹配 key / name
	Q      string
	Status string
}

// UserFilter 成员列表过滤条件。
type UserFilter struct {
	// TenantID 非空时只查该租户（非平台管理员的强制作用域）
	TenantID string
	// Q 关键字：匹配 username / display_name / email
	Q      string
	Role   string
	Status string
}

// TenantInput 新建租户入参。
type TenantInput struct {
	Key    string
	Name   string
	Remark string
}

// TenantPatch 修改租户：nil 表示不改（key 不可改）。
type TenantPatch struct {
	Name   *string
	Status *string
	Remark *string
}

// UserInput 新建成员入参。
type UserInput struct {
	TenantID    string
	Username    string
	DisplayName string
	Email       string
	Role        string
}

// UserPatch 修改成员：nil 表示不改（username 与所属租户不可改）。
type UserPatch struct {
	DisplayName *string
	Email       *string
	Role        *string
	Status      *string
}

// Stat 单个租户的资源计数。
type Stat struct {
	UserCount int
	TaskCount int64
}
