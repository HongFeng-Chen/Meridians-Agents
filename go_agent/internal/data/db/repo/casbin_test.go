package repo

import (
	"testing"
	"time"

	"go_agent/internal/pkg/authz"

	casbin "github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
)

// 内存引擎：验证模型语义（租户内绑定 / 全局绑定 / 直接授权 / 域匹配），
// 不碰数据库；gorm-adapter 的落库行为由集成环境验证。
func newMemoryEngine(t *testing.T) *casbin.SyncedCachedEnforcer {
	t.Helper()
	m, err := model.NewModelFromString(casbinModel)
	if err != nil {
		t.Fatal(err)
	}
	e, err := casbin.NewSyncedCachedEnforcer(m)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([][]string, 0, len(rolePolicySeed))
	for _, r := range rolePolicySeed {
		rows = append(rows, r[1:])
	}
	if _, err := e.AddPolicies(rows); err != nil {
		t.Fatal(err)
	}
	return e
}

func enforce(t *testing.T, e *casbin.SyncedCachedEnforcer, sub, dom, perm string) bool {
	t.Helper()
	ok, err := e.Enforce(sub, dom, perm)
	if err != nil {
		t.Fatalf("enforce(%s,%s,%s): %v", sub, dom, perm, err)
	}
	return ok
}

// 角色绑定：用户绑成租户管理员后， Casbin 独立给出该角色的权限（不依赖内置矩阵）。
func TestRoleBindingInTenant(t *testing.T) {
	e := newMemoryEngine(t)
	if _, err := e.AddGroupingPolicy("alice", "role:tenant_admin", "acme"); err != nil {
		t.Fatal(err)
	}
	if !enforce(t, e, "alice", "acme", "tenant:manage") {
		t.Fatal("租户内绑定应获得 tenant:manage")
	}
	if enforce(t, e, "alice", "acme", "platform:config") {
		t.Fatal("tenant_admin 不应有 platform:config")
	}
	if enforce(t, e, "alice", "other", "tenant:manage") {
		t.Fatal("绑定只对 acme 生效（域隔离）")
	}
}

// 全局绑定（dom=*）在任何租户都生效。
func TestGlobalRoleBinding(t *testing.T) {
	e := newMemoryEngine(t)
	if _, err := e.AddGroupingPolicy("carol", "role:auditor", "*"); err != nil {
		t.Fatal(err)
	}
	if !enforce(t, e, "carol", "any-tenant", "audit:read") {
		t.Fatal("全局绑定应跨租户生效")
	}
	if enforce(t, e, "carol", "any-tenant", "task:operate") {
		t.Fatal("auditor 不应有 task:operate")
	}
}

// 直接授权：给具体用户授权限点，绕过角色。
func TestDirectPolicyGrant(t *testing.T) {
	e := newMemoryEngine(t)
	if _, err := e.AddPolicy("bob", "acme", "platform:config"); err != nil {
		t.Fatal(err)
	}
	if !enforce(t, e, "bob", "acme", "platform:config") {
		t.Fatal("直接授权应生效")
	}
	if enforce(t, e, "bob", "other", "platform:config") {
		t.Fatal("直接授权只对 acme 生效")
	}
}

// 种子的角色矩阵必须与 authz 内置矩阵一致：单点漂移会被这里拦下。
// （新增权限点时要同时改 authz.matrix 与 rolePolicySeed。）
func TestRolePolicySeedMatchesMatrix(t *testing.T) {
	seed := map[string]bool{}
	for _, row := range rolePolicySeed {
		seed[row[1]+"|"+row[3]] = true
	}
	for _, role := range authz.AllRoles() {
		for _, perm := range authz.AllPerms() {
			granted := authz.Can([]authz.Role{role}, perm)
			inSeed := seed["role:"+string(role)+"|"+string(perm)]
			if granted != inSeed {
				t.Fatalf("矩阵与种子不一致: role=%s perm=%s matrix=%v seed=%v",
					role, perm, granted, inSeed)
			}
		}
	}
}

// TestCasbinReloadIntervalIsSane 策略轮询间隔必须是「分钟级」，不能是纳秒级。
//
// 回归背景：StartAutoLoadPolicy 的参数是 time.Duration（纳秒基准）。
// 曾写成 e.StartAutoLoadPolicy(5 * 60 * 1000)，本意是 5 分钟，实际是 0.3 毫秒：
// 每秒数百次 LoadPolicy → 日志被 SELECT * FROM casbin_rule 刷满，且每次都清空
// 判定缓存、加载期持有写锁。这类错误不会报错、不会 panic，只会悄悄烧 CPU，
// 所以用一条断言把单位钉死。
func TestCasbinReloadIntervalIsSane(t *testing.T) {
	if casbinReloadInterval < time.Minute {
		t.Fatalf("重载间隔 %v 过短：疑似又按毫秒写了数值（time.Duration 是纳秒基准）", casbinReloadInterval)
	}
	if casbinReloadInterval > time.Hour {
		t.Fatalf("重载间隔 %v 过长：多实例场景下策略变更迟迟不生效", casbinReloadInterval)
	}
}
