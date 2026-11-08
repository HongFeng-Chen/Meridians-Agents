package agentfactory

// RunState 任务执行的显式状态（O1）：
// 把原先散落在 runToolLoop / runPlanExecute / runWorkflow 等函数里的局部变量
// 收拢成一个可 JSON 序列化的结构，作为 checkpoint（O2）、人工审批（O3）、
// 波次并行（O4）与未来的重放（O7）共同的状态载体。
//
// 设计原则：状态即真相——checkpoint 直接序列化 RunState，恢复时反序列化回放；
// task_events 继续只做观测（trace），不参与状态重建。

import (
	"encoding/json"
	"fmt"
	"strings"

	"go_agent/internal/biz/mcp"
)

// RunState 一次任务执行的显式状态
type RunState struct {
	// Goal 任务原始输入（不可变，冗余存储便于独立回放）
	Goal string `json:"goal"`
	// Turn 全局轮次计数（与 Recorder.turn 同步，事件标号用）
	Turn int `json:"turn"`

	// —— 工具循环状态（runToolLoop；react / plan 单步执行 / reflexion 等共用）——
	// Digest 早期轮次的一句话摘要（滚动压缩，控制 transcript 线性增长）
	Digest []string `json:"digest,omitempty"`
	// Recent 最近 keepRecentTurns 轮的全文段
	Recent []string `json:"recent,omitempty"`
	// Seen 同任务内工具调用去重表（ability|argsKey → 轮次标注）
	Seen map[string]string `json:"seen,omitempty"`
	// InvalidCount 模型输出不合规的连续次数
	InvalidCount int `json:"invalid_count,omitempty"`
	// LoopTurn 工具循环内的轮序号（1-based；恢复时从 LoopTurn+1 继续）
	LoopTurn int `json:"loop_turn,omitempty"`

	// —— 计划类状态（plan_execute / orchestrator / hierarchical）——
	// Steps 当前生效的计划（replan 会整体替换剩余部分）
	Steps []planStep `json:"steps,omitempty"`
	// Done 已完成步骤的人类可读产出（「步骤 N（目标）：结果」）
	Done []string `json:"done,omitempty"`
	// StepIdx 下一个待执行的步骤下标（0-based；用于断点续跑）
	StepIdx int `json:"step_idx,omitempty"`
	// Replans 已触发重规划次数（上限 2）
	Replans int `json:"replans,omitempty"`

	// —— 波次 / 轮次（workflow / debate / blackboard）——
	// Wave 已完成的波次号（workflow 并行）
	Wave int `json:"wave,omitempty"`
	// Round 已完成的辩论/黑板轮次
	Round int `json:"round,omitempty"`
	// Proposal 辩论当前的最新方案（正方产出）
	Proposal string `json:"proposal,omitempty"`
	// Critique 反方最后意见
	Critique string `json:"critique,omitempty"`
	// Board 黑板内容（blackboard）
	Board string `json:"board,omitempty"`
	// MemberOut 成员产出（workflow / flow：agent_key → 最终答案，续跑时复用）
	MemberOut map[string]string `json:"member_out,omitempty"`

	// —— 人工审批（O3）——
	// Approved 已批准的能力集合：审批通过后写入，续跑时跳过闸门
	Approved map[string]bool `json:"approved,omitempty"`

	// Cursor 最近一次 checkpoint 的恢复点描述（JSON 原文，路由恢复用）
	Cursor string `json:"cursor,omitempty"`
}

// newState 全新任务状态
func newState(goal string) *RunState {
	return &RunState{Goal: goal, Seen: map[string]string{}, Approved: map[string]bool{}, MemberOut: map[string]string{}}
}

// cursorKind 恢复点类型：plan_step | wave | round | loop | approval | ""
func (st *RunState) cursorKind() string {
	var c struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(st.Cursor), &c); err != nil {
		return ""
	}
	return c.Kind
}

// approved 某能力是否已人工批准
func (st *RunState) approved(ability string) bool {
	return st.Approved[ability]
}

// markApproved 记录一次人工批准
func (st *RunState) markApproved(ability string) {
	if st.Approved == nil {
		st.Approved = map[string]bool{}
	}
	st.Approved[ability] = true
}

// drainStepOutputs 续跑对齐：把乱序完成的步骤产出（MemberOut["step:<idx>"]）
// 按序搬进 Done，推进 StepIdx（runPlanExecute 用）
func (st *RunState) drainStepOutputs() {
	for {
		key := fmt.Sprintf("step:%d", st.StepIdx)
		out, ok := st.MemberOut[key]
		if !ok {
			return
		}
		delete(st.MemberOut, key)
		idx := st.StepIdx
		goal := ""
		if idx < len(st.Steps) {
			goal = st.Steps[idx].Goal
		}
		st.Done = append(st.Done, fmt.Sprintf("步骤 %d（%s）：%s", idx+1, goal, out))
		st.StepIdx++
	}
}

// encodeState 序列化（失败返回空串，调用方按无 checkpoint 处理）
func encodeState(st *RunState) string {
	if st == nil {
		return ""
	}
	b, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeState 反序列化（损坏的 checkpoint 按"无状态"处理，任务从头执行——
// 宁可多花 token 也不因脏数据卡死任务）
func decodeState(s string) *RunState {
	if s == "" {
		return nil
	}
	var st RunState
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return nil
	}
	if st.Seen == nil {
		st.Seen = map[string]string{}
	}
	if st.Approved == nil {
		st.Approved = map[string]bool{}
	}
	if st.MemberOut == nil {
		st.MemberOut = map[string]string{}
	}
	return &st
}

// gatedDef 该 Agent 是否受审批闸门管辖（决定工具循环是否逐轮落 checkpoint）
func gatedDef(def *AgentDef) bool {
	return len(def.RequireApproval) > 0 || strings.EqualFold(def.RiskPauseLevel, "high")
}

// needApproval 能力是否需要人工审批，两条规则叠加：
//
//  1. require_approval 前缀匹配（尾部 * 通配）——人显式声明的清单；
//  2. **静态风险闸门**：mcp: 前缀的能力若按工具名判定为高危（delete/drop/exec…），
//     即便没写进 require_approval 也要审批。理由：Agent 的定义由 LLM 生成，
//     指望它主动把每个高危工具都列进审批清单不现实；漏写的代价是一条 DELETE
//     直接打到生产库。这条规则让「没声明」变成「默认需要人点一下」，
//     误判的代价只是多点一次审批。
//
// 已批准过的能力直接放行（审批结果持久化在 RunState，续跑不重复问）。
func needApproval(def *AgentDef, st *RunState, ability string) bool {
	if st != nil && st.approved(ability) {
		return false
	}
	if strings.HasPrefix(ability, CapToolPrefix) {
		tool := strings.TrimPrefix(ability, CapToolPrefix)
		if mcp.ClassifyToolRisk(mcp.Tool{ToolName: tool}) == mcp.RiskDestructive {
			return true
		}
	}
	if def == nil || len(def.RequireApproval) == 0 {
		return false
	}
	for _, p := range def.RequireApproval {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(ability, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if p == ability {
			return true
		}
	}
	return false
}

// approvalCursor 审批挂起点的恢复游标
func approvalCursor(ability string) string {
	b, _ := json.Marshal(map[string]string{"kind": "approval", "ability": ability})
	return string(b)
}

// cursorAbility 从恢复游标中取审批能力名（非 approval 游标返回空）
func cursorAbility(cursor string) string {
	var c struct {
		Ability string `json:"ability"`
	}
	if err := json.Unmarshal([]byte(cursor), &c); err != nil {
		return ""
	}
	return c.Ability
}

// cloneForStep 计划步骤并发执行前的状态克隆：
// 保留计划级字段（Steps/Done/StepIdx/Approved），工具循环级字段重置
// （每个步骤的循环从零开始，与重构前"每次 runToolLoop 全新局部变量"行为一致）
func (st *RunState) cloneForStep() *RunState {
	c := *st
	c.Digest = nil
	c.Recent = nil
	c.Seen = map[string]string{}
	c.InvalidCount = 0
	c.LoopTurn = 0
	c.Cursor = ""
	return &c
}
