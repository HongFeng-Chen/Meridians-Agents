package agentfactory

// 角色协作：角色引用校验 + 监察 Agent / 风险评估 Agent 的调用封装。
//
//   root        根 Agent：手动提交或 cron 定时触发；可编排 members（串行/并行）
//   sub         子 Agent：只作为被委派方 / 编排成员
//   supervisor  监察 Agent：按 supervision_scope 审查成员产出，给出 pass/reject
//   risk        风险评估 Agent：仅在 MCP 工具调用前评估操作风险（low/medium/high）

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// validateRoleRefs 校验角色引用：成员 / 监察 Agent / 风险评估 Agent 必须存在且角色匹配
func (u *UseCase) validateRoleRefs(ctx context.Context, in AgentInput) error {
	for _, key := range in.Members {
		d, err := u.repo.GetDefByKey(ctx, key)
		if err != nil {
			return err
		}
		if d == nil {
			return fmt.Errorf("%w: 协作成员 %q 不存在", ErrInvalidDef, key)
		}
		if !d.Enabled {
			return fmt.Errorf("%w: 协作成员 %q 已停用", ErrInvalidDef, key)
		}
	}
	if in.SupervisorKey != "" {
		d, err := u.repo.GetDefByKey(ctx, in.SupervisorKey)
		if err != nil {
			return err
		}
		if d == nil {
			return fmt.Errorf("%w: 监察 Agent %q 不存在", ErrInvalidDef, in.SupervisorKey)
		}
		if d.Role != RoleSupervisor {
			return fmt.Errorf("%w: %q 的角色是 %s，只有 supervisor 角色可作为监察 Agent", ErrInvalidDef, in.SupervisorKey, roleOrDefault(d.Role))
		}
	}
	if in.RiskAgentKey != "" {
		d, err := u.repo.GetDefByKey(ctx, in.RiskAgentKey)
		if err != nil {
			return err
		}
		if d == nil {
			return fmt.Errorf("%w: 风险评估 Agent %q 不存在", ErrInvalidDef, in.RiskAgentKey)
		}
		if d.Role != RoleRisk {
			return fmt.Errorf("%w: %q 的角色是 %s，只有 risk 角色可作为风险评估 Agent", ErrInvalidDef, in.RiskAgentKey, roleOrDefault(d.Role))
		}
	}
	return nil
}

// roleOrDefault 旧数据 role 为空时按 root 处理
func roleOrDefault(r string) string {
	if r == "" {
		return RoleRoot
	}
	return r
}

// nextRunTime 按 cron 计算下次运行时间（表达式非法返回零值）
func nextRunTime(expr string, after time.Time) time.Time {
	spec, err := ParseCron(expr)
	if err != nil {
		return time.Time{}
	}
	return spec.Next(after)
}

// supervisionVerdict 监察结论
type supervisionVerdict struct {
	Passed  bool   `json:"passed"`
	Issues  string `json:"issues"`
	Suggest string `json:"suggestion"`
	ParseOK bool   `json:"-"`
}

// inScope 判断某个成员是否在监察范围内（空范围或 * = 全部）
func inScope(scope []string, key string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, s := range scope {
		if s == ScopeAll || strings.EqualFold(s, key) {
			return true
		}
	}
	return false
}

// superviseMember 让监察 Agent 审查一个成员的产出。
// 返回 nil 表示无需审查（角色不对 / 不在范围内 / 监察 Agent 不可用）。
func (u *UseCase) superviseMember(ctx context.Context, parent *Task, sup *AgentDef, member string, input, output string, rec *Recorder) *supervisionVerdict {
	if sup == nil || u.chat == nil {
		return nil
	}
	if !inScope(sup.SupervisionScope, member) {
		return nil
	}
	system := sup.System
	if system == "" {
		system = "你是多 Agent 协作的监察 Agent，负责审查其他 Agent 的产出是否满足任务要求。"
	}
	system += `

## 输出协议（严格遵守）
只输出一个 JSON 对象，不要输出其他文字：
{"passed": true 或 false, "issues": "发现的问题（通过则留空）", "suggestion": "给执行方的改进建议（可空）"}

审查要点：
1. 产出是否回答了任务要求，有无跑题或缺失关键信息
2. 是否出现编造事实、越权操作、明显风险行为
3. 结论从严：有关键缺陷就给 false`
	user := fmt.Sprintf("## 原始任务\n%s\n\n## 被审查 Agent\n%s\n\n## 该 Agent 的产出\n%s", truncateRunes(input, 3000), member, truncateRunes(output, 6000))

	out, err := u.chatWith(ctx, sup, system, user)
	if err != nil {
		if rec != nil {
			rec.event(EventSupervise, rec.turn, map[string]any{"agent": member, "error": err.Error()})
		}
		return nil
	}
	v := &supervisionVerdict{}
	if act := strings.Index(out, "{"); act >= 0 {
		if end := strings.LastIndex(out, "}"); end > act {
			if e := json.Unmarshal([]byte(out[act:end+1]), v); e == nil {
				v.ParseOK = true
			}
		}
	}
	if !v.ParseOK {
		v.Passed = true // 审查输出不可解析时不阻塞主流程，仅记录
		v.Issues = "监察 Agent 输出无法解析：" + truncateRunes(out, 300)
	}
	if rec != nil {
		rec.event(EventSupervise, rec.turn, map[string]any{
			"agent":      member,
			"supervisor": sup.AgentKey,
			"passed":     v.Passed,
			"issues":     truncateRunes(v.Issues, 1000),
			"suggestion": truncateRunes(v.Suggest, 1000),
		})
	}
	return v
}

// RiskVerdict 风险评估结论
type RiskVerdict struct {
	Level  string `json:"level"` // low | medium | high
	Reason string `json:"reason"`
	Allow  bool   `json:"allow"`
}

// assessRisk 让风险评估 Agent 评估一次 MCP 工具调用的风险。
// 返回 nil 表示未启用风险评估（无 risk Agent / 未配置）。
// high 风险默认拦截；medium 放行并附带提示。
func (u *UseCase) assessRisk(ctx context.Context, parent *Task, riskDef *AgentDef, toolName string, args map[string]any, rec *Recorder) *RiskVerdict {
	if riskDef == nil || u.chat == nil {
		return nil
	}
	system := riskDef.System
	if system == "" {
		system = "你是操作风险评估 Agent，负责在调用外部工具前判断该操作的风险等级。"
	}
	system += `

## 输出协议（严格遵守）
只输出一个 JSON 对象，不要输出其他文字：
{"level":"low|medium|high","reason":"判定依据（一句话）","allow":true 或 false}

判定标准：
- low：只读查询、检索、列举等无副作用操作
- medium：有写入/修改但影响面可控，或涉及外部账号的普通操作
- high：删除、批量变更、转账支付、权限变更、对外发表等不可逆或高风险操作`
	payload := marshalPayload(args)
	user := fmt.Sprintf("## 待评估的工具调用\n工具: %s\n参数: %s", toolName, truncateRunes(payload, 3000))

	out, err := u.chatWith(ctx, riskDef, system, user)
	if err != nil {
		if rec != nil {
			rec.event(EventRiskAssess, rec.turn, map[string]any{"tool": toolName, "error": err.Error()})
		}
		return nil
	}
	v := &RiskVerdict{Level: "medium", Allow: true}
	if act := strings.Index(out, "{"); act >= 0 {
		if end := strings.LastIndex(out, "}"); end > act {
			_ = json.Unmarshal([]byte(out[act:end+1]), v)
		}
	}
	switch strings.ToLower(strings.TrimSpace(v.Level)) {
	case "low", "medium", "high":
		v.Level = strings.ToLower(strings.TrimSpace(v.Level))
	default:
		v.Level = "medium"
	}
	// high 一律拦截（即使模型给了 allow=true，以等级为准）
	v.Allow = v.Level != "high"
	if rec != nil {
		rec.event(EventRiskAssess, rec.turn, map[string]any{
			"tool":     toolName,
			"level":    v.Level,
			"allow":    v.Allow,
			"reason":   truncateRunes(v.Reason, 500),
			"assessor": riskDef.AgentKey,
		})
	}
	return v
}

// loadSupervisor / loadRiskDef 按 key 取角色定义（不存在或角色不符返回 nil）
func (u *UseCase) loadSupervisor(ctx context.Context, key string) *AgentDef {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	d, err := u.repo.GetDefByKey(ctx, key)
	if err != nil || d == nil || d.Role != RoleSupervisor || !d.Enabled {
		return nil
	}
	return d
}

func (u *UseCase) loadRiskDef(ctx context.Context, key string) *AgentDef {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	d, err := u.repo.GetDefByKey(ctx, key)
	if err != nil || d == nil || d.Role != RoleRisk || !d.Enabled {
		return nil
	}
	return d
}
