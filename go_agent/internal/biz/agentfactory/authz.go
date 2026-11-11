package agentfactory

// 运行时二次授权（Runtime Re-Authorization）。
//
// 快照冻结的是「配置」，不是「权限」。任务恢复、重试、长时间运行期间，
// 资源归属 / 授权 / 空间共享都可能已经变了；如果执行时沿用提交那一刻的
// 判断结论，就会出现「已经被收回权限的人，任务还在替他调工具」。
//
// 因此每次任务开始执行、每次真正的资源访问（工具 / 技能 / 委派）都重新问一次
// resource.UseCase：
//
//	不存在 → 未登记 → 兼容旧数据放行（opt-in 加固，见已知边界）
//	已登记 → Check(principal, resource, execute) → 拒绝则失败并留痕
//
// 资源 ID 约定：agent:<agent_key>、tool:<unique_name>、skill:<name>。
// 知识库不走这里：检索侧的白名单（Knowledge Space / doc_id 过滤）已经是
// 按请求实时计算的，比这里更细。
//
// 放在执行边界而不是入口：任务的 principal 在子任务里与父任务一致
// （principal 由 ctx 携带，委派不重新认证），所以一次检查覆盖整条链路。

import (
	"context"
	"fmt"

	"go_agent/internal/biz/audit"
	"go_agent/internal/pkg/principal"
)

// 运行时资源 ID 前缀
const (
	ResourceAgentPrefix = "agent:"
	ResourceToolPrefix  = "tool:"
	ResourceSkillPrefix = "skill:"
)

// RuntimeActionExecute 运行时访问动作（与 resource 包的 ActionExecute 同值；
// 这里不直接依赖 resource 包，避免 biz 层之间多一条编译期耦合）
const RuntimeActionExecute = "execute"

// ResourceAuthorizer 运行时判权端口（绑定 resource.UseCase）
type ResourceAuthorizer interface {
	// Registered 资源是否已登记：用于区分「未登记（兼容放行）」与「明确拒绝」
	Registered(ctx context.Context, resourceID string) (bool, error)
	Check(ctx context.Context, p *principal.Principal, resourceID, action string) (bool, error)
}

// SetResourceAuthorizer 注入判权端口（wire 装配时调用；nil = 关闭二次授权）
func (u *UseCase) SetResourceAuthorizer(a ResourceAuthorizer) { u.authz = a }

// authorizeTaskRuntime 任务开始执行前的准入：Agent 本身是否还能执行。
func (u *UseCase) authorizeTaskRuntime(ctx context.Context, t *Task, def *AgentDef) error {
	if def == nil {
		return nil
	}
	return u.checkRuntimePermission(ctx, t, ResourceAgentPrefix+def.AgentKey, "执行 Agent「"+def.Name+"」")
}

// authorizeCapabilityRuntime 能力调用前的资源级校验（工具 / 技能 / 委派目标）。
func (u *UseCase) authorizeCapabilityRuntime(ctx context.Context, rec *Recorder, name string) error {
	var (
		resourceID string
		label      string
	)
	switch {
	case len(name) > len(CapToolPrefix) && name[:len(CapToolPrefix)] == CapToolPrefix:
		resourceID, label = ResourceToolPrefix+name[len(CapToolPrefix):], "调用工具「"+name+"」"
	case len(name) > len(CapSkillPrefix) && name[:len(CapSkillPrefix)] == CapSkillPrefix:
		resourceID, label = ResourceSkillPrefix+name[len(CapSkillPrefix):], "加载技能「"+name+"」"
	case len(name) > len(CapAgentPrefix) && name[:len(CapAgentPrefix)] == CapAgentPrefix:
		resourceID, label = ResourceAgentPrefix+name[len(CapAgentPrefix):], "委派 Agent「"+name+"」"
	default:
		return nil // create_agent 等非资源型能力不走资源判权
	}
	var task *Task
	if rec != nil {
		task = rec.task
	}
	return u.checkRuntimePermission(ctx, task, resourceID, label)
}

// checkRuntimePermission 实际判权。
//
// 失败策略：
//   - 未登记资源：放行（当前只有新接口会登记，全量收口前必须兼容旧数据）；
//   - 判权存储故障：拒绝。这里的存储就是 MySQL——它挂了任务本来也写不进任何
//     状态，与其带着不确定的权限跑下去，不如明确失败，由租约回收重排。
func (u *UseCase) checkRuntimePermission(ctx context.Context, t *Task, resourceID, label string) error {
	if u.authz == nil || resourceID == "" {
		return nil
	}
	registered, err := u.authz.Registered(ctx, resourceID)
	if err != nil {
		return fmt.Errorf("运行时判权失败（%s）: %w", label, err)
	}
	if !registered {
		return nil
	}
	p := principal.From(ctx)
	ok, err := u.authz.Check(ctx, p, resourceID, RuntimeActionExecute)
	if err != nil {
		return fmt.Errorf("运行时判权失败（%s）: %w", label, err)
	}
	if ok {
		return nil
	}
	audit.Denied(ctx, "agentfactory.runtime.authorize", "resource", resourceID, "运行时二次授权拒绝")
	var taskID uint64
	if t != nil {
		taskID = t.ID
	}
	u.warn("运行时二次授权拒绝", "resource", resourceID, "task_id", taskID, "subject", subjectOf(p))
	return fmt.Errorf("%w: %s（权限可能已被收回，请重新授权后重试）", ErrRuntimeDenied, label)
}

// subjectOf 判权主体标识（日志用）
func subjectOf(p *principal.Principal) string {
	if p == nil {
		return ""
	}
	return p.SubjectID
}
