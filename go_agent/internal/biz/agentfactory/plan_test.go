package agentfactory

// 第四阶段剩余项：计划校验 / 依赖重基 / 步骤独立状态与一致性恢复。
//
// 验收标准对应：
//   - 失败节点不能默认激活成功分支（本文件只覆盖计划侧；DAG 侧见 graph_test.go）
//   - 重启后不能重复执行已完成的步骤（StepState 持久化 + 续跑跳过）

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestValidatePlan 依赖越界 / 自依赖 / 前向依赖 / 重复依赖都要报出来，
// 并给出摘掉非法依赖后的计划（退化为串行仍然正确，只是不并发）。
func TestValidatePlan(t *testing.T) {
	steps := []planStep{
		{Goal: "s1", DependsOn: []int{0}},       // 0 非法（1-based）
		{Goal: "s2", DependsOn: []int{2}},       // 自依赖
		{Goal: "s3", DependsOn: []int{4}},       // 前向依赖
		{Goal: "s4", DependsOn: []int{1, 1, 2}}, // 重复 1
		{Goal: ""},                              // 目标为空
	}
	problems, fixed := validatePlan(steps)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"第 1 步依赖 0", "第 2 步依赖 2", "第 3 步依赖 4", "重复声明", "第 5 步目标为空"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("校验结果应包含 %q，实际：\n%s", want, joined)
		}
	}
	// 合法依赖保留：s4 的 1、2 都在它之前
	if got := fixed[3].DependsOn; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("s4 应保留合法依赖 [1 2]，实际 %v", got)
	}
	if len(fixed[1].DependsOn) != 0 || len(fixed[2].DependsOn) != 0 {
		t.Fatalf("非法依赖应被摘除，实际 s2=%v s3=%v", fixed[1].DependsOn, fixed[2].DependsOn)
	}
	if problems, _ := validatePlan([]planStep{{Goal: "a"}, {Goal: "b", DependsOn: []int{1}}}); len(problems) != 0 {
		t.Fatalf("合法计划不应报错，实际 %v", problems)
	}
}

// TestRebaseDeps 重规划结果拼到绝对下标前必须重基，否则串行依赖会被误判满足。
func TestRebaseDeps(t *testing.T) {
	next := []planStep{
		{Goal: "retry"},
		{Goal: "after", DependsOn: []int{1}},
		{Goal: "merge", DependsOn: []int{1, 2}},
	}
	out := rebaseDeps(2, next) // 替换段从绝对下标 2 开始
	if len(out[0].DependsOn) != 0 {
		t.Fatalf("首步无依赖，实际 %v", out[0].DependsOn)
	}
	if got := out[1].DependsOn; len(got) != 1 || got[0] != 3 {
		t.Fatalf("第 2 步应依赖绝对下标 3（原计划第 3 步），实际 %v", got)
	}
	if got := out[2].DependsOn; len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("第 3 步应依赖 [3 4]，实际 %v", got)
	}
	// 校验必须针对「拼接后的完整计划」：下标是绝对的，单独校验新段会误判前向依赖
	full := append([]planStep{{Goal: "done-1"}, {Goal: "done-2"}}, out...)
	if problems, _ := validatePlan(full); len(problems) != 0 {
		t.Fatalf("拼接后的完整计划应合法，实际 %v", problems)
	}
	// 单独校验新段（错误用法）会把段内依赖全判成前向依赖——记录这个差异，
	// 防止以后有人把 checkPlan 又挪到重基之前
	if problems, _ := validatePlan(out); len(problems) == 0 {
		t.Fatal("新段单独校验必然报前向依赖：调用方必须传拼接后的完整计划")
	}
}

// TestPromoteDoneStepsIdempotent Done/StepIdx 是 StepState 的前缀视图，重复推进不产生重复行。
func TestPromoteDoneStepsIdempotent(t *testing.T) {
	st := newState("x")
	st.Steps = []planStep{{Goal: "a"}, {Goal: "b"}, {Goal: "c"}}
	// 乱序完成：a 与 c 完成，b 还没跑
	st.finishStep(0, stepDone, "A")
	st.finishStep(2, stepDone, "C")
	st.promoteDoneSteps()
	if st.StepIdx != 1 || len(st.Done) != 1 || !strings.Contains(st.Done[0], "A") {
		t.Fatalf("只应推进到第 1 步，实际 StepIdx=%d Done=%v", st.StepIdx, st.Done)
	}
	// 重复推进（续跑）不会重复写 Done
	st.promoteDoneSteps()
	if len(st.Done) != 1 {
		t.Fatalf("重复推进不应产生新行，实际 %v", st.Done)
	}
	// b 完成后连续推进到 3
	st.finishStep(1, stepDone, "B")
	st.promoteDoneSteps()
	if st.StepIdx != 3 || len(st.Done) != 3 {
		t.Fatalf("应推进到 3 步，实际 StepIdx=%d Done=%v", st.StepIdx, st.Done)
	}
	if !strings.Contains(st.Done[2], "C") {
		t.Fatalf("第 3 行应为 C，实际 %v", st.Done)
	}
}

// TestClearStepsFrom 重规划替换某段之后，该段及以后的完成/失败记录必须清掉，
// 否则新步骤会被当成"已完成"跳过（该做的活没人做，任务却显示成功）。
func TestClearStepsFrom(t *testing.T) {
	st := newState("x")
	st.Steps = []planStep{{Goal: "a"}, {Goal: "b"}, {Goal: "c"}, {Goal: "d"}}
	st.finishStep(0, stepDone, "A")
	st.finishStep(2, stepDone, "C")
	st.MemberOut["step:3"] = "老格式残留"
	st.MemberOut["member-x"] = "编排成员产出"
	st.clearStepsFrom(2)
	if _, ok := st.StepState[2]; ok {
		t.Fatal("下标 2 的状态应被清除")
	}
	if _, ok := st.StepState[0]; !ok {
		t.Fatal("下标 0 的状态不应被清除")
	}
	if _, ok := st.MemberOut["step:3"]; ok {
		t.Fatal("老格式的乱序完成记录应被清除")
	}
	if _, ok := st.MemberOut["member-x"]; !ok {
		t.Fatal("编排成员产出不能误删")
	}
	// 清掉之后 StepIdx 仍在 1：重新从第 2 步跑
	st.promoteDoneSteps()
	if st.StepIdx != 1 {
		t.Fatalf("StepIdx 应为 1，实际 %d", st.StepIdx)
	}
}

// TestLegacyStepOutputMigration 老 checkpoint 把步骤产出记在 MemberOut["step:N"]，
// 升级后要能继续续跑（迁进 StepState），不能整段重跑。
func TestLegacyStepOutputMigration(t *testing.T) {
	st := newState("x")
	st.Steps = []planStep{{Goal: "a"}, {Goal: "b"}}
	legacy, _ := json.Marshal(map[string]any{
		"goal": "x", "steps": st.Steps, "step_idx": 0,
		"member_out": map[string]string{"step:0": "老产出"},
	})
	got := decodeState(string(legacy))
	if got == nil {
		t.Fatal("decodeState 不应返回 nil")
	}
	if r, ok := got.StepState[0]; !ok || r.Status != stepDone || r.Output != "老产出" {
		t.Fatalf("老格式应迁进 StepState，实际 %+v", got.StepState)
	}
	if _, ok := got.MemberOut["step:0"]; ok {
		t.Fatal("迁移后 MemberOut 不应残留 step 键")
	}
	got.promoteDoneSteps()
	if got.StepIdx != 1 || len(got.Done) != 1 {
		t.Fatalf("迁移后应能推进前缀，实际 StepIdx=%d Done=%v", got.StepIdx, got.Done)
	}
}

// 乱序完成的步骤在续跑时不能重跑：StepState 说 done 就直接跳过（副作用只发生一次）。
// 场景：并行波次里第 3 步先完成、第 2 步还没跑时进程崩了。
func TestPlanResumeSkipsCompletedSteps(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought":"好","final":"第一步完成"}`,
		`{"thought":"好","final":"第二步完成"}`,
		`总结：都完成了`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "计划续跑", AgentKey: "resume-skip-agent", GraphType: GraphPlanExecute,
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "跑三步"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 预置：第 3 步（下标 2）已完成但 StepIdx 还没推进（崩溃时的真实形态）
	st := newState("跑三步")
	st.Steps = []planStep{{Goal: "第一步"}, {Goal: "第二步"}, {Goal: "第三步"}}
	st.StepState = map[int]StepRecord{2: {Status: stepDone, Output: "第三步产出"}}
	if err := repo.SaveCheckpoint(context.Background(), &TaskCheckpoint{
		TaskID: task.ID, Seq: 1, GraphType: GraphPlanExecute,
		Cursor: `{"kind":"plan_step","index":0,"step":2}`, State: encodeState(st),
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("续跑失败: status=%s err=%s", final.Status, final.ErrorMsg)
	}
	// 第 1、2 步各一次循环 + 汇总 = 3 次；第 3 步被跳过（否则会多一次甚至脚本耗尽）
	if len(chat.calls) != 3 {
		t.Fatalf("应恰好 3 次 LLM 调用（两步循环+汇总），实际 %d", len(chat.calls))
	}
	// 最终 checkpoint 里三步都是 done，且第 3 步仍是原来的产出
	cp, _ := repo.LatestCheckpoint(context.Background(), task.ID)
	if cp == nil {
		t.Fatal("应有 checkpoint")
	}
	state := decodeState(cp.State)
	if state == nil {
		t.Fatal("checkpoint 状态不可解析")
	}
	if state.StepIdx != 3 {
		t.Fatalf("三步应全部完成，StepIdx=%d", state.StepIdx)
	}
	if r := state.StepState[2]; r.Status != stepDone || r.Output != "第三步产出" {
		t.Fatalf("已完成的第 3 步不应被重写，实际 %+v", r)
	}
	if len(state.Done) != 3 || !strings.Contains(state.Done[2], "第三步产出") {
		t.Fatalf("Done 前缀应含三步产出，实际 %v", state.Done)
	}
}

// 并行波次的步骤各自落盘：全部完成后 checkpoint 里应能看到每一步的独立状态。
func TestPlanParallelStepsPersistedIndividually(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"steps":[{"goal":"a"},{"goal":"b","depends_on":[1]},{"goal":"c","depends_on":[1]}]}`,
		`{"thought":"好","final":"A"}`,
		`{"thought":"好","final":"B"}`,
		`{"thought":"好","final":"C"}`,
		`汇总完成`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "波次持久化", AgentKey: "wave-persist-agent", GraphType: GraphPlanExecute,
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "跑波次"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("执行失败: %s %s", final.Status, final.ErrorMsg)
	}

	// 中间快照里应出现过「单步完成」的游标（逐步骤落盘，而不是只在波次末尾）
	repo.mu.Lock()
	defer repo.mu.Unlock()
	perStep := 0
	var lastState *RunState
	for _, cp := range repo.checkpoints {
		if cp.TaskID != task.ID {
			continue
		}
		if strings.Contains(cp.Cursor, `"status"`) {
			perStep++ // 单步完成/失败的快照带状态字段
		}
		lastState = decodeState(cp.State)
	}
	if perStep < 3 {
		t.Fatalf("三个步骤应各自落一次快照，实际 %d", perStep)
	}
	if lastState == nil || len(lastState.StepState) != 3 {
		t.Fatalf("最终快照应含三步独立状态，实际 %+v", lastState)
	}
	for i := 0; i < 3; i++ {
		if r := lastState.StepState[i]; r.Status != stepDone || r.Output == "" {
			t.Fatalf("第 %d 步状态异常：%+v", i+1, r)
		}
	}
}
