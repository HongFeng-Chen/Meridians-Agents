package agentfactory

// 子 Agent 委派上下文（Delegation Context）。
//
// 威胁模型：子 Agent 不能因为「被父 Agent 调用」而获得超出自身配置的权限，
// 更不能把父级已经收敛的权限重新放大。两级防线：
//
//  1. 子任务只按**子 Agent 自己的能力声明**执行——不会继承父的白名单。
//     父定义里写 `agent:<child>` 这件事本身，语义就是「允许按该 Agent 的配置
//     使用它」；如果把它解释成「父的白名单就是子能力上限」，那所有多 Agent
//     编排都得让父再抄一遍子 Agent 的工具清单，实际只会退化成一份大而全的白名单。
//     委派授权的边界由运行时二次授权（authz.go）兜底：执行者必须对
//     `agent:<child>` 有 execute 权限。
//
//  2. 一旦父任务**本身**处于受限上下文（它的 DelegatedCaps 非 nil，说明它是
//     被别人委派出来的、且上游已经把权限收窄），子任务就必须与父的受限清单
//     求交。这一条保证多级委派单调收紧：A ⊇ B ⊇ C，链路上任何一级都不能把
//     上游收回的权限重新发放出去。
//
// 语义约定：
//   - nil = 未受限（根任务，或历史数据没有这一列）→ 沿用子定义；
//   - 非 nil（含空切片）= 受限，执行期只认这一份清单（空切片 = 一项都不给）。
//
// 为什么用 nil / 非 nil 而不是「空即不限」：空切片是有意义的结论
// （上游明确收紧了权限，子任务一项都不该有），不能与「没有约束」混为一谈。

import (
	"fmt"
	"strings"
)

// MaxDelegationDepth 默认委派深度上限（根 = 0）。
// 深度限制是防止「A 委派 B、B 又委派 A」这类成环链路烧光预算的第二道闸
// （第一道是 MaxAgents 数量配额）。
const MaxDelegationDepth = 5

// DelegatedCaps 计算子 Agent 的有效能力集。
//
// 返回 nil 表示不施加额外约束；返回非 nil 表示受限，执行期必须以它为准。
func DelegatedCaps(parentTask *Task, child *AgentDef) []string {
	if parentTask == nil || parentTask.DelegatedCaps == nil {
		// 父任务处于未受限上下文：子 Agent 按自身声明执行
		return nil
	}
	out := make([]string, 0, len(childCaps(child)))
	for _, c := range childCaps(child) {
		if capCovered(parentTask.DelegatedCaps, c) {
			out = append(out, c)
		}
	}
	return out
}

// childCaps 子定义声明的能力（child 为 nil 时为空）
func childCaps(child *AgentDef) []string {
	if child == nil {
		return nil
	}
	return child.Capabilities
}

// effectiveCapabilities 执行期的能力集：有委派上下文时以它为准，否则用定义。
func effectiveCapabilities(t *Task, def *AgentDef) []string {
	if t != nil && t.DelegatedCaps != nil {
		return t.DelegatedCaps
	}
	if def == nil {
		return nil
	}
	return def.Capabilities
}

// capCovered 受限清单是否覆盖某个能力。
//
// 支持父级通配：`mcp:read_*` 覆盖 `mcp:read_doc`。
// 子级自身是通配（`mcp:read_*`）时，只有父级存在完全相同的通配才算覆盖——
// 通配套通配的推导会让边界越算越模糊，宁可保守拒绝（子 Agent 改成精确能力即可）。
func capCovered(parentCaps []string, cap string) bool {
	cap = strings.TrimSpace(cap)
	if cap == "" {
		return false
	}
	childWild := strings.HasSuffix(cap, "*")
	childPrefix := strings.TrimSuffix(cap, "*")
	for _, p := range parentCaps {
		p = strings.TrimSpace(p)
		if p == cap {
			return true
		}
		if !strings.HasSuffix(p, "*") {
			continue
		}
		parentPrefix := strings.TrimSuffix(p, "*")
		if childWild {
			// 通配套通配：只有前缀完全一致才算覆盖
			if parentPrefix == childPrefix {
				return true
			}
			continue
		}
		if strings.HasPrefix(cap, parentPrefix) {
			return true
		}
	}
	return false
}

// ApplyDelegation 组装子任务：写入委派上下文（能力交集 + 归属继承 + 深度闸门）。
//
// 返回的错误是「不该初始化这条子任务」的硬性问题（深度超限 / 上游已收紧到空），
// 调用方直接失败即可，不必建一条注定跑不动的任务。
func (u *UseCase) ApplyDelegation(parent *Task, child *Task, childDef *AgentDef) error {
	if parent == nil || childDef == nil {
		return fmt.Errorf("%w: 委派缺少父子上下文", ErrInvalidTask)
	}
	maxDepth := u.opt.MaxDelegationDepth
	if maxDepth <= 0 {
		maxDepth = MaxDelegationDepth
	}
	if child.Depth > maxDepth {
		return fmt.Errorf("%w: 委派深度 %d 超过上限 %d（链路可能成环）", ErrDelegationDeny, child.Depth, maxDepth)
	}
	caps := DelegatedCaps(parent, childDef)
	if caps != nil && len(caps) == 0 && len(childCaps(childDef)) > 0 {
		return fmt.Errorf("%w: 上游已把权限收紧，没有可下放给「%s」的能力", ErrDelegationDeny, childDef.AgentKey)
	}
	child.DelegatedCaps = caps
	// 归属与租户必须继承父任务：委派不是「换个人执行」，而是同一请求的延伸。
	// 子定义自带租户字段时也不采信——跨租户委派只能由平台侧显式配置。
	child.UserID = parent.UserID
	child.TenantID = parent.TenantID
	child.ConvID = parent.ConvID
	return nil
}
