package agentfactory

// 多 Agent 编排：串行 / 并行。
//
//   serial    按 members 顺序执行，后一个成员能看到前一个的结果
//   parallel  成员并发执行，全部完成后汇总
//
// 每个成员都是一条子任务（parent_task_id 指向根任务），形成任务链路树；
// 编排完成后由根 Agent 汇总成最终答案。若配置了监察 Agent，
// 会在汇总前按监察范围逐个审查成员产出（pass/reject + 意见）。

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// memberResult 成员执行结果
type memberResult struct {
	AgentKey string
	TaskID   uint64
	Answer   string
	Err      error
	Verdict  *supervisionVerdict
}

// runWorkflow 串行 / 并行编排
func (u *UseCase) runWorkflow(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	members := def.Members
	if len(members) == 0 {
		return nil, fmt.Errorf("%w: 编排方式为 %s 但没有配置协作成员", ErrInvalidDef, def.Orchestration)
	}
	sup := u.loadSupervisor(ctx, def.SupervisorKey)

	var results []memberResult
	if def.Orchestration == OrchestrationParallel {
		results = u.runMembersParallel(ctx, def, input, rec)
	} else {
		results = u.runMembersSerial(ctx, def, input, rec)
	}

	// 监察：按监察范围逐个审查（LLM 调用串行，避免并发打爆配额）
	if sup != nil {
		for i := range results {
			r := &results[i]
			if r.Err != nil || r.Answer == "" {
				continue
			}
			r.Verdict = u.superviseMember(ctx, rec.task, sup, r.AgentKey, input, r.Answer, rec)
		}
	}

	// 汇总：把各成员产出（含监察意见）交给根 Agent 合成最终答案
	answer, err := u.synthesize(ctx, def, input, results)
	if err != nil {
		return nil, err
	}
	return &RunResult{Answer: answer, Turns: len(results) + 1}, nil
}

// runMembersSerial 串行执行：后一个成员可见前一个的结果
func (u *UseCase) runMembersSerial(ctx context.Context, def *AgentDef, input string, rec *Recorder) []memberResult {
	results := make([]memberResult, 0, len(def.Members))
	var prev strings.Builder
	for _, key := range def.Members {
		memberInput := input
		if prev.Len() > 0 {
			memberInput = fmt.Sprintf("%s\n\n## 上游成员产出（供参考）\n%s", input, truncateRunes(prev.String(), 3000))
		}
		child, ans, err := u.runChildTask(ctx, rec.task, key, memberInput, rec)
		r := memberResult{AgentKey: key, Answer: ans, Err: err}
		if child != nil {
			r.TaskID = child.ID
		}
		results = append(results, r)
		if err != nil {
			continue // 单个成员失败不阻断后续成员，最终汇总时会带上错误
		}
		fmt.Fprintf(&prev, "\n### %s\n%s\n", key, truncateRunes(ans, 1500))
	}
	return results
}

// runMembersParallel 并发执行：先逐个建子任务（链路清晰），再并发跑
func (u *UseCase) runMembersParallel(ctx context.Context, def *AgentDef, input string, rec *Recorder) []memberResult {
	type job struct {
		key  string
		task *Task
		def  *AgentDef
	}
	jobs := make([]job, 0, len(def.Members))
	results := make([]memberResult, len(def.Members))

	for i, key := range def.Members {
		child, memberDef, err := u.createChildTask(ctx, rec.task, key, input, rec)
		if err != nil {
			results[i] = memberResult{AgentKey: key, Err: err}
			continue
		}
		jobs = append(jobs, job{key: key, task: child, def: memberDef})
		results[i] = memberResult{AgentKey: key, TaskID: child.ID}
	}

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			ans, err := u.executeTaskNow(ctx, j.task, j.def)
			for i := range results {
				if results[i].TaskID == j.task.ID {
					results[i].Answer = ans
					results[i].Err = err
					return
				}
			}
		}(j)
	}
	wg.Wait()
	return results
}

// createChildTask 建子任务并记委派事件（并发编排时先建后跑，保证链路顺序可读）
func (u *UseCase) createChildTask(ctx context.Context, parent *Task, agentKey, input string, rec *Recorder) (*Task, *AgentDef, error) {
	def, err := u.loadDefByKey(ctx, agentKey)
	if err != nil {
		return nil, nil, fmt.Errorf("编排成员 %q 不存在或已停用: %w", agentKey, err)
	}
	if !def.Enabled {
		return nil, nil, fmt.Errorf("编排成员 %q 已停用", agentKey)
	}
	child := &Task{
		AgentDefID:   def.ID,
		AgentKey:     def.AgentKey,
		Title:        truncateRunes(input, 80),
		Input:        input,
		Status:       TaskPending,
		ParentTaskID: parent.ID,
		Depth:        parent.Depth + 1,
		ConvID:       parent.ConvID, // 会话编排链路：子任务继承来源会话，保证任务树整层可按会话追溯
		UserID:       parent.UserID, // 编排成员任务继承父任务归属
	}
	if err := u.repo.CreateTask(ctx, child); err != nil {
		return nil, nil, fmt.Errorf("创建子任务失败: %w", err)
	}
	if rec != nil {
		rec.event(EventDelegate, rec.turn, map[string]any{
			"agent":         agentKey,
			"child_task_id": child.ID,
			"input":         truncateRunes(input, 500),
			"mode":          "workflow",
		})
	}
	return child, def, nil
}

// synthesize 汇总成员产出，交给根 Agent 生成最终答案
func (u *UseCase) synthesize(ctx context.Context, def *AgentDef, input string, results []memberResult) (string, error) {
	system := def.System
	if system == "" {
		system = "你是多 Agent 协作的根 Agent「" + def.Name + "」。"
	}
	system += `

## 汇总要求
下面是各子 Agent 的产出（可能包含监察意见）。请综合它们完成任务：
1. 优先采用监察通过的内容，监察指出的问题要在结论中修正或规避
2. 失败成员要如实说明，不要编造其结论
3. 直接输出面向用户的最终答案，不要输出 JSON，不要罗列过程`

	var b strings.Builder
	b.WriteString("## 任务\n")
	b.WriteString(input)
	b.WriteString("\n\n## 子 Agent 产出\n")
	for _, r := range results {
		fmt.Fprintf(&b, "\n### %s", r.AgentKey)
		if r.TaskID > 0 {
			fmt.Fprintf(&b, "（任务 #%d）", r.TaskID)
		}
		b.WriteString("\n")
		if r.Err != nil {
			fmt.Fprintf(&b, "[执行失败] %s\n", truncateRunes(r.Err.Error(), 300))
			continue
		}
		b.WriteString(truncateRunes(r.Answer, 4000))
		b.WriteString("\n")
		if v := r.Verdict; v != nil {
			if v.Passed {
				b.WriteString("[监察] 通过\n")
			} else {
				fmt.Fprintf(&b, "[监察] 未通过：%s", truncateRunes(v.Issues, 600))
				if v.Suggest != "" {
					fmt.Fprintf(&b, "；建议：%s", truncateRunes(v.Suggest, 400))
				}
				b.WriteString("\n")
			}
		}
	}
	return u.chatWith(ctx, def, system, b.String())
}
