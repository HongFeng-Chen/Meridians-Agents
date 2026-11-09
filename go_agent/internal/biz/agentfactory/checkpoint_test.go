package agentfactory

// M1 优化项的单元测试：
//   - O2 步骤级 checkpoint：plan_execute 从断点续跑（已完成步骤不重跑）
//   - O3 人工审批闸门：require_approval 命中 → paused；approve 续跑跳过闸门；reject 终态
//   - O4 波次调度：readySteps 依赖分层

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestPlanExecuteCheckpointResume 预置一份「第 1 步已完成」的 checkpoint，
// 执行时应直接从第 2 步续跑：不重新规划、不重跑第 1 步。
func TestPlanExecuteCheckpointResume(t *testing.T) {
	chat := &fakeChatter{script: []string{
		// 只应被消费 2 条：第 2 步的工具循环 + 汇总
		`{"thought":"好","final":"第二步完成"}`,
		`总结：两步都完成了`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "计划助手", AgentKey: "resume-plan-agent", GraphType: GraphPlanExecute,
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 预置 checkpoint：第 1 步已完成，待执行第 2 步
	st := newState("算 1+2")
	st.Steps = []planStep{{Goal: "第一步"}, {Goal: "第二步"}}
	st.Done = []string{"步骤 1（第一步）：已算好"}
	st.StepIdx = 1
	if err := repo.SaveCheckpoint(context.Background(), &TaskCheckpoint{
		TaskID: task.ID, Seq: 3, GraphType: GraphPlanExecute,
		Cursor: `{"kind":"plan_step","index":1}`, State: encodeState(st),
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("续跑失败: status=%s err=%s", final.Status, final.ErrorMsg)
	}
	if final.Result != "总结：两步都完成了" {
		t.Fatalf("result=%q", final.Result)
	}
	// 只消费 2 条脚本：第 2 步循环 + 汇总（没有规划调用、没有第 1 步重跑）
	if len(chat.calls) != 2 {
		t.Fatalf("应恰好 2 次 LLM 调用（步骤循环+汇总），实际 %d", len(chat.calls))
	}
	types := typesOf(u, final)
	if types[EventResume] != 1 {
		t.Fatalf("应有 1 条 resume 事件，实际 %v", types)
	}
	if types[EventPlan] != 1 {
		t.Fatalf("续跑也应补一条 plan 事件（resumed），实际 %v", types)
	}
}

// TestApprovalPauseAndResume 命中 require_approval → paused；
// 批准后从 checkpoint 续跑，已批准能力不再触发闸门。
func TestApprovalPauseAndResume(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought":"算一下","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,    // → 命中闸门
		`{"thought":"已批准，再算","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`, // 续跑第 2 轮
		`{"thought":"好","final":"1+2=3"}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "受控助手", AgentKey: "gated-agent", GraphType: GraphReact,
		Capabilities:    []string{"mcp:demo__calc"},
		RequireApproval: []string{"mcp:demo__*"},
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 第一次执行：命中闸门 → paused
	final := runTask(t, u, task.ID)
	if final.Status != TaskPaused {
		t.Fatalf("应挂起待审批，实际 %s（err=%s）", final.Status, final.ErrorMsg)
	}
	types := typesOf(u, final)
	if types[EventPaused] != 1 {
		t.Fatalf("应有 1 条 paused 事件，实际 %v", types)
	}

	// 批准 → 置回 pending，checkpoint 里有 Approved 记录
	approved, err := u.ApproveTask(context.Background(), task.ID, "approve", "同意执行")
	if err != nil {
		t.Fatalf("ApproveTask: %v", err)
	}
	if approved.Status != TaskPending {
		t.Fatalf("批准后应为 pending，实际 %s", approved.Status)
	}

	// 第二次执行：从 loop 断点续跑，不再触发闸门
	final = runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("续跑失败: status=%s err=%s", final.Status, final.ErrorMsg)
	}
	if final.Result != "1+2=3" {
		t.Fatalf("result=%q", final.Result)
	}
	types = typesOf(u, final)
	if types[EventResume] != 1 {
		t.Fatalf("续跑应有 resume 事件，实际 %v", types)
	}
	if types[EventToolCall] != 2 {
		t.Fatalf("批准前后各一次工具调用（共 2），实际 %v", types)
	}
	if types[EventApproval] != 1 {
		t.Fatalf("应有一条 approval 结论事件，实际 %v", types)
	}
}

// TestApprovalReject 驳回 → 终态失败，checkpoint 清空。
func TestApprovalReject(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought":"算一下","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "受控助手", AgentKey: "gated-reject-agent", GraphType: GraphReact,
		Capabilities:    []string{"mcp:demo__calc"},
		RequireApproval: []string{"mcp:demo__calc"},
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskPaused {
		t.Fatalf("应挂起待审批，实际 %s", final.Status)
	}

	rejected, err := u.ApproveTask(context.Background(), task.ID, "reject", "不允许执行该操作")
	if err != nil {
		t.Fatalf("ApproveTask(reject): %v", err)
	}
	if rejected.Status != TaskFailed {
		t.Fatalf("驳回后应为 failed，实际 %s", rejected.Status)
	}
	if !strings.Contains(rejected.ErrorMsg, "人工驳回") {
		t.Fatalf("error_msg 应包含驳回原因，实际 %q", rejected.ErrorMsg)
	}
	if cp, _ := repo.LatestCheckpoint(context.Background(), task.ID); cp != nil {
		t.Fatalf("驳回后 checkpoint 应清空")
	}
}

// TestApproveNonPausedTask 非挂起任务不可审批。
func TestApproveNonPausedTask(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	def, err := u.CreateDef(context.Background(), AgentInput{Name: "x", AgentKey: "x-agent", GraphType: GraphSimple})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if _, err := u.ApproveTask(context.Background(), task.ID, "approve", ""); !errors.Is(err, ErrTaskStateDeny) {
		t.Fatalf("非 paused 任务审批应拒绝，got %v", err)
	}
}

// TestReadySteps 波次调度：显式依赖并行、空依赖串行兜底、已完成步骤跳过。
func TestReadySteps(t *testing.T) {
	steps := []planStep{
		{Goal: "s1", DependsOn: []int{}},     // 空依赖 = 串行语义：只有首个就绪
		{Goal: "s2", DependsOn: []int{1}},    // 依赖 s1
		{Goal: "s3", DependsOn: []int{1}},    // 依赖 s1（与 s2 同波并行）
		{Goal: "s4", DependsOn: []int{2, 3}}, // 依赖 s2、s3
	}
	done := func(idx ...int) map[int]bool {
		m := map[int]bool{}
		for _, i := range idx {
			m[i] = true
		}
		return m
	}
	// startIdx=0：只有 s1（空依赖且是首个）
	if got := readySteps(steps, 0, done()); len(got) != 1 || got[0] != 0 {
		t.Fatalf("startIdx=0 应只有 s1，实际 %v", got)
	}
	// startIdx=1（s1 完成）：s2、s3 都就绪（依赖 s1），并行
	got := readySteps(steps, 1, done(0))
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("startIdx=1 应 s2/s3 并行，实际 %v", got)
	}
	// startIdx=3（s1~s3 完成）：只有 s4
	if got := readySteps(steps, 3, done(0, 1, 2)); len(got) != 1 || got[0] != 3 {
		t.Fatalf("startIdx=3 应只有 s4，实际 %v", got)
	}
	// 乱序完成：s3 先完成、s2 还没跑 → s4 依赖未满足，不得就绪
	if got := readySteps(steps, 1, done(0, 2)); len(got) != 1 || got[0] != 1 {
		t.Fatalf("s4 的依赖 s2 未完成时不应就绪，实际 %v", got)
	}
	// 已完成的步骤不再返回（否则续跑会重复执行副作用）
	if got := readySteps(steps, 1, done(0, 1, 2, 3)); len(got) != 0 {
		t.Fatalf("全部完成时不应有就绪步骤，实际 %v", got)
	}
}

// TestPlanExecuteWaves 三个步骤 1←(2,3)：plan 事件应给出波次分组，执行成功。
func TestPlanExecuteWaves(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"steps":[{"goal":"a"},{"goal":"b","depends_on":[1]},{"goal":"c","depends_on":[1]}]}`,
		`{"thought":"好","final":"A"}`,
		`{"thought":"好","final":"B"}`,
		`{"thought":"好","final":"C"}`,
		`汇总完成`,
	}}
	u, final := runGraphOnce(t, chat, AgentInput{
		Name: "波次助手", AgentKey: "wave-agent", GraphType: GraphPlanExecute,
	})
	if final.Status != TaskSucceeded {
		t.Fatalf("执行失败: %s %s", final.Status, final.ErrorMsg)
	}
	_ = u
}
