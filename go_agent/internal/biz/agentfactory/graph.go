package agentfactory

// 编排图执行器：按 DAG 调度，串并行可混排。
//
// 执行模型（数据流）：
//   · 入度为 0 的节点以任务原始输入启动
//   · 节点在所有入边都有结论（激活 / 跳过）后进入就绪态
//   · 多入边 = 汇聚：把已激活上游的产出拼成输入
//   · 多出边 = 扇出：出边全部激活，下游并发执行（每一波一起跑）
//   · 条件节点只激活命中分支，未命中分支的下游整段跳过
//   · 输出节点负责最终汇总（交回根 Agent 合成）
//
// 子 Agent 仍走「建子任务 → 内联执行」，任务链路树与串并行编排保持一致；
// 事件记录（delegate / node）在并发波次里按顺序写，避免 Recorder 竞态。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// 边状态
const (
	edgeUndecided = iota
	edgeActive
	edgeDead
)

// 节点状态
const (
	nodeWait = iota
	nodeDone
	nodeSkipped
	// nodeFailed 节点失败 / 挂起：不再就绪，出边不放行（下游整段不执行）
	nodeFailed
)

// 节点状态的持久化形态。用字符串而不是 iota 数字：checkpoint 可能比代码活得久，
// 增删一个状态常量不该让老 checkpoint 被误读成另一个状态。
const (
	nodeStatusWait    = "wait"
	nodeStatusDone    = "done"
	nodeStatusSkipped = "skipped"
	nodeStatusFailed  = "failed"
)

func nodeStatusName(s int) string {
	switch s {
	case nodeDone:
		return nodeStatusDone
	case nodeSkipped:
		return nodeStatusSkipped
	case nodeFailed:
		return nodeStatusFailed
	default:
		return nodeStatusWait
	}
}

// nodeStatusFrom 解析持久化状态；未知值按「未执行」处理（宁可重跑一次）
func nodeStatusFrom(s string) int {
	switch s {
	case nodeStatusDone:
		return nodeDone
	case nodeStatusSkipped:
		return nodeSkipped
	case nodeStatusFailed:
		return nodeFailed
	default:
		return nodeWait
	}
}

// 边状态的持久化形态
const (
	edgeStatusActive = "active"
	edgeStatusDead   = "dead"
)

// nodePersistCap 节点产出写入 checkpoint 的上限（rune）。
// 下游拼输入时最多取 4000 runes，超过这个长度的部分对续跑没有意义。
const nodePersistCap = 8000

// NodeRecord 编排图节点的持久化状态
type NodeRecord struct {
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
	// Branch 条件节点的判定结果（nil = 不是条件节点）
	Branch *bool `json:"branch,omitempty"`
}

type graphRun struct {
	g        *FlowGraph
	nodeAt   map[string]int
	outEdges map[string][]int
	inEdges  map[string][]int
	edgeSt   []int
	nodeSt   []int
	outputs  map[string]string // nodeID → 文本产出
	taskIDs  map[string]uint64
	turns    int
}

// runFlowGraph 执行编排图。
//
// 续跑（O2）：RunState.NodeState / EdgeState 记录每个节点与边的决断结果。
// 已完成的节点不再执行、产出直接复用——否则重启一次就把已经发生的
// 外部副作用（发消息、建单、写库）再做一遍。落盘与节点结束在同一处：
// 节点少、波次内串行提交，不需要额外的并发控制。
func (u *UseCase) runFlowGraph(ctx context.Context, def *AgentDef, g *FlowGraph, input string, rec *Recorder, st *RunState) (*RunResult, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	r := &graphRun{
		g:        g,
		nodeAt:   make(map[string]int, len(g.Nodes)),
		outEdges: make(map[string][]int, len(g.Nodes)),
		inEdges:  make(map[string][]int, len(g.Nodes)),
		edgeSt:   make([]int, len(g.Edges)),
		nodeSt:   make([]int, len(g.Nodes)),
		outputs:  make(map[string]string, len(g.Nodes)),
		taskIDs:  make(map[string]uint64, len(g.Nodes)),
	}
	for i, n := range g.Nodes {
		r.nodeAt[n.ID] = i
	}
	for i, e := range g.Edges {
		r.outEdges[e.From] = append(r.outEdges[e.From], i)
		r.inEdges[e.To] = append(r.inEdges[e.To], i)
	}
	if st == nil {
		st = newState(input)
	}
	rec.st = st
	r.restore(st) // 已完成的节点直接复用产出，不再执行（副作用只发生一次）

	// save 落一份节点 + 边 + 产出的快照（同一次写入，避免"节点已完成、边未决断"的中间态）
	save := func(i int) {
		if st.NodeState == nil {
			st.NodeState = map[string]NodeRecord{}
		}
		if st.EdgeState == nil {
			st.EdgeState = map[int]string{}
		}
		node := g.Nodes[i]
		nr := st.NodeState[node.ID]
		nr.Status = nodeStatusName(r.nodeSt[i])
		// 落盘上限：下游拼输入时本来就截到 4000 runes，存全量只会让
		// 每个节点快照都背上几 KB 的历史产出（60 节点 × 每次快照）
		nr.Output = truncateRunes(r.outputs[node.ID], nodePersistCap)
		st.NodeState[node.ID] = nr
		for _, ei := range r.outEdges[node.ID] {
			switch r.edgeSt[ei] {
			case edgeActive:
				st.EdgeState[ei] = edgeStatusActive
			case edgeDead:
				st.EdgeState[ei] = edgeStatusDead
			}
		}
		rec.checkpoint(fmt.Sprintf(`{"kind":"node","node":%q,"status":%q}`, node.ID, nr.Status))
	}

	sup := u.loadSupervisor(ctx, def.SupervisorKey)
	remaining := 0
	for i := range g.Nodes {
		if r.nodeSt[i] == nodeWait {
			remaining++
		}
	}

	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("执行被取消: %w", err)
		}
		// 收集就绪节点
		ready := make([]int, 0, len(g.Nodes))
		for i := range g.Nodes {
			if r.nodeSt[i] != nodeWait {
				continue
			}
			resolved := true
			for _, ei := range r.inEdges[g.Nodes[i].ID] {
				if r.edgeSt[ei] == edgeUndecided {
					resolved = false
					break
				}
			}
			if resolved {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			// 剩余节点互相等待（历史脏数据成环）：安全退出
			return nil, fmt.Errorf("%w: 编排图无法推进，可能存在环或断链", ErrInvalidDef)
		}

		// 1) 没有激活入边的节点直接跳过，并把出边置死
		var wave []int
		for _, i := range ready {
			if r.hasActiveInput(i) {
				wave = append(wave, i)
				continue
			}
			r.nodeSt[i] = nodeSkipped
			r.resolveEdges(i, outSkipped, nil)
			remaining--
			save(i)
			rec.event(EventNode, rec.turn, map[string]any{"node": g.Nodes[i].ID, "kind": g.Nodes[i].Kind, "status": "skipped"})
		}

		// 2) 分出 Agent 节点与其它节点（Agent 先建子任务再并发跑）
		agentIdx := make([]int, 0, len(wave))
		otherIdx := make([]int, 0, len(wave))
		for _, i := range wave {
			if g.Nodes[i].Kind == FlowNodeAgent {
				agentIdx = append(agentIdx, i)
			} else {
				otherIdx = append(otherIdx, i)
			}
		}

		type prepared struct {
			idx    int
			task   *Task
			member *AgentDef
			input  string
			create error
		}
		preps := make([]prepared, len(agentIdx))
		for k, i := range agentIdx {
			node := g.Nodes[i]
			in := r.nodeInput(i, input)
			child, member, err := u.createChildTask(ctx, rec.task, node.AgentKey, in, rec)
			preps[k] = prepared{idx: i, task: child, member: member, input: in, create: err}
			if err != nil {
				rec.event(EventNode, rec.turn, map[string]any{"node": node.ID, "kind": node.Kind, "agent": node.AgentKey, "status": "error", "error": err.Error()})
			}
		}

		answers := make([]string, len(preps))
		errs := make([]error, len(preps))
		// 并发闸门：一个 20 节点的 DAG 不会同时拉起 20 个 Agent 烧 token，
		// 超出上限的节点排队等信号量（串行化但不改变 DAG 语义）。
		sem := make(chan struct{}, u.parallelLimit())
		var wg sync.WaitGroup
		for k := range preps {
			if preps[k].create != nil {
				continue
			}
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				answers[k], errs[k] = u.executeTaskNow(ctx, preps[k].task, preps[k].member)
			}(k)
		}
		wg.Wait()

		// 3) 落结果 + 监察（监察串行，避免并发打爆配额）
		for k, p := range preps {
			i := p.idx
			node := g.Nodes[i]
			if p.create != nil {
				// 子任务都没建起来：节点失败，出边只放行显式声明的 failure/always
				r.outputs[node.ID] = "[执行失败] " + p.create.Error()
				r.nodeSt[i] = nodeFailed
				r.resolveEdges(i, outFailed, nil)
				remaining--
				save(i)
				rec.event(EventNode, rec.turn, map[string]any{
					"node": node.ID, "kind": node.Kind, "agent": node.AgentKey,
					"status": "error", "error": p.create.Error(),
				})
				continue
			}
			ans := answers[k]
			if errs[k] != nil {
				// 子任务挂起等审批：节点既不算完成也不出边（下游不激活），
				// 整张图立刻停下。把它写成「节点完成 + [执行失败] 文本」会让
				// 下游带着残缺上下文继续跑，等于审批没批就把后续动作做了。
				if errors.Is(errs[k], ErrApprovalNeeded) {
					r.nodeSt[i] = nodeFailed
					// 挂起也落盘：重启后这个节点仍是 failed（不重跑、不激活下游），
					// 等人批的是子任务，父图不能自己往前跑
					save(i)
					rec.event(EventNode, rec.turn, map[string]any{
						"node": node.ID, "kind": node.Kind, "agent": node.AgentKey,
						"child_task_id": p.task.ID, "status": "paused",
						"error": "等待人工审批",
					})
					return nil, childApprovalError(p.task, node.AgentKey)
				}
				// 普通失败：节点标 failed（不是 done），默认出边全部置死。
				// 旧实现把失败伪装成「节点完成 + 失败文本」并激活所有下游，
				// 下游 Agent 会把上一跳的报错当成正常输入继续干活（甚至产生副作用）。
				ans = "[执行失败] " + errs[k].Error()
				r.outputs[node.ID] = ans
				if p.task != nil {
					r.taskIDs[node.ID] = p.task.ID
				}
				r.nodeSt[i] = nodeFailed
				r.resolveEdges(i, outFailed, nil)
				remaining--
				save(i)
				rec.event(EventNode, rec.turn, map[string]any{
					"node": node.ID, "kind": node.Kind, "agent": node.AgentKey,
					"child_task_id": p.task.ID, "status": "error",
					"error": truncateRunes(errs[k].Error(), 400),
				})
				continue
			}
			if sup != nil {
				if v := u.superviseMember(ctx, rec.task, sup, node.AgentKey, p.input, ans, rec); v != nil {
					if v.Passed {
						ans += "\n[监察] 通过"
					} else {
						ans += fmt.Sprintf("\n[监察] 未通过：%s；建议：%s", truncateRunes(v.Issues, 300), truncateRunes(v.Suggest, 200))
					}
				}
			}
			r.outputs[node.ID] = ans
			r.taskIDs[node.ID] = p.task.ID
			r.turns++
			r.nodeSt[i] = nodeDone
			r.resolveEdges(i, outSuccess, nil)
			remaining--
			save(i)
			rec.event(EventNode, rec.turn, map[string]any{
				"node": node.ID, "kind": node.Kind, "agent": node.AgentKey,
				"child_task_id": p.task.ID, "status": "done",
				"output": truncateRunes(ans, 600),
			})
		}

		// 4) 条件 / 输出节点（轻量，串行执行）
		for _, i := range otherIdx {
			node := g.Nodes[i]
			in := r.nodeInput(i, input)
			switch node.Kind {
			case FlowNodeCondition:
				pass, cerr := u.evalCondition(ctx, node.Cond, in)
				if cerr != nil {
					// 判断失败不等于「条件不成立」：节点标失败、出边按失败语义
					// 决断（只有 when=failure/always 的边放行），不猜分支。
					r.outputs[node.ID] = "[执行失败] 条件判断失败：" + cerr.Error()
					r.nodeSt[i] = nodeFailed
					r.resolveEdges(i, outFailed, nil)
					save(i)
					rec.event(EventNode, rec.turn, map[string]any{
						"node": node.ID, "kind": node.Kind, "status": "error",
						"error": truncateRunes(cerr.Error(), 400),
					})
					break
				}
				branch := "false"
				if pass {
					branch = "true"
				}
				r.outputs[node.ID] = in // 条件节点透传上游，供分支下游使用
				r.nodeSt[i] = nodeDone
				r.resolveEdges(i, outSuccess, &pass)
				if st.NodeState == nil {
					st.NodeState = map[string]NodeRecord{}
				}
				nr := st.NodeState[node.ID]
				nr.Branch = &pass // 判定结果入档：重启后能回答"走了哪条分支"
				st.NodeState[node.ID] = nr
				save(i)
				rec.event(EventNode, rec.turn, map[string]any{
					"node": node.ID, "kind": node.Kind, "status": "condition",
					"branch": branch, "input": truncateRunes(in, 400),
				})
			case FlowNodeOutput:
				ans, err := u.graphFinalize(ctx, def, input, in)
				if err != nil {
					return nil, err
				}
				r.outputs[node.ID] = ans
				r.nodeSt[i] = nodeDone
				r.resolveEdges(i, outSuccess, nil)
				save(i)
				r.turns++
				rec.event(EventNode, rec.turn, map[string]any{"node": node.ID, "kind": node.Kind, "status": "done"})
			default:
				r.nodeSt[i] = nodeSkipped
				r.resolveEdges(i, outSkipped, nil)
				save(i)
			}
			remaining--
		}
	}

	// 输出节点产出即最终答案；没有到达输出节点（条件全跳过）时退回汇总全部产出
	final := r.outputs[r.outputNode()]
	if strings.TrimSpace(final) == "" {
		final = r.fallbackAnswer(def, input)
	}
	if strings.TrimSpace(final) == "" {
		return nil, fmt.Errorf("编排执行完成但没有产生输出")
	}
	return &RunResult{Answer: final, Turns: r.turns + 1}, nil
}

// restore 把 checkpoint 里的节点 / 边状态装回运行时，返回已完成（不必再跑）的节点数。
//
// 语义按状态分三种：
//
//	done / skipped  复用：副作用已经发生（或分支已判定），不能再做一遍
//	failed          重试：失败的节点在重新提交时应该再跑一次，同时清掉它上次
//	                留下的出边判定——否则重试成功了，下游还因为"边已置死"不启动
//	wait            不动
//
// 只认「节点 ID 还在、边下标还在范围内」的记录：编排图被改过（删节点、重连线）时，
// 老 checkpoint 直接忽略，宁可整张图重跑，也不能拿着对不上的状态继续。
func (r *graphRun) restore(st *RunState) int {
	if st == nil || len(st.NodeState) == 0 {
		return 0
	}
	done := 0
	for i, n := range r.g.Nodes {
		rec, ok := st.NodeState[n.ID]
		if !ok {
			continue
		}
		status := nodeStatusFrom(rec.Status)
		switch status {
		case nodeWait:
			continue
		case nodeFailed:
			delete(st.NodeState, n.ID)
			for _, ei := range r.outEdges[n.ID] {
				delete(st.EdgeState, ei)
				r.edgeSt[ei] = edgeUndecided
			}
			continue
		}
		r.nodeSt[i] = status
		r.outputs[n.ID] = rec.Output
		if status == nodeDone {
			done++
		}
	}
	for ei, s := range st.EdgeState {
		if ei < 0 || ei >= len(r.edgeSt) {
			continue
		}
		switch s {
		case edgeStatusActive:
			r.edgeSt[ei] = edgeActive
		case edgeStatusDead:
			r.edgeSt[ei] = edgeDead
		}
	}
	return done
}

// hasActiveInput 是否存在已激活入边；无入边的入口节点视作激活
func (r *graphRun) hasActiveInput(i int) bool {
	ins := r.inEdges[r.g.Nodes[i].ID]
	if len(ins) == 0 {
		return true
	}
	for _, ei := range ins {
		if r.edgeSt[ei] == edgeActive {
			return true
		}
	}
	return false
}

// nodeInput 汇聚上游产出；入口节点用任务原始输入
func (r *graphRun) nodeInput(i int, input string) string {
	ins := r.inEdges[r.g.Nodes[i].ID]
	if len(ins) == 0 {
		return input
	}
	type part struct{ key, text string }
	parts := make([]part, 0, len(ins))
	for _, ei := range ins {
		if r.edgeSt[ei] != edgeActive {
			continue
		}
		from := r.g.Edges[ei].From
		text := strings.TrimSpace(r.outputs[from])
		if text == "" {
			continue
		}
		key := from
		for _, n := range r.g.Nodes {
			if n.ID == from {
				if n.AgentKey != "" {
					key = n.AgentKey
				} else if n.Label != "" {
					key = n.Label
				}
				break
			}
		}
		parts = append(parts, part{key: key, text: text})
	}
	if len(parts) == 0 {
		return input
	}
	if len(parts) == 1 {
		// 单上游：保留任务上下文 + 上游产出
		return fmt.Sprintf("%s\n\n## 上游产出（%s）\n%s", input, parts[0].key, truncateRunes(parts[0].text, 4000))
	}
	var b strings.Builder
	b.WriteString(input)
	b.WriteString("\n\n## 并行上游产出\n")
	for _, p := range parts {
		fmt.Fprintf(&b, "\n### %s\n%s\n", p.key, truncateRunes(p.text, 3000))
	}
	return b.String()
}

// nodeOutcome 节点结束状态：决定出边怎么决断
type nodeOutcome int

const (
	outSuccess nodeOutcome = iota
	outFailed
	outSkipped
)

// resolveEdges 出边决断。
//
// 语义刻意收紧（P1-06）：只有「成功」才默认放行下游。旧实现无论节点是
// 完成、失败还是跳过，都调用 resolveEdges(i, nil) 把全部出边置为激活——
// 于是一个 Agent 失败后，下游 Agent 会把 "[执行失败] ..." 这段文本当成
// 正常上游输入继续干活，失败因此被静默吞掉。
//
//	成功        When 为空 / success / always → 放行；条件节点按 true/false 分支
//	失败        只放行显式声明的 failure / always 边，其余置死
//	跳过        全部置死（未命中的分支整段不执行）
//
// 「失败也要继续」必须写成 when: "failure" 或 "always"，把意图显式化。
func (r *graphRun) resolveEdges(i int, outcome nodeOutcome, branch *bool) {
	node := r.g.Nodes[i]
	for _, ei := range r.outEdges[node.ID] {
		e := r.g.Edges[ei]
		when := strings.ToLower(strings.TrimSpace(e.When))
		if when == "" {
			if branch != nil {
				when = "true" // 条件节点的默认出边算「命中分支」
			} else {
				when = "success"
			}
		}
		active := false
		switch outcome {
		case outSuccess:
			switch when {
			case "success", "always":
				active = true
			case "true":
				active = branch == nil || *branch
			case "false":
				active = branch != nil && !*branch
			}
		case outFailed:
			active = when == "failure" || when == "always"
		case outSkipped:
			active = false
		}
		if active {
			r.edgeSt[ei] = edgeActive
		} else {
			r.edgeSt[ei] = edgeDead
		}
	}
}

// outputNode 唯一输出节点 id
func (r *graphRun) outputNode() string {
	for _, n := range r.g.Nodes {
		if n.Kind == FlowNodeOutput {
			return n.ID
		}
	}
	return ""
}

// evalCondition 条件判断：规则本地算，LLM 模式走模型判定。
//
// 判断失败必须显式返回错误：条件节点决定「哪条分支被激活」，把失败当成
// false 会让未命中分支照常执行（等于猜了一个答案还继续跑），
// 调用方据此把节点标 failed、出边按失败语义决断。
func (u *UseCase) evalCondition(ctx context.Context, c *FlowCond, input string) (bool, error) {
	if c == nil {
		return true, nil
	}
	if c.Mode != CondModeLLM {
		return evalCond(c, input)
	}
	if u.chat == nil {
		return false, fmt.Errorf("条件节点配置为模型判断，但当前没有可用模型")
	}
	question := strings.TrimSpace(c.Question)
	if question == "" {
		return false, fmt.Errorf("条件节点配置为模型判断，但缺少判断问题")
	}
	system := `你是流程编排里的条件判断器。根据给定材料回答一个问题，只输出 YES 或 NO，不要任何解释。`
	user := fmt.Sprintf("## 判断问题\n%s\n\n## 材料\n%s", question, truncateRunes(input, 6000))
	ans, err := u.chat.ChatTemp(ctx, system, user, 0)
	if err != nil {
		return false, fmt.Errorf("条件判断调用模型失败: %w", err)
	}
	pass, ok := parseYesNo(ans)
	if !ok {
		return false, fmt.Errorf("条件判断未得到明确结论（模型输出：%s）", truncateRunes(strings.TrimSpace(ans), 80))
	}
	return pass, nil
}

// parseYesNo 解析模型的是/否判断；无法判定时 ok=false（由调用方转成显式错误）。
//
// 两种结论同时出现（"YES 也可以是 NO"）按无法判定处理：条件节点决定走哪条分支，
// 拿不准的时候必须报错让人看，不能挑一个更容易解析的当结论。
func parseYesNo(ans string) (bool, bool) {
	up := strings.ToUpper(strings.TrimSpace(ans))
	if up == "" {
		return false, false
	}
	hasYes := strings.Contains(up, "YES") || strings.Contains(up, "是")
	hasNo := strings.Contains(up, "NO") || strings.Contains(up, "否")
	if hasYes && hasNo {
		return false, false
	}
	switch {
	case strings.HasPrefix(up, "YES"), strings.HasPrefix(up, "是"):
		return true, true
	case strings.HasPrefix(up, "NO"), strings.HasPrefix(up, "否"), strings.HasPrefix(up, "不是"):
		return false, true
	case hasYes:
		return true, true
	case hasNo:
		return false, true
	}
	return false, false
}

// graphFinalize 输出节点：交回根 Agent 汇总成面向用户的答案
func (u *UseCase) graphFinalize(ctx context.Context, def *AgentDef, input, upstream string) (string, error) {
	system := def.System
	if system == "" {
		system = "你是多 Agent 协作的根 Agent「" + def.Name + "」。"
	}
	system += `

## 汇总要求
下面是编排链路汇总上来的产出（含并行分支与监察意见）。请综合成最终答案：
1. 直接输出面向用户的结论，不要罗列过程，不要输出 JSON
2. 分支之间冲突时说明取舍理由；失败分支如实说明，不要编造
3. 内容较多时用简洁的分点结构`
	user := fmt.Sprintf("## 任务\n%s\n\n## 汇总材料\n%s", input, truncateRunes(upstream, 12000))
	return u.chatWith(ctx, def, system, user)
}

// fallbackAnswer 未到达输出节点时的兜底：拼接所有已执行节点产出
func (r *graphRun) fallbackAnswer(def *AgentDef, input string) string {
	var b strings.Builder
	b.WriteString("## 任务\n")
	b.WriteString(input)
	b.WriteString("\n\n## 已执行节点产出\n")
	for _, n := range r.g.Nodes {
		text := strings.TrimSpace(r.outputs[n.ID])
		if text == "" {
			continue
		}
		label := n.AgentKey
		if label == "" {
			label = n.Label
		}
		if label == "" {
			label = n.ID
		}
		fmt.Fprintf(&b, "\n### %s\n%s\n", label, truncateRunes(text, 2000))
	}
	return b.String()
}
