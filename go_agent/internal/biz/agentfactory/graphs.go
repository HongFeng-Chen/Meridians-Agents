package agentfactory

// 执行图（Agent 架构模式）实现。
//
// 所有模式共用两件基础设施：runToolLoop（工具循环）与 runChildTask（委派子 Agent），
// 差别只在「谁来决定下一步」：
//
//	plan_execute  计划驱动：先出完整计划，再逐步执行，步骤失败重规划
//	reflexion     自评驱动：执行 → 评估 → 反思 → 带教训重试
//	router        分类驱动：先判意图，再路由到技能/成员，不做自由推理
//	self_ask      子问题驱动：拆子问题 → 逐个回答 → 汇总
//	function_call 工具驱动：无思考文本，一步选工具再作答
//	state_machine 状态驱动：固定状态流转，模型只做状态判断
//	orchestrator  协调器：总指挥派活给成员并汇总
//	hierarchical  分层：规划器拆子目标，成员各自细化执行
//	debate        辩论：正方生成 / 反方挑错，多轮后收敛
//	blackboard    黑板：成员围绕同一块共享状态板读写推进

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// hardDispatchErr 分发错误是否属于「必须停下」的状态。
// 审批挂起（本任务 / 子任务）不能折成观察文本喂回模型：模型会顺手换个
// 参数或换个工具重试，等于用一条没被批准的路把事做掉。
func hardDispatchErr(err error) bool {
	return errors.Is(err, ErrApprovalNeeded) || errors.Is(err, ErrChildApprovalPending)
}

// planStep 计划中的一步；Agent 为空表示由当前 Agent 自己执行
type planStep struct {
	Goal  string `json:"goal"`
	Agent string `json:"agent"`
	// DependsOn 依赖的步骤序号（1-based，指向 steps 数组下标+1）：
	// 空 = 依赖前一步（保守串行）；波次调度（O4）按依赖分层并发
	DependsOn []int `json:"depends_on,omitempty"`
}

// evalVerdict 反思型 Agent 的自评结果
type evalVerdict struct {
	Pass   bool   `json:"pass"`
	Score  int    `json:"score"`
	Issues string `json:"issues"`
}

// stateDecision 状态机的状态判断
type stateDecision struct {
	State string `json:"state"`
	Note  string `json:"note"`
	Final string `json:"final"`
}

// funcCallDecision 原生工具调用的一次决策
type funcCallDecision struct {
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Answer string         `json:"answer"`
}

// routeDecision 路由决策
type routeDecision struct {
	Target string         `json:"target"`
	Args   map[string]any `json:"args"`
	Reason string         `json:"reason"`
}

// —— 通用小工具 ——

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// memberKeys 编排成员（去掉空值与自身，防止自调用）
func (u *UseCase) memberKeys(def *AgentDef) []string {
	out := make([]string, 0, len(def.Members))
	for _, m := range def.Members {
		m = strings.TrimSpace(m)
		if m == "" || m == def.AgentKey || containsStr(out, m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// allowedSchemas 白名单内的能力（提示词与路由候选共用）
func (u *UseCase) allowedSchemas(ctx context.Context, def *AgentDef) ([]CapabilitySchema, error) {
	all, err := u.catalog.Schemas(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(def.Capabilities))
	for _, c := range def.Capabilities {
		allowed[c] = struct{}{}
	}
	out := make([]CapabilitySchema, 0, len(all))
	for _, s := range all {
		if _, ok := allowed[s.Name]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func capabilityLines(schemas []CapabilitySchema) string {
	var b strings.Builder
	for _, s := range schemas {
		fmt.Fprintf(&b, "- %s（%s）：%s\n", s.Name, s.Type, truncateRunes(s.Description, 160))
	}
	return b.String()
}

// finish 记最终答案事件并返回结果
func (u *UseCase) finish(rec *Recorder, answer string) *RunResult {
	rec.event(EventFinal, rec.turn, map[string]any{"answer": answer})
	return &RunResult{Answer: answer, Turns: rec.turn}
}

// delegate 委派给成员 Agent 执行（复用任务链路，trace 里能看到父子任务）
func (u *UseCase) delegate(ctx context.Context, rec *Recorder, agentKey, input string) (string, error) {
	rec.turn++
	_, ans, err := u.runChildTask(ctx, rec.task, agentKey, input, rec)
	return ans, err
}

// stepSeed 单个步骤的输入：总任务 + 已完成步骤 + 当前步骤
func (u *UseCase) stepSeed(input string, done []string, goal string) string {
	var b strings.Builder
	b.WriteString("## 总任务\n")
	b.WriteString(input)
	b.WriteString("\n\n")
	if len(done) > 0 {
		b.WriteString("## 已完成步骤\n")
		for _, d := range done {
			b.WriteString("- ")
			b.WriteString(truncateRunes(d, 800))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("## 当前步骤\n")
	b.WriteString(goal)
	b.WriteString("\n\n只完成当前这一步，不要越界做后续步骤。信息足够就直接给出 final。\n\n## 执行记录\n")
	return b.String()
}

// —— 规划相关（plan_execute / orchestrator / hierarchical 共用）——

func (u *UseCase) plannerPrompt(def *AgentDef, extra string) string {
	var b strings.Builder
	b.WriteString("你是 Agent「")
	b.WriteString(def.Name)
	b.WriteString("」的规划器，负责把任务拆成清晰、可执行的步骤。\n")
	if def.Description != "" {
		b.WriteString("Agent 职责：")
		b.WriteString(def.Description)
		b.WriteString("\n")
	}
	members := u.memberKeys(def)
	if len(members) > 0 {
		b.WriteString("\n可用执行成员（需要跨专长协作时把 agent 填成成员标识，否则留空由本 Agent 自己执行）：\n")
		for _, m := range members {
			b.WriteString("- ")
			b.WriteString(m)
			b.WriteString("\n")
		}
	}
	if extra != "" {
		b.WriteString("\n## 额外要求\n")
		b.WriteString(extra)
		b.WriteString("\n")
	}
	b.WriteString(`
只输出一个 JSON 对象，不要输出其他文字：
{"steps":[{"goal":"这一步要达成的具体目标","agent":"成员标识或空字符串","depends_on":[1]}]}

要求：
1. 步骤数控制在 1~6 个，按依赖顺序排列
2. depends_on 填依赖的步骤序号（从 1 开始的数组）；省略或空数组 = 依赖紧邻的上一步（按顺序串行执行）；
   多个步骤之间没有依赖时，明确写出各自只依赖哪些步骤，它们会被并行执行以加速
3. 每步写清产出物，不要写"分析一下"这类空泛步骤
4. 步骤之间不重复，后一步可直接使用前面步骤的结果
5. 能一步做完就不要拆成两步`)
	return b.String()
}

// makePlan 生成步骤计划
func (u *UseCase) makePlan(ctx context.Context, def *AgentDef, input, extra string, rec *Recorder) ([]planStep, error) {
	rec.turn++
	// 规划输出是 JSON（{"steps":[...]}）：低温 + JSON 模式，稳定且省自纠轮
	out, err := u.chatWithOpts(ctx, def, u.plannerPrompt(def, extra), "## 任务\n"+input, 0.2, true)
	if err != nil {
		return nil, err
	}
	var p struct {
		Steps []planStep `json:"steps"`
	}
	if err := decodeJSONInto(out, &p); err != nil {
		return nil, err
	}
	steps := make([]planStep, 0, len(p.Steps))
	for _, s := range p.Steps {
		s.Goal = strings.TrimSpace(s.Goal)
		s.Agent = strings.TrimSpace(s.Agent)
		if s.Goal == "" {
			continue
		}
		steps = append(steps, s)
		if len(steps) >= 6 {
			break
		}
	}
	return steps, nil
}

// synthesizePlan 把步骤产出整合成最终答案
func (u *UseCase) synthesizePlan(ctx context.Context, def *AgentDef, input string, done []string) (string, error) {
	system := "你是 Agent「" + def.Name + "」，负责把各步骤产出整合成一份面向用户的最终答案。"
	if def.System != "" {
		system += "\n\n## 角色设定\n" + def.System
	}
	system += `

要求：
1. 直接输出最终答案本身，不要罗列步骤过程，不要输出 JSON
2. 失败或缺失的步骤要如实说明，不要编造
3. 结论优先，过程只在必要时简述`
	user := "## 总任务\n" + input + "\n\n## 步骤产出\n" + strings.Join(done, "\n\n")
	return u.chatWith(ctx, def, system, user)
}

// execPlanStep 执行一个计划步骤：指定成员则委派，否则用自身工具循环。
// 派生 Recorder 并置 silentFinal（单步产出不是任务最终答案），
// 并发波次下每个步骤有独立轮次计数，不会互相踩。
// loopSt 为该步骤的工具循环状态（见 cloneForStep；传 nil 则全新）。
func (u *UseCase) execPlanStep(ctx context.Context, def *AgentDef, rec *Recorder, system, input string, done []string, st planStep, loopSt *RunState) (string, error) {
	stepRec := rec.derive()
	stepRec.silentFinal = true
	if st.Agent != "" && st.Agent != def.AgentKey {
		return u.delegate(ctx, stepRec, st.Agent, u.stepSeed(input, done, st.Goal))
	}
	res, err := u.runToolLoop(ctx, def, system, u.stepSeed(input, done, st.Goal), clampInt(u.turnBudget(def, 4), 2, 8), stepRec, loopSt)
	if err != nil {
		return "", err
	}
	return res.Answer, nil
}

// —— 1. Plan-and-Execute ——

// runPlanExecute 规划-执行分离：先一次性出完整计划，再按依赖分波执行（O4），
// 每个波次边界落 checkpoint（O2，崩溃/审批后续跑），单步失败触发重规划（上限 2 次）。
//
// 依赖约定：planStep.DependsOn 为 1-based 步骤序号；空 = 依赖前一步（保守串行，
// 与历史行为一致）。波次调度：每轮收集所有「依赖已满足」的待执行步骤并发跑
// （上限 parallelLimit），汇聚后推进 StepIdx。
func (u *UseCase) runPlanExecute(ctx context.Context, def *AgentDef, input string, rec *Recorder, st *RunState) (*RunResult, error) {
	if st == nil {
		st = newState(input)
	}
	rec.st = st

	// 计划：新任务生成；续跑任务沿用 checkpoint 里的计划（replan 后的剩余部分）
	if len(st.Steps) == 0 {
		steps, err := u.makePlan(ctx, def, input, "", rec)
		if err != nil {
			return nil, fmt.Errorf("生成计划失败: %w", err)
		}
		if len(steps) == 0 {
			// 不需要拆解：退化为单轮直答
			return u.runSimple(ctx, def, input, rec)
		}
		st.Steps = steps
		rec.event(EventPlan, rec.turn, map[string]any{"steps": steps, "waves": planWaves(steps)})
	} else {
		rec.event(EventPlan, rec.turn, map[string]any{"steps": st.Steps, "resumed": true})
	}

	system, err := u.buildSystemPrompt(ctx, def)
	if err != nil {
		return nil, err
	}

	// 续跑对齐：把乱序完成记录（MemberOut["step:<idx>"]）按序搬进 Done
	st.drainStepOutputs()

	total := len(st.Steps)
	for st.StepIdx < len(st.Steps) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("执行被取消: %w", err)
		}
		rec.setProgress(10 + (st.StepIdx*75)/max(total, 1))

		// 收集本波次：所有依赖已满足的待执行步骤
		ready := readySteps(st.Steps, st.StepIdx)
		if len(ready) == 0 {
			ready = []int{st.StepIdx} // 依赖成环等异常：兜底串行推进，防死锁
		}
		for _, i := range ready {
			rec.event(EventNode, rec.turn, map[string]any{
				"node": fmt.Sprintf("step-%d", i+1), "kind": "step", "agent": st.Steps[i].Agent,
				"status": "running", "output": truncateRunes(st.Steps[i].Goal, 300),
			})
		}

		// 并发执行（上限 parallelLimit；每个步骤派生独立 Recorder 与循环状态）
		results := make([]string, len(ready))
		errs := make([]error, len(ready))
		sem := make(chan struct{}, u.parallelLimit())
		var wg sync.WaitGroup
		for k, i := range ready {
			wg.Add(1)
			go func(k, i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				results[k], errs[k] = u.execPlanStep(ctx, def, rec, system, input, st.Done, st.Steps[i], st.cloneForStep())
			}(k, i)
		}
		wg.Wait()

		// 审批挂起不是步骤失败：不能走重规划（换条路绕开审批比停下更危险），
		// 也不能推进 StepIdx（那会把没执行的步标记成完成）。直接上抛挂起状态，
		// 任务置 paused，批准后从这一步续跑。
		for k := range ready {
			if errors.Is(errs[k], ErrApprovalNeeded) {
				rec.event(EventPaused, rec.turn, map[string]any{
					"step":   ready[k] + 1,
					"goal":   truncateRunes(st.Steps[ready[k]].Goal, 200),
					"reason": "approval_required",
				})
				return nil, errs[k]
			}
			if errors.Is(errs[k], ErrChildApprovalPending) {
				// 子 Agent 在等审批：父任务同样不能继续（下游步骤会缺上下文）
				return nil, errs[k]
			}
		}

		// 失败处理：取最早的失败步骤触发重规划（只重排该步之后的剩余部分；
		// 乱序完成的后续步骤产出丢弃，重跑代价 < 状态错乱的代价）
		failK, failIdx := -1, -1
		for k := range ready {
			if errs[k] != nil && (failIdx < 0 || ready[k] < failIdx) {
				failK, failIdx = k, ready[k]
			}
		}
		if failIdx >= 0 {
			if st.Replans >= 2 {
				return nil, fmt.Errorf("计划第 %d 步执行失败: %w", failIdx+1, errs[failK])
			}
			st.Replans++
			rec.event(EventError, rec.turn, map[string]any{
				"message": fmt.Sprintf("第 %d 步失败，触发重规划：%v", failIdx+1, errs[failK]),
			})
			note := fmt.Sprintf("原计划第 %d 步「%s」执行失败：%v。请重新给出从该步开始的剩余步骤（已完成的不再重复）。", failIdx+1, st.Steps[failIdx].Goal, errs[failK])
			next, perr := u.makePlan(ctx, def, input, note, rec)
			if perr != nil || len(next) == 0 {
				return nil, fmt.Errorf("第 %d 步失败且重规划无结果: %w", failIdx+1, errs[failK])
			}
			st.Steps = append(st.Steps[:failIdx], next...)
			total = len(st.Steps)
			rec.event(EventPlan, rec.turn, map[string]any{"steps": st.Steps, "revised": true})
			rec.checkpoint(fmt.Sprintf(`{"kind":"plan_step","index":%d}`, st.StepIdx))
			continue // 不推进 StepIdx：重试当前失败位置
		}

		// 成功：先入乱序完成表，再按序推进 StepIdx（保持 Done 与步骤前缀对齐）
		for k, i := range ready {
			st.MemberOut[fmt.Sprintf("step:%d", i)] = results[k]
		}
		for {
			out, ok := st.MemberOut[fmt.Sprintf("step:%d", st.StepIdx)]
			if !ok {
				break
			}
			delete(st.MemberOut, fmt.Sprintf("step:%d", st.StepIdx))
			rec.event(EventNode, rec.turn, map[string]any{
				"node": fmt.Sprintf("step-%d", st.StepIdx+1), "kind": "step", "agent": st.Steps[st.StepIdx].Agent,
				"status": "done", "output": truncateRunes(out, 800),
			})
			st.Done = append(st.Done, fmt.Sprintf("步骤 %d（%s）：%s", st.StepIdx+1, st.Steps[st.StepIdx].Goal, out))
			st.StepIdx++
		}
		rec.checkpoint(fmt.Sprintf(`{"kind":"plan_step","index":%d}`, st.StepIdx))
	}

	rec.setProgress(92)
	answer, err := u.synthesizePlan(ctx, def, input, st.Done)
	if err != nil {
		return nil, err
	}
	rec.turn++
	return u.finish(rec, answer), nil
}

// readySteps 收集本波次可执行的步骤下标：依赖全部落在已完成前缀内。
// DependsOn 为空 = 依赖前一步（串行语义：只有下一个待执行步骤就绪）。
func readySteps(steps []planStep, startIdx int) []int {
	ready := make([]int, 0, len(steps)-startIdx)
	for i := startIdx; i < len(steps); i++ {
		deps := steps[i].DependsOn
		if len(deps) == 0 {
			if i != startIdx {
				continue // 串行语义：等前一步
			}
			ready = append(ready, i)
			continue
		}
		ok := true
		for _, d := range deps {
			if d < 1 || d > startIdx { // 依赖必须是已完成的步骤（1-based ≤ startIdx）
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, i)
		}
	}
	return ready
}

// planWaves 静态预览波次分组（事件展示用）：贪心按依赖分层
func planWaves(steps []planStep) [][]int {
	done := 0
	waves := [][]int{}
	taken := make([]bool, len(steps))
	for done < len(steps) && len(waves) < 10 {
		wave := []int{}
		for i := range steps {
			if taken[i] {
				continue
			}
			deps := steps[i].DependsOn
			ok := true
			for _, d := range deps {
				if d < 1 || d > len(steps) || !taken[d-1] {
					ok = false
					break
				}
			}
			if ok && (len(deps) > 0 || i == done || takenBefore(i, taken)) {
				wave = append(wave, i+1)
			}
		}
		if len(wave) == 0 {
			break
		}
		for _, n := range wave {
			taken[n-1] = true
			done++
		}
		waves = append(waves, wave)
	}
	return waves
}

// takenBefore i 之前是否全部已完成（串行约定下首个未完成步骤才可入波）
func takenBefore(i int, taken []bool) bool {
	for j := 0; j < i; j++ {
		if !taken[j] {
			return false
		}
	}
	return true
}

// —— 2. Reflexion ——

func evaluatorPrompt(def *AgentDef) string {
	return "你是严格的评审员，负责判断 Agent「" + def.Name + "」的答案是否真正完成了任务。\n" + `
只输出一个 JSON 对象：
{"pass": true/false, "score": 0-100, "issues": "不通过时列出具体问题（多条用；分隔），通过时留空"}

判定标准：
1. 是否回答了任务的全部要求，有没有漏项
2. 事实/计算/引用是否可靠，有没有编造
3. 是否只是复述过程而没给结论
4. 表述是否可直接交付给用户
分数 ≥80 且无重大问题时 pass=true；不要为了严格而挑刺，能交付就通过。`
}

func reflectorPrompt(def *AgentDef) string {
	return "你是 Agent「" + def.Name + "」的复盘教练。根据评估意见总结可执行的教训。\n" + `
直接输出教训文本（不要 JSON），要求：
1. 逐条对应评估指出的问题，说明「错在哪 → 下次怎么做」
2. 给出具体做法而不是"要更仔细"这类空话
3. 不超过 5 条`
}

func (u *UseCase) runReflexion(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	system, err := u.buildSystemPrompt(ctx, def)
	if err != nil {
		return nil, err
	}
	rounds := clampInt(u.turnBudget(def, 3), 1, 5)
	seed := "## 任务\n" + input + "\n\n## 执行记录\n"
	var draft string

	for round := 1; round <= rounds; round++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("执行被取消: %w", err)
		}
		rec.setProgress(10 + (round-1)*60/rounds)
		saved := rec.silentFinal
		rec.silentFinal = true // 草稿不是最终答案
		res, rerr := u.runToolLoop(ctx, def, system, seed, clampInt(u.turnBudget(def, 4), 2, 8), rec, nil)
		rec.silentFinal = saved
		if rerr != nil {
			return nil, fmt.Errorf("第 %d 轮执行失败: %w", round, rerr)
		}
		draft = res.Answer

		// 评估
		rec.turn++
		evalOut, cerr := u.chatWith(ctx, def, evaluatorPrompt(def), fmt.Sprintf("## 任务\n%s\n\n## 待评估答案\n%s", input, draft))
		if cerr != nil {
			return nil, fmt.Errorf("第 %d 轮评估失败: %w", round, cerr)
		}
		var ver evalVerdict
		if derr := decodeJSONInto(evalOut, &ver); derr != nil {
			ver.Pass = false
			ver.Score = 50
			ver.Issues = "评估输出不合规：" + truncateRunes(evalOut, 200)
		}
		rec.event(EventEvaluate, rec.turn, map[string]any{
			"round": round, "pass": ver.Pass, "score": ver.Score, "issues": truncateRunes(ver.Issues, 1000),
		})
		rec.setProgress(10 + round*60/rounds)
		if ver.Pass {
			return u.finish(rec, draft), nil
		}
		if round == rounds {
			break // 最后一轮不再反思，直接返回最好的一版
		}

		// 反思
		rec.turn++
		lesson, lerr := u.chatWith(ctx, def, reflectorPrompt(def),
			fmt.Sprintf("## 任务\n%s\n\n## 上一轮答案\n%s\n\n## 评估意见\n%s", input, draft, ver.Issues))
		if lerr != nil || strings.TrimSpace(lesson) == "" {
			lesson = ver.Issues
		}
		rec.event(EventReflect, rec.turn, map[string]any{"round": round, "lesson": truncateRunes(lesson, 1500)})
		seed = fmt.Sprintf("## 任务\n%s\n\n## 上一轮答案（未通过评估）\n%s\n\n## 反思教训（必须逐条修正）\n%s\n\n## 执行记录\n",
			input, draft, lesson)
	}
	return u.finish(rec, draft), nil
}

// —— 3. Router + Skill ——

func routerPrompt(def *AgentDef, schemas []CapabilitySchema) string {
	var b strings.Builder
	b.WriteString("你是 Agent「")
	b.WriteString(def.Name)
	b.WriteString("」的路由器。你的唯一职责是判断这个任务该交给哪个技能处理，不要自己推理任务内容。\n\n## 可路由技能\n")
	b.WriteString(capabilityLines(schemas))
	b.WriteString(`
只输出一个 JSON 对象：
{"target": "技能名或 none", "args": {}, "reason": "一句话说明为什么路由到它"}

规则：
1. 只有确实能推进任务时才选技能；纯问答、闲聊、写作类任务输出 "none"
2. args 必须符合该技能的参数示例
3. 一次只选一个技能，不做多步规划
4. 不确定就选 none，由本 Agent 直接回答`)
	return b.String()
}

func directSystem(def *AgentDef) string {
	sys := "你是 Agent「" + def.Name + "」。"
	if def.Description != "" {
		sys += def.Description
	}
	if def.System != "" {
		sys += "\n\n" + def.System
	}
	return sys + "\n\n直接输出面向用户的答案，不要输出 JSON。"
}

func (u *UseCase) runRouter(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	schemas, err := u.allowedSchemas(ctx, def)
	if err != nil {
		return nil, err
	}
	// 没有可路由的技能：退化为直答，避免空转
	if len(schemas) == 0 {
		return u.runSimple(ctx, def, input, rec)
	}

	rec.turn++
	out, err := u.chatWith(ctx, def, routerPrompt(def, schemas), input)
	if err != nil {
		return nil, fmt.Errorf("路由判断失败: %w", err)
	}
	var route routeDecision
	if derr := decodeJSONInto(out, &route); derr != nil {
		return nil, fmt.Errorf("路由输出不合规: %w", derr)
	}
	target := strings.TrimSpace(route.Target)
	rec.event(EventRoute, rec.turn, map[string]any{
		"target": target, "args": route.Args, "reason": truncateRunes(route.Reason, 500),
	})

	if target == "" || strings.EqualFold(target, "none") || strings.EqualFold(target, "direct") {
		rec.setProgress(60)
		rec.turn++
		answer, aerr := u.chatWith(ctx, def, directSystem(def), input)
		if aerr != nil {
			return nil, aerr
		}
		return u.finish(rec, answer), nil
	}

	args := route.Args
	if args == nil {
		args = map[string]any{}
	}
	rec.event(EventToolCall, rec.turn, map[string]any{"name": target, "args": args})
	obs, callErr := u.InvokeDispatch(ctx, target, args, rec)
	if callErr != nil {
		if hardDispatchErr(callErr) {
			return nil, callErr
		}
		obs = "[路由技能调用失败] " + callErr.Error()
	}
	rec.event(EventToolResult, rec.turn, map[string]any{"name": target, "result": truncateRunes(obs, 4000), "is_error": callErr != nil})

	rec.setProgress(80)
	rec.turn++
	system := directSystem(def) + "\n\n下面给你该技能返回的结果，请据此直接回答用户任务；结果失败要如实说明，不要编造。"
	answer, aerr := u.chatWith(ctx, def, system,
		fmt.Sprintf("## 任务\n%s\n\n## 技能 %s 的结果\n%s", input, target, truncateRunes(obs, 6000)))
	if aerr != nil {
		return nil, aerr
	}
	return u.finish(rec, answer), nil
}

// —— 4. Self-Ask ——

func decomposePrompt(def *AgentDef) string {
	return "你是 Agent「" + def.Name + "」的提问拆解器。把任务拆成 1~4 个必须回答的子问题，用于逐个求解后再回答原问题。\n" + `
只输出一个 JSON 对象：
{"questions":["子问题1","子问题2"]}

要求：
1. 子问题要能被独立回答，且合起来足以回答原问题
2. 简单任务只给 1 个（甚至 0 个）子问题，不要为了拆而拆
3. 不要包含"如何回答原问题"这类元问题`
}

func (u *UseCase) runSelfAsk(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	rec.turn++
	out, err := u.chatWith(ctx, def, decomposePrompt(def), "## 任务\n"+input)
	if err != nil {
		return nil, fmt.Errorf("子问题拆解失败: %w", err)
	}
	var qd struct {
		Questions []string `json:"questions"`
	}
	if derr := decodeJSONInto(out, &qd); derr != nil {
		return nil, fmt.Errorf("子问题解析失败: %w", derr)
	}
	questions := make([]string, 0, 4)
	for _, q := range qd.Questions {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		questions = append(questions, q)
		if len(questions) >= 4 {
			break
		}
	}
	if len(questions) == 0 {
		return u.runSimple(ctx, def, input, rec)
	}
	rec.event(EventPlan, rec.turn, map[string]any{"questions": questions})

	system, err := u.buildSystemPrompt(ctx, def)
	if err != nil {
		return nil, err
	}
	qa := make([]string, 0, len(questions))
	for i, q := range questions {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("执行被取消: %w", err)
		}
		rec.setProgress(10 + i*70/len(questions))
		saved := rec.silentFinal
		rec.silentFinal = true // 子答案不是最终答案
		res, rerr := u.runToolLoop(ctx, def, system, "## 子问题\n"+q+"\n\n## 执行记录\n", clampInt(u.turnBudget(def, 3), 2, 6), rec, nil)
		rec.silentFinal = saved
		if rerr != nil {
			qa = append(qa, fmt.Sprintf("Q%d：%s\nA：（未能回答：%v）", i+1, q, rerr))
			continue
		}
		rec.event(EventNode, rec.turn, map[string]any{
			"node": fmt.Sprintf("q%d", i+1), "kind": "subq", "status": "done", "output": truncateRunes(res.Answer, 800),
		})
		qa = append(qa, fmt.Sprintf("Q%d：%s\nA：%s", i+1, q, res.Answer))
	}

	rec.setProgress(88)
	rec.turn++
	system2 := directSystem(def) + "\n\n下面是你自己拆出的子问题与答案，请综合它们回答原问题。"
	answer, aerr := u.chatWith(ctx, def, system2,
		fmt.Sprintf("## 原问题\n%s\n\n## 子问题与答案\n%s", input, strings.Join(qa, "\n\n")))
	if aerr != nil {
		return nil, aerr
	}
	return u.finish(rec, answer), nil
}

// —— 5. Function Call（原生单步工具调用）——

func funcCallPrompt(def *AgentDef, schemas []CapabilitySchema) string {
	var b strings.Builder
	if def.System != "" {
		b.WriteString(def.System)
		b.WriteString("\n\n")
	}
	b.WriteString("## 可用工具\n")
	b.WriteString(capabilityLines(schemas))
	b.WriteString(`
## 输出协议
只输出一个 JSON 对象，不要写思考过程、不要输出解释文字：

需要工具时：
{"tool":"工具名","args":{}}

可以直接回答时：
{"answer":"面向用户的完整答案"}

规则：
1. 只调用一次工具，拿到结果后会再给你一次机会作答
2. args 必须是对象，符合工具参数示例
3. 不写 thought，不写分析，不输出 JSON 以外的内容`)
	return b.String()
}

func (u *UseCase) runFunctionCall(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	schemas, err := u.allowedSchemas(ctx, def)
	if err != nil {
		return nil, err
	}
	if len(schemas) == 0 {
		return u.runSimple(ctx, def, input, rec)
	}

	rec.turn++
	out, err := u.chatWith(ctx, def, funcCallPrompt(def, schemas), input)
	if err != nil {
		return nil, fmt.Errorf("工具选择失败: %w", err)
	}
	var fc funcCallDecision
	if derr := decodeJSONInto(out, &fc); derr != nil {
		// 模型没按协议、但给了自然语言答案：容忍
		if strings.TrimSpace(out) != "" {
			return u.finish(rec, strings.TrimSpace(out)), nil
		}
		return nil, fmt.Errorf("工具调用输出不合规: %w", derr)
	}
	tool := strings.TrimSpace(fc.Tool)
	if tool == "" || strings.EqualFold(tool, "none") {
		answer := strings.TrimSpace(fc.Answer)
		if answer == "" {
			answer = strings.TrimSpace(out)
		}
		return u.finish(rec, answer), nil
	}

	args := fc.Args
	if args == nil {
		args = map[string]any{}
	}
	rec.event(EventToolCall, rec.turn, map[string]any{"name": tool, "args": args})
	obs, callErr := u.InvokeDispatch(ctx, tool, args, rec)
	if callErr != nil {
		if hardDispatchErr(callErr) {
			return nil, callErr
		}
		obs = "[工具调用失败] " + callErr.Error()
	}
	rec.event(EventToolResult, rec.turn, map[string]any{"name": tool, "result": truncateRunes(obs, 4000), "is_error": callErr != nil})

	rec.setProgress(80)
	rec.turn++
	system := directSystem(def) + "\n\n根据工具结果作答，只输出最终答案文本。"
	answer, aerr := u.chatWith(ctx, def, system,
		fmt.Sprintf("## 任务\n%s\n\n## 工具 %s 的结果\n%s", input, tool, truncateRunes(obs, 6000)))
	if aerr != nil {
		return nil, aerr
	}
	return u.finish(rec, answer), nil
}

// —— 6. State Machine ——

// stateList 状态集合：优先用编排图里的 Agent 节点，其次用成员，最后用通用三段
func (u *UseCase) stateList(def *AgentDef) []string {
	states := make([]string, 0, 6)
	if g := ParseFlowGraph(def.Flow); g != nil {
		for _, n := range g.Nodes {
			if n.Kind == FlowNodeAgent && n.AgentKey != "" && !containsStr(states, n.AgentKey) {
				states = append(states, n.AgentKey)
			}
		}
	}
	if len(states) == 0 {
		states = append(states, u.memberKeys(def)...)
	}
	if len(states) == 0 {
		states = []string{"分析", "执行", "校验"}
	}
	return states
}

func isDoneState(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "done", "end", "finish", "output", "完成", "结束", "输出":
		return true
	}
	return false
}

func statePrompt(def *AgentDef, states []string) string {
	var b strings.Builder
	b.WriteString("你是 Agent「")
	b.WriteString(def.Name)
	b.WriteString("」，按固定状态机推进任务：你只负责判断当前该进入哪个状态，不要自由发挥。\n\n## 状态列表（按顺序）\n")
	for i, s := range states {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	b.WriteString(`
## 输出协议
只输出一个 JSON 对象：
{"state":"状态名","note":"进入该状态要做什么"}
任务已经可以交付时：
{"state":"完成","final":"面向用户的最终答案"}

规则：
1. 只能使用状态列表中的状态名，或 "完成"
2. 只能前进，最多回退一次；同一状态连续执行不超过 2 次
3. 信息足够就直接给 final，不要空转`)
	if def.System != "" {
		b.WriteString("\n## 角色设定\n")
		b.WriteString(def.System)
	}
	return b.String()
}

func (u *UseCase) runStateMachine(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	states := u.stateList(def)
	prompt := statePrompt(def, states)
	maxTurns := clampInt(u.turnBudget(def, 6), 2, 12)
	history := ""
	lastState := ""
	repeat, backs := 0, 0
	lastIdx := -1

	for turn := 1; turn <= maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("执行被取消: %w", err)
		}
		rec.turn++
		rec.setProgress(5 + turn*80/maxTurns)
		out, err := u.chatWith(ctx, def, prompt, "## 任务\n"+input+"\n\n## 状态轨迹\n"+history)
		if err != nil {
			return nil, fmt.Errorf("状态判断失败: %w", err)
		}
		var d stateDecision
		if derr := decodeJSONInto(out, &d); derr != nil {
			history += fmt.Sprintf("\n[第 %d 次] 输出不合规，请只输出 JSON。\n", turn)
			continue
		}
		if strings.TrimSpace(d.Final) != "" {
			return u.finish(rec, d.Final), nil
		}
		state := strings.TrimSpace(d.State)
		if isDoneState(state) {
			// 状态机要求结束时没有给 final：让模型在已有轨迹上收敛
			rec.turn++
			answer, aerr := u.chatWith(ctx, def, directSystem(def),
				fmt.Sprintf("## 任务\n%s\n\n## 状态轨迹\n%s\n\n状态机已结束，请输出最终答案。", input, history))
			if aerr != nil {
				return nil, aerr
			}
			return u.finish(rec, answer), nil
		}
		idx := -1
		for i, s := range states {
			if s == state {
				idx = i
			}
		}
		if idx < 0 {
			history += fmt.Sprintf("\n[第 %d 次] 状态「%s」不在列表内，请重选。\n", turn, state)
			continue
		}
		if state == lastState {
			repeat++
			if repeat > 2 {
				history += "\n该状态已重复执行 3 次，请前进或直接给 final。\n"
				repeat = 0
				continue
			}
		} else {
			repeat = 0
		}
		if idx < lastIdx {
			backs++
			if backs > 1 {
				history += "\n不允许再次回退，请前进或给 final。\n"
				continue
			}
		}
		lastIdx = idx
		lastState = state

		rec.event(EventNode, rec.turn, map[string]any{"node": state, "kind": "state", "status": "running", "output": truncateRunes(d.Note, 300)})
		var produced string
		var xerr error
		if containsStr(u.memberKeys(def), state) {
			produced, xerr = u.delegate(ctx, rec, state, fmt.Sprintf("## 总任务\n%s\n\n## 你负责的状态\n%s\n\n%s\n\n请完成该状态的产出并直接给出结果。", input, state, d.Note))
		} else {
			produced, xerr = u.chatWith(ctx, def, "你是 Agent「"+def.Name+"」在状态「"+state+"」下的执行者。只完成本状态的工作，输出可被下一步使用的完整内容，不要输出 JSON。",
				fmt.Sprintf("## 总任务\n%s\n\n## 状态轨迹\n%s\n\n## 本状态要求\n%s\n%s", input, history, state, d.Note))
		}
		if xerr != nil {
			rec.event(EventNode, rec.turn, map[string]any{"node": state, "kind": "state", "status": "failed", "output": truncateRunes(xerr.Error(), 500)})
			history += fmt.Sprintf("\n[%s] 执行失败：%v\n", state, xerr)
			continue
		}
		rec.event(EventNode, rec.turn, map[string]any{"node": state, "kind": "state", "status": "done", "output": truncateRunes(produced, 800)})
		history += fmt.Sprintf("\n[%s] %s\n", state, truncateRunes(produced, 2000))
	}

	// 状态没走完：基于轨迹收敛
	rec.setProgress(92)
	rec.turn++
	answer, err := u.chatWith(ctx, def, directSystem(def),
		fmt.Sprintf("## 任务\n%s\n\n## 状态轨迹\n%s\n\n请基于以上信息直接输出最终答案。", input, history))
	if err != nil {
		return nil, err
	}
	return u.finish(rec, answer), nil
}

// —— 7/8. Orchestrator / Hierarchical ——

func (u *UseCase) runOrchestrator(ctx context.Context, def *AgentDef, input string, rec *Recorder, hierarchical bool, st *RunState) (*RunResult, error) {
	members := u.memberKeys(def)
	mode := "协调器（orchestrator）"
	extra := "为每个步骤指定负责成员：agent 字段填成员标识，让不同专长的成员分头执行。"
	if hierarchical {
		mode = "分层（hierarchical）"
		extra = "先把目标拆成 2~4 个子目标，每个子目标指定一个负责成员；成员会自己再细化执行。"
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("%w: %s 模式需要至少一个编排成员（在画布上连入成员 Agent）", ErrInvalidDef, mode)
	}

	if st == nil {
		st = newState(input)
	}
	rec.st = st
	if len(st.Steps) == 0 {
		steps, err := u.makePlan(ctx, def, input, extra, rec)
		if err != nil {
			return nil, fmt.Errorf("生成计划失败: %w", err)
		}
		if len(steps) == 0 {
			return u.runSimple(ctx, def, input, rec)
		}
		st.Steps = steps
		rec.event(EventPlan, rec.turn, map[string]any{"steps": steps, "mode": def.GraphType})
	}

	system, err := u.buildSystemPrompt(ctx, def)
	if err != nil {
		return nil, err
	}
	for i := st.StepIdx; i < len(st.Steps); i++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("执行被取消: %w", err)
		}
		s := st.Steps[i]
		// 未指定成员或成员不合法：按顺序轮转分配，保证每个成员都被用上
		if s.Agent == "" || !containsStr(members, s.Agent) {
			s.Agent = members[i%len(members)]
			st.Steps[i].Agent = s.Agent
		}
		goal := s.Goal
		if hierarchical {
			goal = "你是该子目标的负责人，请先把子目标细化为 2~4 个子步骤，再逐个执行并给出结果。\n子目标：" + s.Goal
		}
		rec.setProgress(10 + i*75/len(st.Steps))
		rec.event(EventNode, rec.turn, map[string]any{
			"node": fmt.Sprintf("subgoal-%d", i+1), "kind": "subgoal", "agent": s.Agent,
			"status": "running", "output": truncateRunes(s.Goal, 300),
		})
		got, serr := u.execPlanStep(ctx, def, rec, system, input, st.Done, planStep{Goal: goal, Agent: s.Agent}, nil)
		if serr != nil {
			// 单个成员失败不整体失败，如实记入汇总材料
			rec.event(EventNode, rec.turn, map[string]any{
				"node": fmt.Sprintf("subgoal-%d", i+1), "kind": "subgoal", "agent": s.Agent,
				"status": "failed", "output": truncateRunes(serr.Error(), 500),
			})
			got = fmt.Sprintf("[执行失败] %v", serr)
		} else {
			rec.event(EventNode, rec.turn, map[string]any{
				"node": fmt.Sprintf("subgoal-%d", i+1), "kind": "subgoal", "agent": s.Agent,
				"status": "done", "output": truncateRunes(got, 800),
			})
		}
		st.Done = append(st.Done, fmt.Sprintf("子目标 %d（%s，由 %s 负责）：%s", i+1, s.Goal, s.Agent, got))
		st.StepIdx = i + 1
		rec.checkpoint(fmt.Sprintf(`{"kind":"plan_step","index":%d}`, st.StepIdx))
	}

	rec.setProgress(92)
	answer, err := u.synthesizePlan(ctx, def, input, st.Done)
	if err != nil {
		return nil, err
	}
	rec.turn++
	return u.finish(rec, answer), nil
}

// —— 9. Debate ——

func (u *UseCase) runDebate(ctx context.Context, def *AgentDef, input string, rec *Recorder, st *RunState) (*RunResult, error) {
	members := u.memberKeys(def)
	if len(members) < 2 {
		return nil, fmt.Errorf("%w: 辩论（debate）模式需要至少 2 个成员（正方 / 反方）", ErrInvalidDef)
	}
	pro, con := members[0], members[1]
	rounds := clampInt(u.turnBudget(def, 2), 1, 4)

	if st == nil {
		st = newState(input)
	}
	rec.st = st

	// 正方首轮（续跑时复用 checkpoint 里的方案）
	rec.setProgress(10)
	if st.Proposal == "" {
		proposal, err := u.delegate(ctx, rec, pro,
			fmt.Sprintf("## 任务\n%s\n\n## 你的角色\n你是正方。请给出一个完整、可执行的方案，并说明关键理由与依据。", input))
		if err != nil {
			return nil, fmt.Errorf("正方首轮发言失败: %w", err)
		}
		st.Proposal = proposal
		rec.event(EventNode, rec.turn, map[string]any{"node": "proposal", "kind": "debate", "agent": pro, "status": "done", "output": truncateRunes(proposal, 800)})
		rec.checkpoint(`{"kind":"round","round":0}`)
	}

	for r := st.Round + 1; r <= rounds; r++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("执行被取消: %w", err)
		}
		rec.setProgress(20 + r*50/rounds)
		critique, cerr := u.delegate(ctx, rec, con,
			fmt.Sprintf("## 任务\n%s\n\n## 正方方案\n%s\n\n## 你的角色\n你是反方。请挑出其中事实错误、逻辑漏洞、遗漏与风险，并给出可执行的改进建议。", input, truncateRunes(st.Proposal, 6000)))
		if cerr != nil {
			return nil, fmt.Errorf("第 %d 轮反方发言失败: %w", r, cerr)
		}
		st.Critique = critique
		rec.event(EventNode, rec.turn, map[string]any{"node": fmt.Sprintf("round-%d-con", r), "kind": "debate", "agent": con, "status": "done", "output": truncateRunes(critique, 800)})

		revised, rerr := u.delegate(ctx, rec, pro,
			fmt.Sprintf("## 任务\n%s\n\n## 你的上一版方案\n%s\n\n## 反方意见\n%s\n\n## 你的角色\n你是正方。请据此修订方案，采纳合理意见，对不成立的质疑给出反驳。直接输出修订后的完整方案。",
				input, truncateRunes(st.Proposal, 6000), truncateRunes(critique, 6000)))
		if rerr != nil {
			return nil, fmt.Errorf("第 %d 轮正方修订失败: %w", r, rerr)
		}
		st.Proposal = revised
		st.Round = r
		rec.event(EventNode, rec.turn, map[string]any{"node": fmt.Sprintf("round-%d-pro", r), "kind": "debate", "agent": pro, "status": "done", "output": truncateRunes(revised, 800)})
		rec.checkpoint(fmt.Sprintf(`{"kind":"round","round":%d}`, r))
	}

	// 裁决：优先用监察 Agent，其次自我汇总
	rec.setProgress(88)
	if def.SupervisorKey != "" {
		verdict, verr := u.delegate(ctx, rec, def.SupervisorKey,
			fmt.Sprintf("## 任务\n%s\n\n## 辩论后的最终方案\n%s\n\n## 反方最后意见\n%s\n\n请作为裁决者：采纳成立的意见、修正错误，直接输出面向用户的最终答案。",
				input, truncateRunes(st.Proposal, 6000), truncateRunes(st.Critique, 4000)))
		if verr == nil && strings.TrimSpace(verdict) != "" {
			rec.event(EventSupervise, rec.turn, map[string]any{"agent": pro, "supervisor": def.SupervisorKey, "passed": true, "issues": ""})
			return u.finish(rec, verdict), nil
		}
	}
	rec.turn++
	answer, err := u.chatWith(ctx, def, directSystem(def)+"\n\n下面是正反双方辩论后的方案与反方意见，请给出共识结论，采纳成立的批评。",
		fmt.Sprintf("## 任务\n%s\n\n## 正方最终方案\n%s\n\n## 反方最后意见\n%s", input, truncateRunes(st.Proposal, 6000), truncateRunes(st.Critique, 4000)))
	if err != nil {
		return nil, err
	}
	return u.finish(rec, answer), nil
}

// —— 10. Blackboard ——

func (u *UseCase) runBlackboard(ctx context.Context, def *AgentDef, input string, rec *Recorder, st *RunState) (*RunResult, error) {
	members := u.memberKeys(def)
	if len(members) == 0 {
		return nil, fmt.Errorf("%w: 黑板（blackboard）模式需要至少一个成员", ErrInvalidDef)
	}
	rounds := clampInt(u.turnBudget(def, 2), 1, 3)

	if st == nil {
		st = newState(input)
	}
	rec.st = st
	if st.Board == "" {
		st.Board = "（空）"
	}
	total := rounds * len(members)
	// Wave = 全局发言序号（0-based）：续跑时跳过已完成发言（Wave 之前的不重跑）
	step := st.Wave

	for r := 1; r <= rounds; r++ {
		for mi, m := range members {
			g := (r-1)*len(members) + mi
			if g < st.Wave {
				continue // 续跑：该发言已完成
			}
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("执行被取消: %w", err)
			}
			step = g
			rec.setProgress(10 + (step+1)*75/total)
			in := fmt.Sprintf("## 目标\n%s\n\n## 黑板（共享状态，第 %d 轮）\n%s\n\n请读取黑板：补充你负责的部分，或修正他人的错误；直接输出你写入黑板的完整内容，不要复述黑板全文。",
				input, r, truncateRunes(st.Board, 6000))
			got, err := u.delegate(ctx, rec, m, in)
			if err != nil {
				rec.event(EventNode, rec.turn, map[string]any{"node": fmt.Sprintf("%s-r%d", m, r), "kind": "blackboard", "agent": m, "status": "failed", "output": truncateRunes(err.Error(), 500)})
				continue
			}
			st.Board += fmt.Sprintf("\n### 第 %d 轮 · %s\n%s", r, m, truncateRunes(got, 3000))
			st.Wave = g + 1
			rec.event(EventNode, rec.turn, map[string]any{"node": fmt.Sprintf("%s-r%d", m, r), "kind": "blackboard", "agent": m, "status": "done", "output": truncateRunes(got, 800)})
			rec.checkpoint(fmt.Sprintf(`{"kind":"round","round":%d,"member":%q}`, r, m))
		}
	}

	rec.setProgress(92)
	rec.turn++
	answer, err := u.chatWith(ctx, def, directSystem(def)+"\n\n下面是多轮协作的黑板内容，请整合成最终答案；冲突处以证据更充分的一方为准。",
		fmt.Sprintf("## 任务\n%s\n\n## 黑板\n%s", input, truncateRunes(st.Board, 12000)))
	if err != nil {
		return nil, err
	}
	return u.finish(rec, answer), nil
}
