package authz

import (
	"context"
	"testing"

	"go_agent/internal/pkg/identity"
)

// fakeEngine 引擎替身：只放行白名单里的 (sub, dom, perm)
type fakeEngine struct {
	grants map[string]bool
}

func (f *fakeEngine) Allow(sub, dom string, p Perm) bool {
	return f.grants[sub+"|"+dom+"|"+string(p)]
}

func (f *fakeEngine) Reload() error { return nil }

func ctxAs(user, tenant string) context.Context {
	return identity.With(context.Background(), identity.Info{UserID: user, TenantID: tenant, Auth: "jwt"})
}

// Casbin 命中即放行：普通用户也能被额外策略授权。
func TestEngineGrantOverridesMatrix(t *testing.T) {
	SetEngine(&fakeEngine{grants: map[string]bool{"carol|acme|platform:config": true}})
	defer SetEngine(nil)

	if !Allowed(ctxAs("carol", "acme"), PermPlatformConfig) {
		t.Fatal("Casbin 额外授予应放行")
	}
	// 未授权的权限仍按内置矩阵拒绝
	if Allowed(ctxAs("carol", "acme"), PermTenantManage) {
		t.Fatal("未授权权限不应放行")
	}
	// 其他租户不受该策略影响
	if Allowed(ctxAs("carol", "other"), PermPlatformConfig) {
		t.Fatal("额外授予按租户隔离")
	}
}

// 引擎未注册时回落内置矩阵，行为与历史一致。
func TestFallbackWithoutEngine(t *testing.T) {
	SetEngine(nil)
	defer SetEngine(nil)
	// 普通角色查 platform:config → 拒绝（矩阵里 user 角色无权限）
	if Allowed(ctxAs("someone", "public"), PermPlatformConfig) {
		t.Fatal("无引擎时应按内置矩阵拒绝")
	}
}

// nil 安全：EngineLoaded / 内部包装不会 panic。
func TestNilSafety(t *testing.T) {
	SetEngine(nil)
	if EngineLoaded() {
		t.Fatal("未注册应返回 false")
	}
	if Allowed(ctxAs("x", "public"), PermAuditRead) {
		t.Fatal("nil 引擎不应放行")
	}
}
