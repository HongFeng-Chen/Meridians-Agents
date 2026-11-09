package agentfactory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go_agent/internal/biz/llm"
)

// dagFlow 生成 root → t1 →（t2/t3 并行）→ 条件 → t4(是)/t5(否) → output
func dagFlow(cond FlowCond) string {
	g := FlowGraph{
		Nodes: []FlowNode{
			{ID: "n1", Kind: FlowNodeAgent, AgentKey: "dag-t1", X: 80, Y: 100},
			{ID: "n2", Kind: FlowNodeAgent, AgentKey: "dag-t2", X: 220, Y: 40},
			{ID: "n3", Kind: FlowNodeAgent, AgentKey: "dag-t3", X: 220, Y: 160},
			{ID: "c1", Kind: FlowNodeCondition, Cond: &cond, X: 360, Y: 100},
			{ID: "n4", Kind: FlowNodeAgent, AgentKey: "dag-t4", X: 500, Y: 40},
			{ID: "n5", Kind: FlowNodeAgent, AgentKey: "dag-t5", X: 500, Y: 160},
			{ID: "out", Kind: FlowNodeOutput, X: 640, Y: 100},
		},
		Edges: []FlowEdge{
			{From: "n1", To: "n2"},
			{From: "n1", To: "n3"},
			{From: "n2", To: "c1"},
			{From: "n3", To: "c1"},
			{From: "c1", To: "n4", When: "true"},
			{From: "c1", To: "n5", When: "false"},
			{From: "n4", To: "out"},
			{From: "n5", To: "out"},
		},
	}
	raw, _ := json.Marshal(g)
	return string(raw)
}

// TestFlowGraphMixedSerialParallel 串并行混排 + 条件分支
func TestFlowGraphMixedSerialParallel(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &wfChatter{})
	ctx := context.Background()

	for _, key := range []string{"dag-t1", "dag-t2", "dag-t3", "dag-t4", "dag-t5"} {
		if _, err := u.CreateDef(ctx, AgentInput{AgentKey: key, Name: key, GraphType: GraphSimple}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}

	def, err := u.CreateDef(ctx, AgentInput{
		AgentKey: "dag-root", Name: "DAG 根", Role: RoleRoot,
		GraphType: GraphSimple,
		// 条件命中「是」分支（子 Agent 输出固定为 CHILD-ANSWER）
		Flow: dagFlow(FlowCond{Mode: CondModeRule, Op: CondOpContains, Value: "CHILD"}),
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "dag-root", Input: "跑一遍混排流程"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("任务应成功，实际 %s / %s", final.Status, final.ErrorMsg)
	}
	if final.Result != "FINAL-ANSWER" {
		t.Fatalf("输出节点应给汇总答案，实际 %q", final.Result)
	}

	// 子任务：t1/t2/t3/t4 执行，t5 因条件未命中被跳过
	ran := map[string]int{}
	repo.mu.Lock()
	for _, tk := range repo.tasks {
		if tk.ParentTaskID == task.ID {
			ran[tk.AgentKey]++
		}
	}
	var nodeEvents []TaskEvent
	for _, e := range repo.events {
		if e.TaskID == task.ID && e.Type == EventNode {
			nodeEvents = append(nodeEvents, e)
		}
	}
	repo.mu.Unlock()

	for _, key := range []string{"dag-t1", "dag-t2", "dag-t3", "dag-t4"} {
		if ran[key] != 1 {
			t.Fatalf("%s 应执行 1 次，实际 %d（全量子任务 %v）", key, ran[key], ran)
		}
	}
	if ran["dag-t5"] != 0 {
		t.Fatalf("条件未命中分支不应执行，实际 %d", ran["dag-t5"])
	}

	// 条件节点应记一次判断事件，且分支为 true
	branch := ""
	for _, e := range nodeEvents {
		var p map[string]any
		_ = json.Unmarshal([]byte(e.Payload), &p)
		if p["kind"] == FlowNodeCondition {
			branch, _ = p["branch"].(string)
		}
	}
	if branch != "true" {
		t.Fatalf("条件应走 true 分支，实际 %q", branch)
	}

	// flow 应随定义持久化
	got, err := u.GetDef(ctx, def.ID)
	if err != nil {
		t.Fatalf("GetDef: %v", err)
	}
	if !strings.Contains(got.Flow, `"nodes"`) {
		t.Fatalf("flow 应保存为图结构，实际 %q", truncateRunes(got.Flow, 80))
	}
}

// TestFlowGraphConditionFalseBranch 条件走「否」分支
func TestFlowGraphConditionFalseBranch(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &wfChatter{})
	ctx := context.Background()

	for _, key := range []string{"dag-t1", "dag-t2", "dag-t3", "dag-t4", "dag-t5"} {
		if _, err := u.CreateDef(ctx, AgentInput{AgentKey: key, Name: key, GraphType: GraphSimple}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}
	if _, err := u.CreateDef(ctx, AgentInput{
		AgentKey: "dag-root2", Name: "DAG 根2", Role: RoleRoot, GraphType: GraphSimple,
		Flow: dagFlow(FlowCond{Mode: CondModeRule, Op: CondOpContains, Value: "不存在的关键字"}),
	}); err != nil {
		t.Fatalf("create root: %v", err)
	}

	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "dag-root2", Input: "跑一遍"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("任务应成功，实际 %s / %s", final.Status, final.ErrorMsg)
	}

	ran := map[string]int{}
	repo.mu.Lock()
	for _, tk := range repo.tasks {
		if tk.ParentTaskID == task.ID {
			ran[tk.AgentKey]++
		}
	}
	repo.mu.Unlock()
	if ran["dag-t5"] != 1 {
		t.Fatalf("条件未命中应走 false 分支执行 t5，实际 %d", ran["dag-t5"])
	}
	if ran["dag-t4"] != 0 {
		t.Fatalf("true 分支不应执行，实际 %d", ran["dag-t4"])
	}
}

// TestFlowGraphValidate 结构校验：环 / 缺输出 / 悬空边
func TestFlowGraphValidate(t *testing.T) {
	base := func() FlowGraph {
		return FlowGraph{
			Nodes: []FlowNode{
				{ID: "a", Kind: FlowNodeAgent, AgentKey: "x"},
				{ID: "b", Kind: FlowNodeAgent, AgentKey: "y"},
				{ID: "out", Kind: FlowNodeOutput},
			},
			Edges: []FlowEdge{{From: "a", To: "b"}, {From: "b", To: "out"}},
		}
	}

	g := base()
	if err := g.Validate(); err != nil {
		t.Fatalf("合法图不应报错: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*FlowGraph)
		want string
	}{
		{"成环", func(g *FlowGraph) { g.Edges = append(g.Edges, FlowEdge{From: "b", To: "a"}) }, "环"},
		{"缺输出", func(g *FlowGraph) { g.Nodes = g.Nodes[:2] }, "输出"},
		{"悬空边", func(g *FlowGraph) { g.Edges = append(g.Edges, FlowEdge{From: "a", To: "ghost"}) }, "不存在"},
		{"条件无配置", func(g *FlowGraph) {
			g.Nodes[0].Kind = FlowNodeCondition
		}, "条件"},
	}
	for _, c := range cases {
		gg := base()
		c.mut(&gg)
		err := gg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：期望包含 %q 的错误，实际 %v", c.name, c.want, err)
		}
	}
}

// TestCreateDefRejectsBadFlow 定义保存时拦截非法编排图
func TestCreateDefRejectsBadFlow(t *testing.T) {
	u, _, _ := newTestUseCase(t, &wfChatter{})
	ctx := context.Background()
	bad := `{"nodes":[{"id":"a","kind":"agent","agent_key":"x"},{"id":"b","kind":"agent","agent_key":"x"}],"edges":[{"from":"a","to":"b"},{"from":"b","to":"a"}]}`
	if _, err := u.CreateDef(ctx, AgentInput{AgentKey: "bad-flow", Name: "坏图", Flow: bad}); err == nil {
		t.Fatal("成环的编排图应被拒绝")
	}
}

// —— P1-06：节点失败不再激活下游 ——

// boomChatter 只让「坏节点」的模型调用失败，其余照常，
// 这样失败发生在子任务执行期（子任务记录存在），覆盖真实场景。
type boomChatter struct{ wfChatter }

func (c *boomChatter) ChatTemp(ctx context.Context, system, user string, temp float64) (string, error) {
	if strings.Contains(system, "BOOM-ONLY") {
		return "", errors.New("模型调用失败")
	}
	return c.wfChatter.ChatTemp(ctx, system, user, temp)
}

func (c *boomChatter) Chat(ctx context.Context, system, user string) (string, error) {
	return c.ChatTemp(ctx, system, user, 0)
}

func (c *boomChatter) ChatSource(ctx context.Context, _ string, system, user string, temp float64) (string, error) {
	return c.ChatTemp(ctx, system, user, temp)
}

// ChatOpts 是执行期的实际入口（chatWith → ChatOpts），必须一并覆盖
func (c *boomChatter) ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error) {
	return c.ChatTemp(ctx, o.System, o.User, o.Temperature)
}

// failureFlow 构造 root 图：bad 节点必然失败，good 节点可正常执行，out 汇总。
func failureFlow(when string) string {
	g := FlowGraph{
		Nodes: []FlowNode{
			{ID: "bad", Kind: FlowNodeAgent, AgentKey: "p1-bad"},
			{ID: "good", Kind: FlowNodeAgent, AgentKey: "p1-ok"},
			{ID: "out", Kind: FlowNodeOutput},
		},
		Edges: []FlowEdge{
			{From: "bad", To: "out"},
			{From: "bad", To: "good", When: when},
			{From: "good", To: "out"},
		},
	}
	raw, _ := json.Marshal(g)
	return string(raw)
}

func runFailureFlow(t *testing.T, when, key string) (*Task, map[string]int, map[string]string) {
	t.Helper()
	u, repo, _ := newTestUseCase(t, &boomChatter{})
	ctx := context.Background()
	if _, err := u.CreateDef(ctx, AgentInput{AgentKey: "p1-bad", Name: "坏节点", GraphType: GraphSimple, System: "BOOM-ONLY"}); err != nil {
		t.Fatalf("create bad agent: %v", err)
	}
	if _, err := u.CreateDef(ctx, AgentInput{AgentKey: "p1-ok", Name: "正常 Agent", GraphType: GraphSimple}); err != nil {
		t.Fatalf("create ok agent: %v", err)
	}
	if _, err := u.CreateDef(ctx, AgentInput{
		AgentKey: key, Name: "失败图根", Role: RoleRoot, GraphType: GraphSimple,
		Flow: failureFlow(when),
	}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: key, Input: "跑一遍"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	final := runTask(t, u, task.ID)

	ran := map[string]int{}
	status := map[string]string{}
	repo.mu.Lock()
	for _, tk := range repo.tasks {
		if tk.ParentTaskID == task.ID {
			ran[tk.AgentKey]++
		}
	}
	for _, e := range repo.events {
		if e.TaskID != task.ID || e.Type != EventNode {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(e.Payload), &p)
		if id, ok := p["node"].(string); ok {
			status[id], _ = p["status"].(string)
		}
	}
	repo.mu.Unlock()
	return final, ran, status
}

// 默认边（无 when）：上游节点失败 → 下游不执行，失败不会被当成正常输入流下去。
func TestFlowNodeFailureDoesNotActivateDownstream(t *testing.T) {
	final, ran, status := runFailureFlow(t, "", "p1-fail-root")
	if ran["p1-bad"] != 1 {
		t.Fatalf("失败节点应执行 1 次，实际 %d（%v）", ran["p1-bad"], ran)
	}
	if ran["p1-ok"] != 0 {
		t.Fatalf("上游失败不应激活下游节点，实际执行 %d 次", ran["p1-ok"])
	}
	if !strings.Contains(final.Result, "执行失败") {
		t.Fatalf("兜底产出应包含失败信息，实际 %q", truncateRunes(final.Result, 120))
	}
	if status["bad"] != "error" {
		t.Fatalf("失败节点应记 error 状态，实际 %v", status)
	}
	if status["good"] != "skipped" {
		t.Fatalf("下游节点应跳过，实际 %v", status)
	}
}

// 显式 on_error 边（when=failure）：失败也要继续时把意图写在边上。
func TestFlowFailureEdgeOptIn(t *testing.T) {
	_, ran, _ := runFailureFlow(t, "failure", "p1-fail-root2")
	if ran["p1-ok"] != 1 {
		t.Fatalf("显式 failure 边应放行下游，实际执行 %d 次", ran["p1-ok"])
	}
}

// 出边 when 只接受约定的取值，且 true/false 仅限条件节点。
func TestFlowEdgeWhenValidation(t *testing.T) {
	build := func(when string, cond bool) *FlowGraph {
		nodes := []FlowNode{
			{ID: "a", Kind: FlowNodeAgent, AgentKey: "x"},
			{ID: "out", Kind: FlowNodeOutput},
		}
		if cond {
			nodes[0] = FlowNode{ID: "a", Kind: FlowNodeCondition, Cond: &FlowCond{Mode: CondModeRule, Op: CondOpContains, Value: "x"}}
		}
		return &FlowGraph{Nodes: nodes, Edges: []FlowEdge{{From: "a", To: "out", When: when}}}
	}
	if err := build("always", false).Validate(); err != nil {
		t.Fatalf("always 应合法: %v", err)
	}
	if err := build("true", true).Validate(); err != nil {
		t.Fatalf("条件节点的 true 分支应合法: %v", err)
	}
	if err := build("true", false).Validate(); err == nil {
		t.Fatal("非条件节点不应允许 true/false 边")
	}
	if err := build("sometimes", false).Validate(); err == nil {
		t.Fatal("未知 when 值应被拒绝")
	}
}

// —— 第四阶段：条件判断失败必须显式报错，不能当成「条件不成立」 ——

// evalCond 只在能得出结论时返回 true/false；配置错误 / 数据不可比较返回 error。
func TestEvalCondExplicitErrors(t *testing.T) {
	cases := []struct {
		name string
		cond *FlowCond
		text string
		pass bool
		err  string
	}{
		{"contains 命中", &FlowCond{Mode: CondModeRule, Op: CondOpContains, Value: "完成"}, "任务已完成", true, ""},
		{"contains 缺比较值", &FlowCond{Mode: CondModeRule, Op: CondOpContains}, "任何内容", false, "缺少比较值"},
		{"未知运算符", &FlowCond{Mode: CondModeRule, Op: "match"}, "x", false, "未知运算符"},
		{"gt 比较值非数字", &FlowCond{Mode: CondModeRule, Op: CondOpGt, Value: "很多"}, "共 3 条", false, "不是数字"},
		{"gt 文本里没数字", &FlowCond{Mode: CondModeRule, Op: CondOpGt, Value: "3"}, "没有任何数字", false, "没有可比较的数字"},
		{"gt 正常比较", &FlowCond{Mode: CondModeRule, Op: CondOpGt, Value: "3"}, "共 5 条", true, ""},
		{"is_empty", &FlowCond{Mode: CondModeRule, Op: CondOpIsEmpty}, "   ", true, ""},
	}
	for _, c := range cases {
		pass, err := evalCond(c.cond, c.text)
		if c.err == "" {
			if err != nil || pass != c.pass {
				t.Fatalf("%s：期望 pass=%v，实际 pass=%v err=%v", c.name, c.pass, pass, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Fatalf("%s：期望错误包含 %q，实际 %v", c.name, c.err, err)
		}
	}
}

// parseYesNo 只接受明确结论；模棱两可时 ok=false 由调用方转成错误。
func TestParseYesNo(t *testing.T) {
	yes := []string{"YES", "yes.", " 是", "是，满足要求", "答案是 YES"}
	no := []string{"NO", "no", "否", "不是", "答案为 NO"}
	unclear := []string{"", "CHILD-ANSWER", "无法判断", "YES 也可能是 NO"}
	for _, s := range yes {
		if pass, ok := parseYesNo(s); !ok || !pass {
			t.Fatalf("%q 应判为是，实际 pass=%v ok=%v", s, pass, ok)
		}
	}
	for _, s := range no {
		if pass, ok := parseYesNo(s); !ok || pass {
			t.Fatalf("%q 应判为否，实际 pass=%v ok=%v", s, pass, ok)
		}
	}
	for _, s := range unclear {
		if _, ok := parseYesNo(s); ok {
			t.Fatalf("%q 不应给出结论", s)
		}
	}
}

// 模型判断失败 → 条件节点标 failed，两条分支都不激活（不猜一条继续跑）。
func TestFlowConditionErrorActivatesNoBranch(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &wfChatter{}) // 非条件提示词一律回 "CHILD-ANSWER"（不可解析）
	ctx := context.Background()
	for _, key := range []string{"dag-t1", "dag-t2", "dag-t3", "dag-t4", "dag-t5"} {
		if _, err := u.CreateDef(ctx, AgentInput{AgentKey: key, Name: key, GraphType: GraphSimple}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}
	if _, err := u.CreateDef(ctx, AgentInput{
		AgentKey: "cond-err-root", Name: "条件失败根", Role: RoleRoot, GraphType: GraphSimple,
		Flow: dagFlow(FlowCond{Mode: CondModeLLM, Question: "上游产出是否达标？"}),
	}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "cond-err-root", Input: "跑一遍"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	final := runTask(t, u, task.ID)

	ran := map[string]int{}
	condStatus := ""
	repo.mu.Lock()
	for _, tk := range repo.tasks {
		if tk.ParentTaskID == task.ID {
			ran[tk.AgentKey]++
		}
	}
	for _, e := range repo.events {
		if e.TaskID != task.ID || e.Type != EventNode {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(e.Payload), &p)
		if p["node"] == "c1" {
			condStatus, _ = p["status"].(string)
		}
	}
	repo.mu.Unlock()

	if condStatus != "error" {
		t.Fatalf("条件节点应记 error 状态，实际 %q", condStatus)
	}
	if ran["dag-t4"] != 0 || ran["dag-t5"] != 0 {
		t.Fatalf("条件判断失败时两条分支都不应执行，实际 t4=%d t5=%d", ran["dag-t4"], ran["dag-t5"])
	}
	if !strings.Contains(final.Result, "条件判断失败") {
		t.Fatalf("兜底产出应说明条件判断失败，实际 %q", truncateRunes(final.Result, 120))
	}
}

// —— 第四阶段：DAG 节点级续跑 ——

// 已完成节点在重启后不再执行：产出直接复用，子任务不重建（副作用只发生一次）。
func TestFlowGraphResumeSkipsDoneNodes(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &wfChatter{})
	ctx := context.Background()
	for _, key := range []string{"dag-t1", "dag-t2", "dag-t3", "dag-t4", "dag-t5"} {
		if _, err := u.CreateDef(ctx, AgentInput{AgentKey: key, Name: key, GraphType: GraphSimple}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}
	if _, err := u.CreateDef(ctx, AgentInput{
		AgentKey: "dag-resume-root", Name: "DAG 续跑根", Role: RoleRoot, GraphType: GraphSimple,
		Flow: dagFlow(FlowCond{Mode: CondModeRule, Op: CondOpContains, Value: "CHILD"}),
	}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "dag-resume-root", Input: "跑一遍混排流程"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 预置：入口节点 n1 已完成、两条出边已决断（崩溃发生在 n2/n3 之前）
	st := newState("跑一遍混排流程")
	st.NodeState = map[string]NodeRecord{"n1": {Status: nodeStatusDone, Output: "上游已完成"}}
	st.EdgeState = map[int]string{0: edgeStatusActive, 1: edgeStatusActive}
	if err := repo.SaveCheckpoint(ctx, &TaskCheckpoint{
		TaskID: task.ID, Seq: 1, GraphType: GraphSimple,
		Cursor: `{"kind":"node","node":"n1","status":"done"}`, State: encodeState(st),
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("续跑失败: %s / %s", final.Status, final.ErrorMsg)
	}
	if final.Result != "FINAL-ANSWER" {
		t.Fatalf("结果应为汇总答案，实际 %q", final.Result)
	}

	ran := map[string]int{}
	repo.mu.Lock()
	for _, tk := range repo.tasks {
		if tk.ParentTaskID == task.ID {
			ran[tk.AgentKey]++
		}
	}
	repo.mu.Unlock()
	if ran["dag-t1"] != 0 {
		t.Fatalf("已完成节点不应重跑，实际执行 %d 次", ran["dag-t1"])
	}
	for _, key := range []string{"dag-t2", "dag-t3", "dag-t4"} {
		if ran[key] != 1 {
			t.Fatalf("%s 应执行 1 次，实际 %d（%v）", key, ran[key], ran)
		}
	}
	if ran["dag-t5"] != 0 {
		t.Fatalf("条件未命中分支不应执行，实际 %d", ran["dag-t5"])
	}

	// 最终 checkpoint 里应能看到全部节点状态，条件节点的分支也要入档
	cp, _ := repo.LatestCheckpoint(ctx, task.ID)
	if cp == nil {
		t.Fatal("应有 checkpoint")
	}
	state := decodeState(cp.State)
	if state == nil {
		t.Fatal("checkpoint 状态不可解析")
	}
	for _, id := range []string{"n1", "n2", "n3", "c1", "n4", "n5", "out"} {
		if _, ok := state.NodeState[id]; !ok {
			t.Fatalf("节点 %s 的状态应入档，实际 %+v", id, state.NodeState)
		}
	}
	if br := state.NodeState["c1"].Branch; br == nil || !*br {
		t.Fatalf("条件节点应记录分支判定（true），实际 %v", br)
	}
	if got := state.NodeState["n1"].Output; got != "上游已完成" {
		t.Fatalf("已完成节点的产出不应被改写，实际 %q", got)
	}
}

// 失败节点在重新提交时重试：出边判定一并复位，重试成功后下游照常执行。
func TestFlowGraphRetriesFailedNodeOnResume(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &wfChatter{})
	ctx := context.Background()
	for _, key := range []string{"dag-t1", "dag-t2", "dag-t3", "dag-t4", "dag-t5"} {
		if _, err := u.CreateDef(ctx, AgentInput{AgentKey: key, Name: key, GraphType: GraphSimple}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}
	if _, err := u.CreateDef(ctx, AgentInput{
		AgentKey: "dag-retry-root", Name: "DAG 重试根", Role: RoleRoot, GraphType: GraphSimple,
		Flow: dagFlow(FlowCond{Mode: CondModeRule, Op: CondOpContains, Value: "CHILD"}),
	}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "dag-retry-root", Input: "跑一遍混排流程"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 预置：n1 上次失败，两条出边因此被判死
	st := newState("跑一遍混排流程")
	st.NodeState = map[string]NodeRecord{"n1": {Status: nodeStatusFailed, Output: "[执行失败] 上次挂了"}}
	st.EdgeState = map[int]string{0: edgeStatusDead, 1: edgeStatusDead}
	if err := repo.SaveCheckpoint(ctx, &TaskCheckpoint{
		TaskID: task.ID, Seq: 1, GraphType: GraphSimple,
		Cursor: `{"kind":"node","node":"n1","status":"failed"}`, State: encodeState(st),
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("重试失败: %s / %s", final.Status, final.ErrorMsg)
	}
	if final.Result != "FINAL-ANSWER" {
		t.Fatalf("重试后应正常汇总，实际 %q", final.Result)
	}
	ran := map[string]int{}
	repo.mu.Lock()
	for _, tk := range repo.tasks {
		if tk.ParentTaskID == task.ID {
			ran[tk.AgentKey]++
		}
	}
	repo.mu.Unlock()
	if ran["dag-t1"] != 1 {
		t.Fatalf("失败节点应重试一次，实际 %d", ran["dag-t1"])
	}
	if ran["dag-t2"] != 1 || ran["dag-t3"] != 1 || ran["dag-t4"] != 1 {
		t.Fatalf("重试成功后下游应正常执行，实际 %v", ran)
	}
}
