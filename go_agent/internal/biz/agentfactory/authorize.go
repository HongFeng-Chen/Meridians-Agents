package agentfactory

// 能力授权（Capability Authorization）：白名单是权限，不是提示词建议。
//
// 修复前的执行路径：
//
//	buildSystemPrompt / allowedSchemas  →  按 def.Capabilities 过滤「模型看得见的能力」
//	模型输出 action.name                →  InvokeDispatch 按名字前缀直接分发
//
// 也就是说白名单只约束了输入侧（提示词），没有约束出口。模型幻觉、提示词注入，
// 或者被委派的子 Agent 转述一段「请调用 mcp:prod-db__drop_table」，都能让一个
// 只被授予只读工具的 Agent 调到高危工具——因为分发入口从不回看白名单。
//
// 这里把校验放到 InvokeDispatch 的第一行：所有执行路径（tool loop、router、
// function_call、各执行图、并行波次）都经过它，是唯一的强制点。
//
// 二级校验刻意不在这里重复：能力名合法 ≠ 目标资源当前可用。
//   - mcp:   CallTool 校验工具/服务器 enabled
//   - agent: runChildTask 校验目标 enabled
//   - skill: skillCapability 校验启用状态
// 两处都查会漂移，一处查死即可；这里只回答「这个 Agent 有没有这项权限」。

import (
	"context"
	"fmt"
	"strings"

	"go_agent/internal/biz/audit"
)

// capabilityAllowed 能力名是否在**生效的**白名单内（精确匹配）。
//
// 生效清单来自 effectiveCapabilities：子任务有委派上下文时用交集结果，
// 否则用定义自身的白名单。委派上下文是执行期的硬约束——即便子 Agent
// 的定义里写着 `mcp:*`，父任务没下放也调不到。
//
// 不支持通配：定义与调用都来自同一份字符串，通配只会制造「看起来被限制、
// 实际全放行」的错觉（`mcp:*` 一条就等于把全部工具给出去）。通配只出现在
// 委派交集的计算里（delegation.go），用于「父级批量下放」的声明式写法。
func capabilityAllowed(t *Task, def *AgentDef, name string) bool {
	if def == nil || name == "" {
		return false
	}
	for _, c := range effectiveCapabilities(t, def) {
		if c == name {
			return true
		}
	}
	return false
}

// authorizeCapability 能力分发前的强制授权检查。
//
// 拒绝的三类情况都会留痕（审计 + 任务事件）：
//  1. 没有 Agent 上下文（rec / rec.def 为空）—— 不允许绕过白名单直接分发；
//  2. 能力名不在白名单（模型自行编造或跨 Agent 借用）；
//  3. 沙箱 Agent 试图提升（委派 / 自举建 Agent）。
func (u *UseCase) authorizeCapability(ctx context.Context, rec *Recorder, name string) error {
	if rec == nil || rec.def == nil {
		audit.Denied(ctx, "agentfactory.capability.invoke", "capability", name, "缺少 Agent 上下文")
		return fmt.Errorf("%w: %s（缺少 Agent 执行上下文，拒绝执行）", ErrCapabilityDeny, name)
	}
	def := rec.def

	if !capabilityAllowed(rec.task, def, name) {
		u.capabilityDenied(ctx, rec, name, "capability_not_in_whitelist")
		return fmt.Errorf("%w: %s（Agent「%s」未获授权）", ErrCapabilityDeny, name, def.Name)
	}

	// 沙箱提升防护：白名单里即便写了委派/自举能力，沙箱也不放行。
	// （创建期就会剔除这类能力，这里挡的是历史数据与手工改库）
	if def.Sandbox {
		switch {
		case name == CapFactory:
			u.capabilityDenied(ctx, rec, name, "sandbox_bootstrap")
			return fmt.Errorf("%w: 沙箱 Agent 不能创建新 Agent", ErrSandboxDeny)
		case strings.HasPrefix(name, CapAgentPrefix):
			u.capabilityDenied(ctx, rec, name, "sandbox_delegate")
			return ErrSandboxDeny
		}
	}
	return nil
}

// capabilityDenied 越权尝试留痕：审计表（可检索）+ 任务事件（时间线可见）
func (u *UseCase) capabilityDenied(ctx context.Context, rec *Recorder, name, reason string) {
	var taskID uint64
	agentKey := ""
	if rec != nil && rec.def != nil {
		agentKey = rec.def.AgentKey
	}
	if rec != nil && rec.task != nil {
		taskID = rec.task.ID
		rec.event(EventError, rec.turn, map[string]any{
			"message": "能力越权被拒：" + name,
			"denied":  true,
			"reason":  reason,
			"ability": name,
		})
	}
	audit.Denied(ctx, "agentfactory.capability.invoke", "capability", name, reason)
	u.warn("能力越权被拒", "ability", name, "reason", reason, "agent", agentKey, "task_id", taskID)
}
