// Package authz 角色与权限模型（RBAC）。
//
// 背景：平台原先只有一个 `Admin` 布尔量——要么全平台管理员，要么什么管理操作都做不了。
// 这带来两个问题：
//   - 运维要审批任务、审计要读记录，都被迫发一张「平台管理员」令牌（权限远大于所需）；
//   - 想给某个用户「只能改 Agent/Prompt」也没有表达方式。
//
// 这里把权限拆成「资源:动作」，角色只负责映射到一组权限。判定仍然是**拒绝优先**：
// 没有任何角色匹配就是拒绝，不因为「已登录」而放行。
//
// 与 identity 的分工：
//   - identity 只回答「你是谁」（用户、租户、凭证来源）；
//   - authz 回答「你能不能做这件事」，输入只有角色字符串，不碰凭证与网络。
package authz

import (
	"context"
	"strings"

	"go_agent/internal/pkg/identity"
)

// Role 角色标识（令牌里 `role` / `roles` claim 的取值）
type Role string

const (
	// RolePlatformAdmin 平台管理员：平台级配置（MCP 服务、模型数据源、技能市场、AI 平台）
	RolePlatformAdmin Role = "platform_admin"
	// RoleTenantAdmin 租户管理员：本租户的成员、配额与全部租户内资源
	RoleTenantAdmin Role = "tenant_admin"
	// RoleDeveloper 开发者：Agent / Prompt / Skill 的开发与调试
	RoleDeveloper Role = "developer"
	// RoleOperator 运维：任务执行、审批、重试
	RoleOperator Role = "operator"
	// RoleAuditor 审计：只读执行记录与审计事件
	RoleAuditor Role = "auditor"
	// RoleUser 普通用户：使用被授权的 Agent（默认角色）
	RoleUser Role = "user"
)

// Perm 权限点，形如 "资源:动作"
type Perm string

const (
	// PermPlatformConfig 平台级配置读写（MCP / 模型数据源 / 技能市场 / AI 平台）
	PermPlatformConfig Perm = "platform:config"
	// PermTenantManage 租户内成员、配额管理
	PermTenantManage Perm = "tenant:manage"
	// PermAgentDevelop Agent 定义 / Prompt / Skill 的创建与修改
	PermAgentDevelop Perm = "agent:develop"
	// PermTaskOperate 任务提交、取消、重试
	PermTaskOperate Perm = "task:operate"
	// PermTaskApprove 人工审批（批准/驳回挂起任务）
	PermTaskApprove Perm = "task:approve"
	// PermAuditRead 跨用户读取审计 / 调用记录（默认只有本人可见）
	PermAuditRead Perm = "audit:read"
)

// matrix 角色 → 权限集合。新增权限点时只需在这里授权，判定逻辑不用改。
var matrix = map[Role]map[Perm]bool{
	RolePlatformAdmin: {
		PermPlatformConfig: true,
		PermTenantManage:   true,
		PermAgentDevelop:   true,
		PermTaskOperate:    true,
		PermTaskApprove:    true,
		PermAuditRead:      true,
	},
	RoleTenantAdmin: {
		PermTenantManage: true,
		PermAgentDevelop: true,
		PermTaskOperate:  true,
		PermTaskApprove:  true,
		PermAuditRead:    true,
	},
	RoleDeveloper: {
		PermAgentDevelop: true,
		PermTaskOperate:  true,
	},
	RoleOperator: {
		PermTaskOperate: true,
		PermTaskApprove: true,
	},
	RoleAuditor: {
		PermAuditRead: true,
	},
	RoleUser: {},
}

// alias 常见写法的归一化（令牌通常由别的系统签发，不能要求对方改 claim）
var alias = map[string]Role{
	"admin":          RolePlatformAdmin,
	"superadmin":     RolePlatformAdmin,
	"platform-admin": RolePlatformAdmin,
	"platformadmin":  RolePlatformAdmin,
	"tenant-admin":   RoleTenantAdmin,
	"tenantadmin":    RoleTenantAdmin,
	"dev":            RoleDeveloper,
	"ops":            RoleOperator,
	"audit":          RoleAuditor,
	"viewer":         RoleAuditor,
	"member":         RoleUser,
}

// Normalize 把任意写法归一化成已知角色；未知角色原样返回（不会因此获得任何权限）
func Normalize(raw string) Role {
	r := Role(strings.ToLower(strings.TrimSpace(raw)))
	if r == "" {
		return ""
	}
	if mapped, ok := alias[string(r)]; ok {
		return mapped
	}
	if _, ok := matrix[r]; ok {
		return r
	}
	return r
}

// Roles 当前请求的角色集合。
// Admin 标记（老令牌只有 role=admin）等价于平台管理员，保持向后兼容。
func Roles(ctx context.Context) []Role {
	info := identity.From(ctx)
	out := make([]Role, 0, len(info.Roles)+1)
	seen := map[Role]bool{}
	add := func(r Role) {
		if r == "" || seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
	}
	if info.Admin {
		add(RolePlatformAdmin)
	}
	for _, raw := range info.Roles {
		add(Normalize(raw))
	}
	if len(out) == 0 {
		add(RoleUser)
	}
	return out
}

// Can 角色集合是否包含某个权限（拒绝优先：没有显式授予就是不允许）
func Can(roles []Role, p Perm) bool {
	for _, r := range roles {
		if matrix[r][p] {
			return true
		}
	}
	return false
}

// Allowed 当前请求是否具备该权限。
//
// 开发态（认证关闭 + 回环地址）沿用 identity.AdminAllowed 的规则视为平台管理员，
// 保证单机开发不被自己的权限模型挡住；这条逃生阀只在 dev + local 下成立。
func Allowed(ctx context.Context, p Perm) bool {
	if identity.AdminAllowed(ctx) {
		return true
	}
	// 只有内置角色矩阵：角色来自令牌 claim 或成员名册（users.role），
	// 没有「数据库里另存一份授权」的旁路——权限变更必须走代码评审或改角色，
	// 不存在「策略表被误删 / 被误配导致实际权限与代码不一致」的中间态。
	return Can(Roles(ctx), p)
}

// HasRole 当前请求是否具备某个角色（含 Admin 兼容与别名归一化）
func HasRole(ctx context.Context, want Role) bool {
	for _, r := range Roles(ctx) {
		if r == want {
			return true
		}
	}
	return false
}
