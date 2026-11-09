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
	"strconv"
	"strings"
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
	// StepState 计划步骤的独立状态（键 = 0-based 步骤下标）：
	// 并行波次里每个步骤完成即落盘，崩溃重启后按这张表跳过已完成的步骤，
	// 保证「已发生的副作用不被重复执行」；Done / StepIdx 只是它的一段连续前缀视图。
	StepState map[int]StepRecord `json:"step_state,omitempty"`

	// —— 编排图（flow DAG）——
	// NodeState 节点状态（nodeID → 状态 / 产出 / 条件分支）：
	// 崩溃重启后已完成的节点不再执行，产出直接复用。
	NodeState map[string]NodeRecord `json:"node_state,omitempty"`
	// EdgeState 边状态（边下标 → active/dead）。与 NodeState 同一次快照写入：
	// 只落节点不落边，重启后节点等不到"入边已决断"，整张图会卡在不能推进。
	EdgeState map[int]string `json:"edge_state,omitempty"`

	// —— 人工审批（O3 / P1-04）——
	// Approvals 已批准的「一次具体调用」：键 = ability + 参数指纹 + 策略版本，
	// 值为完整凭据（谁申请、谁批准、何时过期）。只按能力名放行会把一次审批
	// 变成整个工具的永久通行证，见 approval.go 的说明。
	Approvals map[string]ApprovalGrant `json:"approvals,omitempty"`

	// Cursor 最近一次 checkpoint 的恢复点描述（JSON 原文，路由恢复用）
	Cursor string `json:"cursor,omitempty"`
}

// newState 全新任务状态
func newState(goal string) *RunState {
	return &RunState{Goal: goal, Seen: map[string]string{}, Approvals: map[string]ApprovalGrant{},
		MemberOut: map[string]string{}, StepState: map[int]StepRecord{}}
}

// 计划步骤状态
const (
	stepRunning = "running" // 已派发、结果未知（崩溃后按"未完成"重跑）
	stepDone    = "done"    // 已完成：续跑时跳过，副作用只发生一次
	stepFailed  = "failed"  // 失败：走重规划
	stepPaused  = "paused"  // 挂在人工审批：续跑时重放该步骤（工具调用本身还没执行）
)

// StepRecord 单个计划步骤的执行状态
type StepRecord struct {
	Status string `json:"status"`
	// Output 步骤产出（续跑时作为 Done 的正文，不再调模型）
	Output string `json:"output,omitempty"`
	// Attempts 该步骤被执行过的次数（>1 说明重试/续跑过，排障用）
	Attempts int `json:"attempts,omitempty"`
}

// startStep 标记步骤开跑（Attempts 累加：崩溃后重跑的步骤 Attempts 会 >1）
func (st *RunState) startStep(idx int) {
	if st.StepState == nil {
		st.StepState = map[int]StepRecord{}
	}
	r := st.StepState[idx]
	r.Status = stepRunning
	r.Attempts++
	st.StepState[idx] = r
}

// finishStep 记录步骤结果。done 才带产出（失败/挂起的产出不参与后续步骤上下文）。
func (st *RunState) finishStep(idx int, status, out string) {
	if st.StepState == nil {
		st.StepState = map[int]StepRecord{}
	}
	r := st.StepState[idx]
	r.Status = status
	if status == stepDone {
		r.Output = out
	}
	st.StepState[idx] = r
}

// doneSteps 已完成步骤集合（0-based 下标）
func (st *RunState) doneSteps() map[int]bool {
	done := make(map[int]bool, len(st.StepState))
	for i, r := range st.StepState {
		if r.Status == stepDone {
			done[i] = true
		}
	}
	// 老 checkpoint 没有 StepState：按 StepIdx 连续前缀兜底（历史数据可继续续跑）
	if len(st.StepState) == 0 {
		for i := 0; i < st.StepIdx && i < len(st.Steps); i++ {
			done[i] = true
		}
	}
	return done
}

// promoteDoneSteps 把「连续完成」的步骤推进到 Done 前缀（幂等）。
//
// Done / StepIdx 是 StepState 的前缀视图：并行波次里步骤乱序完成时，只有前面的
// 步骤都完成了才把后面的产出并入 Done，保证「步骤 N 的上下文包含 1..N-1 的全部产出」。
// 重复调用（续跑、崩溃恢复）不会写出重复行。
func (st *RunState) promoteDoneSteps() {
	if st.StepIdx < 0 {
		st.StepIdx = 0
	}
	if len(st.Done) > st.StepIdx {
		st.Done = st.Done[:st.StepIdx]
	}
	for st.StepIdx < len(st.Steps) {
		r, ok := st.StepState[st.StepIdx]
		if !ok || r.Status != stepDone {
			return
		}
		line := fmt.Sprintf("步骤 %d（%s）：%s", st.StepIdx+1, st.Steps[st.StepIdx].Goal, r.Output)
		if st.StepIdx < len(st.Done) {
			st.Done[st.StepIdx] = line
		} else {
			st.Done = append(st.Done, line)
		}
		st.StepIdx++
	}
}

// clearStepsFrom 重规划后清掉从 idx 起的步骤状态：新计划替换了这一段，
// 旧的完成/失败记录不能留着，否则新步骤会被当成"已完成"直接跳过。
func (st *RunState) clearStepsFrom(idx int) {
	for i := range st.StepState {
		if i >= idx {
			delete(st.StepState, i)
		}
	}
	// 兼容老 checkpoint 的乱序完成表（step:<idx>）
	for k := range st.MemberOut {
		if !strings.HasPrefix(k, "step:") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(k, "step:")); err == nil && n >= idx {
			delete(st.MemberOut, k)
		}
	}
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

// drainStepOutputs 兼容老 checkpoint：把乱序完成记录（MemberOut["step:<idx>"]）
// 迁进 StepState。新版本只在 StepState 里记步骤状态，MemberOut 留给编排成员。
func (st *RunState) drainStepOutputs() {
	for k, out := range st.MemberOut {
		if !strings.HasPrefix(k, "step:") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(k, "step:"))
		if err != nil || idx < 0 {
			continue
		}
		delete(st.MemberOut, k)
		if _, exists := st.StepState[idx]; exists {
			continue // 新格式已有更准的状态
		}
		st.StepState[idx] = StepRecord{Status: stepDone, Output: out}
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
	if st.Approvals == nil {
		st.Approvals = map[string]ApprovalGrant{}
	}
	if st.MemberOut == nil {
		st.MemberOut = map[string]string{}
	}
	if st.StepState == nil {
		st.StepState = map[int]StepRecord{}
	}
	// 老格式（步骤产出记在 MemberOut["step:N"]）迁到 StepState
	st.drainStepOutputs()
	return &st
}

// gatedDef 该 Agent 是否受审批闸门管辖（决定工具循环是否逐轮落 checkpoint）
func gatedDef(def *AgentDef) bool {
	return humanGate(def)
}

// cloneForStep 计划步骤并发执行前的状态克隆：
// 保留计划级字段（Steps/Done/StepIdx/Approvals 深拷贝），工具循环级字段重置
// （每个步骤的循环从零开始，与重构前"每次 runToolLoop 全新局部变量"行为一致）
func (st *RunState) cloneForStep() *RunState {
	c := *st
	c.Digest = nil
	c.Recent = nil
	c.Seen = map[string]string{}
	c.InvalidCount = 0
	c.LoopTurn = 0
	c.Cursor = ""
	// 并发步骤各有独立的凭据表：共用同一 map 会在并发写入时直接 panic
	c.Approvals = copyApprovals(st.Approvals)
	// 计划级表同理深拷贝：步骤副本只承载自己的工具循环状态，
	// 落 checkpoint 时计划级字段以父状态为准（见 waveCommit.checkpoint）
	c.MemberOut = copyStringMap(st.MemberOut)
	c.StepState = copyStepState(st.StepState)
	return &c
}

// copyStepState 深拷贝步骤状态表
func copyStepState(in map[int]StepRecord) map[int]StepRecord {
	out := make(map[int]StepRecord, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// copyStringMap 深拷贝字符串表
func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
