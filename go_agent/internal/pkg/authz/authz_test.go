package authz

// RBAC 判定的回归用例：角色矩阵、别名归一化、身份上下文与开发态逃生阀。

import (
	"context"
	"testing"

	"go_agent/internal/pkg/identity"
)

func ctxWith(info identity.Info) context.Context {
	return identity.With(context.Background(), info)
}

// 角色矩阵：平台管理员全权；开发者能改 Agent 不能改平台配置；运维能审批不能建 MCP；
// 审计员只能读；普通用户没有任何管理权限。
func TestRoleMatrix(t *testing.T) {
	cases := []struct {
		role  Role
		perm  Perm
		allow bool
	}{
		{RolePlatformAdmin, PermPlatformConfig, true},
		{RolePlatformAdmin, PermAuditRead, true},
		{RoleTenantAdmin, PermPlatformConfig, false},
		{RoleTenantAdmin, PermTenantManage, true},
		{RoleDeveloper, PermAgentDevelop, true},
		{RoleDeveloper, PermPlatformConfig, false},
		{RoleOperator, PermTaskApprove, true},
		{RoleOperator, PermPlatformConfig, false},
		{RoleAuditor, PermAuditRead, true},
		{RoleAuditor, PermAgentDevelop, false},
		{RoleUser, PermAuditRead, false},
		{RoleUser, PermTaskOperate, false},
	}
	for _, c := range cases {
		if got := Can([]Role{c.role}, c.perm); got != c.allow {
			t.Fatalf("%s 对 %s 的判定应为 %v，实际 %v", c.role, c.perm, c.allow, got)
		}
	}
}

// 未知角色不能获得任何权限（拒绝优先）
func TestUnknownRoleGetsNothing(t *testing.T) {
	unknown := Normalize("something-else")
	if unknown != Role("something-else") {
		t.Fatalf("未知角色应原样返回，实际 %v", unknown)
	}
	for _, p := range []Perm{PermPlatformConfig, PermTenantManage, PermAgentDevelop, PermTaskApprove, PermAuditRead} {
		if Can([]Role{unknown}, p) {
			t.Fatalf("未知角色不应获得 %s", p)
		}
	}
}

// 令牌里的写法五花八门，别名要归一化到同一角色
func TestNormalizeAliases(t *testing.T) {
	cases := map[string]Role{
		"admin":          RolePlatformAdmin,
		"superadmin":     RolePlatformAdmin,
		"platform-admin": RolePlatformAdmin,
		"tenant-admin":   RoleTenantAdmin,
		"ops":            RoleOperator,
		"audit":          RoleAuditor,
		"OPerator":       RoleOperator, // 大小写不敏感
		"":               "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %v, want %v", in, got, want)
		}
	}
}

// 身份上下文：roles claim 生效；无角色时视为普通用户；Admin 标记等价平台管理员
func TestAllowedFromContext(t *testing.T) {
	dev := ctxWith(identity.Info{UserID: "alice", TenantID: "t1", Auth: "jwt"})
	if Allowed(dev, PermPlatformConfig) {
		t.Fatal("未授角色的登录用户不应获得平台配置权限")
	}
	if Allowed(dev, PermAuditRead) {
		t.Fatal("未授角色的登录用户不应获得审计读取权限")
	}
	if !HasRole(dev, RoleUser) {
		t.Fatal("无角色时应落到默认 user 角色")
	}

	auditor := ctxWith(identity.Info{UserID: "bob", TenantID: "t1", Auth: "jwt", Roles: []string{"auditor"}})
	if !Allowed(auditor, PermAuditRead) {
		t.Fatal("auditor 应能读审计记录")
	}
	if Allowed(auditor, PermPlatformConfig) {
		t.Fatal("auditor 不应获得平台配置权限")
	}

	admin := ctxWith(identity.Info{UserID: "root", TenantID: "t1", Auth: "jwt", Admin: true})
	if !Allowed(admin, PermPlatformConfig) || !Allowed(admin, PermAuditRead) {
		t.Fatal("Admin 标记应等价平台管理员")
	}

	// 老令牌只写 role=admin（Admin=true，Roles 里是 admin）也走同一路径
	legacy := ctxWith(identity.Info{UserID: "root", Auth: "jwt", Admin: true, Roles: []string{"admin"}})
	if !HasRole(legacy, RolePlatformAdmin) {
		t.Fatal("role=admin 应归一化为 platform_admin")
	}
}

// 开发态（认证关闭 + 回环）沿用逃生阀：否则单机开发会被权限模型挡死
func TestDevLocalEscapeHatch(t *testing.T) {
	devLocal := ctxWith(identity.Info{UserID: "dev", Auth: "dev", Local: true})
	if !Allowed(devLocal, PermPlatformConfig) {
		t.Fatal("认证关闭 + 本机请求应放行，保证单机开发可用")
	}
	// 认证关闭但来自远程：不因为「开发态」而放行
	devRemote := ctxWith(identity.Info{UserID: "dev", Auth: "dev", Local: false})
	if Allowed(devRemote, PermPlatformConfig) {
		t.Fatal("认证关闭 + 远程请求不应获得管理权限")
	}
}
