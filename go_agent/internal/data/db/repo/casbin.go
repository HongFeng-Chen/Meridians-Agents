// casbin.go Casbin 授权引擎（额外授予层）：p 规则 + g 规则，gorm-adapter 落库。
//
// 定位与 authz 内置角色矩阵的关系：
//   - 内置矩阵（pkg/authz 的 matrix）是**基线**，走代码评审，回答「角色默认能干什么」；
//   - 这里的 casbin_rule 是**增量**，运维在控制台改、即时生效，回答「额外给谁授什么」。
//
// 模型是 RBAC with domains：
//
//	p 规则：p, sub, dom, perm —— 给 sub（用户名或 role:<角色>）在 dom 租户授 perm；
//	g 规则：g, user, role:<角色>, dom —— 把用户在 dom 租户绑成某角色（dom=* 全局）。
//
// 判定链在 pkg/authz.Allowed：开发态 → Casbin → 内置矩阵。平台管理员在更上游
// 已经放行（矩阵全量授权），不依赖这里。
package repo

import (
	"fmt"
	"strings"

	"go_agent/internal/data/db"
	"go_agent/internal/pkg/authz"
	"go_agent/internal/pkg/identity"

	casbin "github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	gormadapter "github.com/casbin/gorm-adapter/v3"
	"gorm.io/gorm"
)

// casbinModel RBAC with domains。刻意收窄：
//   - 不用 keyMatch 通配 perm（策略里的 perm 必须是规范权限点或 *，避免误配半截串）；
//   - g(r.sub, p.sub, r.dom) || g(r.sub, p.sub, "*")：租户内绑定 + 全局绑定都认。
const casbinModel = `
[request_definition]
r = sub, dom, obj

[policy_definition]
p = sub, dom, obj

[role_definition]
g = _, _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (r.sub == p.sub || g(r.sub, p.sub, r.dom) || g(r.sub, p.sub, "*")) && (r.dom == p.dom || p.dom == "*") && (r.obj == p.obj || p.obj == "*")
`

// rolePolicySeed 内置角色矩阵的 Casbin 种子：让「绑角色」的 g 规则能通过
// Casbin 独立生效（而不是绕回内置矩阵）。sub 用 role: 前缀与真实用户名区分。
var rolePolicySeed = func() [][]string {
	// 从 authz 的导出视角逐角色展开（matrix 未导出，这里手写同名清单，
	// 并在测试里断言两份一致——单点漂移会被 CI 拦下）
	type rp struct{ role, perms string }
	list := []rp{
		{"platform_admin", "platform:config,tenant:manage,agent:develop,task:operate,task:approve,audit:read"},
		{"tenant_admin", "tenant:manage,agent:develop,task:operate,task:approve,audit:read"},
		{"developer", "agent:develop,task:operate"},
		{"operator", "task:operate,task:approve"},
		{"auditor", "audit:read"},
	}
	out := make([][]string, 0, len(list))
	for _, it := range list {
		for _, perm := range strings.Split(it.perms, ",") {
			out = append(out, []string{"p", "role:" + it.role, "*", perm})
		}
	}
	return out
}()

// CasbinEngine 授权引擎（data 层实现 authz.Engine）
type CasbinEngine struct {
	e *casbin.SyncedCachedEnforcer
}

var _ authz.Engine = (*CasbinEngine)(nil)

// NewCasbinEngine 构建引擎并幂等种子内置角色的 p 规则。
// casbin_rule 表由 gorm-adapter 自动迁移（复用主库连接，跟 rag_documents 同一个库）。
func NewCasbinEngine(db *gorm.DB) (*CasbinEngine, error) {
	adapter, err := gormadapter.NewAdapterByDBUseTableName(db, "", "casbin_rule")
	if err != nil {
		return nil, fmt.Errorf("初始化 casbin adapter: %w", err)
	}
	m, err := model.NewModelFromString(casbinModel)
	if err != nil {
		return nil, fmt.Errorf("解析 casbin 模型: %w", err)
	}
	e, err := casbin.NewSyncedCachedEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("创建 casbin enforcer: %w", err)
	}
	if err := e.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("加载 casbin 策略: %w", err)
	}
	eng := &CasbinEngine{e: e}
	if err := eng.seed(); err != nil {
		return nil, err
	}
	// 策略可能被其他实例改过：每 5 分钟重载一次（单实例场景写入方就是自己，无影响）
	e.StartAutoLoadPolicy(5 * 60 * 1000)
	return eng, nil
}

// seed 幂等写入内置角色的 p 规则；手动删掉也会被补回来（这是基线，不是可删数据）。
func (c *CasbinEngine) seed() error {
	rules := make([][]string, 0, len(rolePolicySeed))
	for _, rule := range rolePolicySeed {
		rules = append(rules, rule[1:])
	}
	added, err := c.e.AddPolicies(rules)
	if err != nil {
		return fmt.Errorf("种子角色策略: %w", err)
	}
	_ = added
	return nil
}

// Allow 额外授予判定（authz.Engine）
func (c *CasbinEngine) Allow(sub, dom string, perm authz.Perm) bool {
	if sub == "" || perm == "" {
		return false
	}
	ok, err := c.e.Enforce(sub, identityOr(dom), perm.Key())
	return err == nil && ok
}

// identityOr dom 为空时用默认租户，与登录/白名单语义对齐
func identityOr(dom string) string {
	if strings.TrimSpace(dom) == "" {
		return identity.DefaultTenant
	}
	return dom
}

// Reload 手动重载策略
func (c *CasbinEngine) Reload() error { return c.e.LoadPolicy() }

// —— 策略管理（PolicyService 的存储层）——

// ListPolicies 全部 p 规则（含种子行——列表如实展示，种子行删了也会回来）
func (c *CasbinEngine) ListPolicies() [][]string {
	rows, _ := c.e.GetPolicy()
	return rows
}

// AddPolicies 追加 p 规则（已存在自动跳过，幂等）
func (c *CasbinEngine) AddPolicies(rules [][]string) error {
	if _, err := c.e.AddPolicies(rules); err != nil {
		return fmt.Errorf("写入策略: %w", err)
	}
	return nil
}

// RemovePolicies 删除 p 规则
func (c *CasbinEngine) RemovePolicies(rules [][]string) error {
	if _, err := c.e.RemovePolicies(rules); err != nil {
		return fmt.Errorf("删除策略: %w", err)
	}
	return nil
}

// ListBindings 全部 g 规则（用户 → 角色绑定）
func (c *CasbinEngine) ListBindings() [][]string {
	rows, _ := c.e.GetGroupingPolicy()
	return rows
}

// AddBindings 绑定用户角色
func (c *CasbinEngine) AddBindings(rules [][]string) error {
	if _, err := c.e.AddGroupingPolicies(rules); err != nil {
		return fmt.Errorf("写入绑定: %w", err)
	}
	return nil
}

// RemoveBindings 解绑用户角色
func (c *CasbinEngine) RemoveBindings(rules [][]string) error {
	if _, err := c.e.RemoveGroupingPolicies(rules); err != nil {
		return fmt.Errorf("删除绑定: %w", err)
	}
	return nil
}

// NewAuthzEngine wire 入口：构建引擎并注册为全局授权引擎（authz.Allowed 桥接）
func NewAuthzEngine(d *db.Data) (*CasbinEngine, error) {
	eng, err := NewCasbinEngine(d.DB())
	if err == nil {
		authz.SetEngine(eng)
	}
	return eng, err
}
