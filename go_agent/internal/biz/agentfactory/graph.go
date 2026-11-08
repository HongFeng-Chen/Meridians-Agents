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

// runFlowGraph 执行编排图
func (u *UseCase) runFlowGraph(ctx context.Context, def *AgentDef, g *FlowGraph, input string, rec *Recorder) (*RunResult, error) {
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

	sup := u.loadSupervisor(ctx, def.SupervisorKey)
	remaining := len(g.Nodes)

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
			r.resolveEdges(i, nil)
			remaining--
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
				r.outputs[node.ID] = "[执行失败] " + p.create.Error()
				r.nodeSt[i] = nodeDone
				r.resolveEdges(i, nil)
				remaining--
				continue
			}
			ans := answers[k]
			if errs[k] != nil {
				// 子任务挂起等审批：节点既不算完成也不出边（下游不激活），
				// 整张图立刻停下。把它写成「节点完成 + [执行失败] 文本」会让
				// 下游带着残缺上下文继续跑，等于审批没批就把后续动作做了。
				if errors.Is(errs[k], ErrApprovalNeeded) {
					r.nodeSt[i] = nodeFailed
					rec.event(EventNode, rec.turn, map[string]any{
						"node": node.ID, "kind": node.Kind, "agent": node.AgentKey,
						"child_task_id": p.task.ID, "status": "paused",
						"error": "等待人工审批",
					})
					return nil, childApprovalError(p.task, node.AgentKey)
				}
				ans = "[执行失败] " + errs[k].Error()
			} else if sup != nil {
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
			r.resolveEdges(i, nil)
			remaining--
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
				pass := u.evalCondition(ctx, node.Cond, in)
				branch := "false"
				if pass {
					branch = "true"
				}
				r.outputs[node.ID] = in // 条件节点透传上游，供分支下游使用
				r.nodeSt[i] = nodeDone
				r.resolveEdges(i, &pass)
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
				r.resolveEdges(i, nil)
				r.turns++
				rec.event(EventNode, rec.turn, map[string]any{"node": node.ID, "kind": node.Kind, "status": "done"})
			default:
				r.nodeSt[i] = nodeSkipped
				r.resolveEdges(i, nil)
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

// resolveEdges 出边放行：nil = 全部激活；条件节点按命中分支放行
func (r *graphRun) resolveEdges(i int, branch *bool) {
	node := r.g.Nodes[i]
	for _, ei := range r.outEdges[node.ID] {
		e := r.g.Edges[ei]
		if branch == nil {
			r.edgeSt[ei] = edgeActive
			continue
		}
		want := e.When
		if want == "" {
			want = "true"
		}
		if (*branch && want == "true") || (!*branch && want == "false") {
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

// evalCondition 条件判断：规则本地算，LLM 模式走模型判定
func (u *UseCase) evalCondition(ctx context.Context, c *FlowCond, input string) bool {
	if c == nil {
		return true
	}
	if c.Mode == CondModeLLM && u.chat == nil {
		return evalCond(c, input)
	}
	if c.Mode != CondModeLLM {
		return evalCond(c, input)
	}
	question := strings.TrimSpace(c.Question)
	if question == "" {
		question = "上游产出是否满足任务要求？"
	}
	system := `你是流程编排里的条件判断器。根据给定材料回答一个问题，只输出 YES 或 NO，不要任何解释。`
	user := fmt.Sprintf("## 判断问题\n%s\n\n## 材料\n%s", question, truncateRunes(input, 6000))
	ans, err := u.chat.ChatTemp(ctx, system, user, 0)
	if err != nil {
		return false
	}
	up := strings.ToUpper(strings.TrimSpace(ans))
	return strings.Contains(up, "YES") || strings.Contains(up, "是")
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
