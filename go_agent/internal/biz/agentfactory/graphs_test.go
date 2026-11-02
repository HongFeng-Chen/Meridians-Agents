package agentfactory

// 各执行图（Agent 架构模式）的用例：验证每种架构都能按各自的控制流跑通并落 trace。

import (
	"context"
	"testing"
)

// runGraphOnce 用给定脚本创建 Agent 并手动执行一次任务
func runGraphOnce(t *testing.T, chat *fakeChatter, in AgentInput) (*UseCase, *Task) {
	t.Helper()
	u, _, cache := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDef(%s): %v", in.GraphType, err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if len(cache.queue) != 1 {
		t.Fatalf("任务应已入队")
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("%s 执行失败: status=%s err=%s", in.GraphType, final.Status, final.ErrorMsg)
	}
	return u, final
}

func typesOf(u *UseCase, task *Task) map[string]int {
	out := map[string]int{}
	for _, x := range u.repo.(*fakeRepo).eventTypes(task.ID) {
		out[x]++
	}
	return out
}

// —— 1. Plan-and-Execute ——

func TestPlanExecuteFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"steps":[{"goal":"算出 1+2","agent":""},{"goal":"核对结果","agent":""}]}`,
		`{"thought":"调用计算器","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
		`{"thought":"得到 3","final":"1+2=3"}`,
		`{"thought":"无需再算","final":"核对完成"}`,
		`综合结论：1+2=3`,
	}}
	u, final := runGraphOnce(t, chat, AgentInput{
		Name: "计划助手", AgentKey: "plan-agent", GraphType: GraphPlanExecute,
		Capabilities: []string{"mcp:demo__calc"},
	})
	if final.Result != "综合结论：1+2=3" {
		t.Fatalf("result=%q", final.Result)
	}
	types := typesOf(u, final)
	if types[EventPlan] != 1 {
		t.Fatalf("应有 1 条计划事件，实际 %v", types)
	}
	if types[EventNode] != 4 {
		t.Fatalf("两个步骤各有 running/done 两条 node 事件（共 4），实际 %v", types)
	}
	if types[EventFinal] != 1 {
		t.Fatalf("中间步骤不应产生 final 事件，实际 %v", types)
	}
}

// —— 2. Reflexion ——

func TestReflexionFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought":"先给结论","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
		`{"thought":"大概是这样","final":"草案答案"}`,
		`{"pass":false,"score":40,"issues":"没有给出计算依据"}`,
		`教训：先算再下结论，并写出依据。`,
		`{"thought":"补上依据","final":"改进后的答案：1+2=3"}`,
		`{"pass":true,"score":95,"issues":""}`,
	}}
	u, final := runGraphOnce(t, chat, AgentInput{
		Name: "反思助手", AgentKey: "reflexion-agent", GraphType: GraphReflexion,
		Capabilities: []string{"mcp:demo__calc"}, MaxTurns: 3,
	})
	if final.Result != "改进后的答案：1+2=3" {
		t.Fatalf("result=%q", final.Result)
	}
	types := typesOf(u, final)
	if types[EventEvaluate] != 2 || types[EventReflect] != 1 {
		t.Fatalf("评估/反思事件数不对：%v", types)
	}
	if types[EventFinal] != 1 {
		t.Fatalf("草稿不应产生 final 事件：%v", types)
	}
}

// —— 3. Router + Skill ——

func TestRouterFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"target":"mcp:demo__calc","args":{"a":1,"b":2},"reason":"需要算数"}`,
		`计算结果是 3`,
	}}
	u, final := runGraphOnce(t, chat, AgentInput{
		Name: "路由助手", AgentKey: "router-agent", GraphType: GraphRouter,
		Capabilities: []string{"mcp:demo__calc"},
	})
	if final.Result != "计算结果是 3" {
		t.Fatalf("result=%q", final.Result)
	}
	types := typesOf(u, final)
	if types[EventRoute] != 1 || types[EventToolCall] != 1 {
		t.Fatalf("路由/调用事件数不对：%v", types)
	}
}

// —— 4. Self-Ask ——

func TestSelfAskFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"questions":["1+2 等于几"]}`,
		`{"thought":"直接计算","final":"3"}`,
		`原问题答案：3`,
	}}
	u, final := runGraphOnce(t, chat, AgentInput{
		Name: "自问助手", AgentKey: "selfask-agent", GraphType: GraphSelfAsk,
	})
	if final.Result != "原问题答案：3" {
		t.Fatalf("result=%q", final.Result)
	}
	types := typesOf(u, final)
	if types[EventPlan] != 1 || types[EventNode] != 1 {
		t.Fatalf("子问题事件数不对：%v", types)
	}
}

// —— 5. Function Call ——

func TestFunctionCallFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"tool":"mcp:demo__calc","args":{"a":2,"b":3}}`,
		`结果是 5`,
	}}
	u, final := runGraphOnce(t, chat, AgentInput{
		Name: "工具助手", AgentKey: "fc-agent", GraphType: GraphFuncCall,
		Capabilities: []string{"mcp:demo__calc"},
	})
	if final.Result != "结果是 5" {
		t.Fatalf("result=%q", final.Result)
	}
	types := typesOf(u, final)
	if types[EventThought] != 0 {
		t.Fatalf("function_call 不应输出思考事件：%v", types)
	}
	if types[EventToolCall] != 1 {
		t.Fatalf("应有一次工具调用：%v", types)
	}
}

// —— 6. State Machine ——

func TestStateMachineFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"state":"分析","note":"先看清任务"}`,
		`任务是把 1 与 2 相加`,
		`{"state":"完成","final":"状态机最终答案：3"}`,
	}}
	u, final := runGraphOnce(t, chat, AgentInput{
		Name: "状态机助手", AgentKey: "sm-agent", GraphType: GraphStateMachine,
	})
	if final.Result != "状态机最终答案：3" {
		t.Fatalf("result=%q", final.Result)
	}
	types := typesOf(u, final)
	if types[EventNode] == 0 {
		t.Fatalf("应有状态节点事件：%v", types)
	}
}

// —— 7. Orchestrator ——

func TestOrchestratorFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"steps":[{"goal":"子任务一","agent":"worker-a"},{"goal":"子任务二","agent":"worker-b"}]}`,
		`成员 A 产出`,
		`成员 B 产出`,
		`汇总答案：A+B`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	for _, k := range []string{"worker-a", "worker-b"} {
		if _, err := u.CreateDef(context.Background(), AgentInput{
			Name: k, AgentKey: k, GraphType: GraphSimple,
		}); err != nil {
			t.Fatalf("create %s: %v", k, err)
		}
	}
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "协调器", AgentKey: "orch-agent", GraphType: GraphOrchestrator,
		Members: []string{"worker-a", "worker-b"},
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded || final.Result != "汇总答案：A+B" {
		t.Fatalf("status=%s result=%q err=%s", final.Status, final.Result, final.ErrorMsg)
	}
	types := typesOf(u, final)
	if types[EventDelegate] != 2 {
		t.Fatalf("应委派两个成员：%v", types)
	}
}

// —— 8. Hierarchical ——

func TestHierarchicalFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"steps":[{"goal":"拆解并完成整个任务","agent":"worker-a"}]}`,
		`子目标完成`,
		`分层汇总答案`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "worker-a", AgentKey: "worker-a", GraphType: GraphSimple,
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "分层", AgentKey: "hier-agent", GraphType: GraphHierarchical, Members: []string{"worker-a"},
	}); err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "hier-agent", Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded || final.Result != "分层汇总答案" {
		t.Fatalf("status=%s result=%q err=%s", final.Status, final.Result, final.ErrorMsg)
	}
}

// —— 9. Debate ——

func TestDebateFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`正方首轮方案：用计算器求和`,
		`反方批评：没说明进位规则`,
		`正方修订方案：按十进制进位，和为 3`,
		`共识结论：1+2=3`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	for _, k := range []string{"debater-pro", "debater-con"} {
		if _, err := u.CreateDef(context.Background(), AgentInput{
			Name: k, AgentKey: k, GraphType: GraphSimple,
		}); err != nil {
			t.Fatalf("create %s: %v", k, err)
		}
	}
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "辩论", AgentKey: "debate-agent", GraphType: GraphDebate,
		Members: []string{"debater-pro", "debater-con"}, MaxTurns: 1,
	}); err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "debate-agent", Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded || final.Result != "共识结论：1+2=3" {
		t.Fatalf("status=%s result=%q err=%s", final.Status, final.Result, final.ErrorMsg)
	}
}

// —— 10. Blackboard ——

func TestBlackboardFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`黑板写入：1+2 用加法，结果 3`,
		`黑板汇总答案：3`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "scriber", AgentKey: "scriber", GraphType: GraphSimple,
	}); err != nil {
		t.Fatalf("create scriber: %v", err)
	}
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "黑板", AgentKey: "board-agent", GraphType: GraphBlackboard,
		Members: []string{"scriber"}, MaxTurns: 1,
	}); err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "board-agent", Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded || final.Result != "黑板汇总答案：3" {
		t.Fatalf("status=%s result=%q err=%s", final.Status, final.Result, final.ErrorMsg)
	}
}

// —— 校验 ——

func TestGraphTypesValidation(t *testing.T) {
	for _, g := range GraphTypes {
		if !ValidGraphType(g) {
			t.Fatalf("%s 应合法", g)
		}
	}
	if ValidGraphType("nope") {
		t.Fatalf("未知执行图应不合法")
	}
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "坏图", AgentKey: "bad-graph", GraphType: "nope",
	}); err == nil {
		t.Fatalf("未知执行图应被拒绝")
	}
	// 需要成员的多 Agent 模式，缺成员时执行应报错而不是空转
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "空辩论", AgentKey: "empty-debate", GraphType: GraphDebate,
	}); err != nil {
		t.Fatalf("定义可保存（执行期校验成员）: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "empty-debate", Input: "x"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskFailed {
		t.Fatalf("无成员的辩论应失败，got status=%s result=%q", final.Status, final.Result)
	}
}
