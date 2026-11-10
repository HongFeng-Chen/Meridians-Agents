// engine.go 授权引擎挂载点：Casbin 策略优先，内置角色矩阵兜底。
//
// 分层约束：authz 是 pkg，不能依赖 data（gorm/casbin）。这里用「注册回调」
// 打通——启动时由 data 层把 Casbin 引擎注册进来，之后所有 Allowed 判定先问
// Casbin（额外授予 / 角色绑定），未命中再落回内置角色矩阵。
//
// 为什么不把内置矩阵整个搬进 Casbin：矩阵是代码评审的对象（改权限点必须过
// code review），Casbin 规则运维改在数据库里、即时生效——两者的变更节奏
// 不一样，混在一起会让「基线权限」失去版本控制。
package authz

import "sync"

// PermKey 权限点的字符串形态（Casbin 规则里存的就是它）
func (p Perm) Key() string { return string(p) }

// Engine 额外授权引擎（Casbin）。实现方保证并发安全。
type Engine interface {
	// Allow 是否有额外授予：sub=用户名，dom=租户键，perm=权限点
	Allow(sub, dom string, perm Perm) bool
	// Reload 重新加载策略（多实例部署时手动触发）
	Reload() error
}

var (
	engineMu sync.RWMutex
	engine   Engine
)

// SetEngine 注册授权引擎；重复注册以最后一次为准（wire 只会调一次）。
// 传 nil 可以卸载（测试用）。
func SetEngine(e Engine) {
	engineMu.Lock()
	defer engineMu.Unlock()
	engine = e
}

// EngineLoaded 引擎是否已注册（服务层判断「策略管理接口是否可用」用）
func EngineLoaded() bool {
	engineMu.RLock()
	defer engineMu.RUnlock()
	return engine != nil
}

// engineAllow 引擎判定的内部包装：nil 安全。
func engineAllow(sub, dom string, p Perm) bool {
	engineMu.RLock()
	e := engine
	engineMu.RUnlock()
	if e == nil {
		return false
	}
	return e.Allow(sub, dom, p)
}
