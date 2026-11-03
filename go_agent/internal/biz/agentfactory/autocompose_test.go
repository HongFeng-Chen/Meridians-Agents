package agentfactory

// 自动编排：LLM 判定架构与成员 → 自动创建子 Agent → 按选定图执行。

import (
	"context"
	"strings"
	"testing"
)

func TestAutoComposeBuildsDebateTeam(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"graph_type":"debate","reason":"结论需要正反校验","members":[
			{"agent_key":"Solution-Author","name":"方案作者","description":"产出可落地方案","duty":"出方案","system":"你是正方"},
			{"agent_key":"risk_critic","name":"风险批评者","description":"挑错并给改进建议","duty":"挑错","system":"你是反方"}]}`,
		`{"thought":"出方案","final":"方案 v1"}`,
		`{"thought":"挑错","final":"问题清单"}`,
		`{"thought":"修订","final":"方案 v2"}`,
		`最终结论`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "自动评审", AgentKey: "auto-review", GraphType: GraphAuto, MaxTurns: 1,
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "评审一份技术方案"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("任务应成功: status=%s err=%s", final.Status, final.ErrorMsg)
	}
	if final.Result != "最终结论" {
		t.Fatalf("result=%q", final.Result)
	}

	// graph_type=auto 的定义保持 auto（语义 = 每次任务自适应），成员回写供下次复用
	got, _ := repo.GetDef(context.Background(), def.ID)
	if got.GraphType != GraphAuto {
		t.Fatalf("auto 定义不应被固化成具体图，实际 %s", got.GraphType)
	}
	if len(got.Members) != 2 || got.Members[0] != "solution-author" || got.Members[1] != "risk-critic" {
		t.Fatalf("成员回写错误（key 应清洗为小写连字符、顺序即正反方）: %v", got.Members)
	}

	// 成员 Agent 已落库：role=sub、sandbox=true
	for _, key := range []string{"solution-author", "risk-critic"} {
		m, err := repo.GetDefByKey(context.Background(), key)
		if err != nil || m == nil {
			t.Fatalf("成员 %s 未创建: %v", key, err)
		}
		if m.Role != RoleSub || !m.Sandbox || m.GraphType != GraphReact {
			t.Fatalf("成员 %s 属性错误: role=%s sandbox=%v graph=%s", key, m.Role, m.Sandbox, m.GraphType)
		}
	}

	types := typesOf(u, final)
	if types[EventOrchestrate] != 1 {
		t.Fatalf("应有 1 条 orchestrate 事件，实际 %v", types)
	}
	if types[EventDelegate] < 3 {
		t.Fatalf("辩论应产生至少 3 次委派（正方/反方/修订），实际 %v", types)
	}
}

func TestAutoComposeFillsMembersForDebateGraph(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"graph_type":"debate","reason":"用户选了辩论图","members":[
			{"agent_key":"pro-side","name":"正方","description":"出方案","system":"正方"},
			{"agent_key":"con-side","name":"反方","description":"挑错","system":"反方"}]}`,
		`{"thought":"出方案","final":"方案"}`,
		`{"thought":"挑错","final":"意见"}`,
		`{"thought":"修订","final":"方案 v2"}`,
		`结论`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "技术评审", AgentKey: "tech-review", GraphType: GraphDebate, MaxTurns: 1,
	})
	if err != nil {
		t.Fatalf("debate 图允许空成员创建（运行时自动补）: %v", err)
	}
	task, _ := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "评审方案"})
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("任务应成功: %s / %s", final.Status, final.ErrorMsg)
	}
	// 非 auto 的定义固化本次规划结果，控制台可直接看到成员
	got, _ := repo.GetDef(context.Background(), def.ID)
	if got.GraphType != GraphDebate || len(got.Members) != 2 {
		t.Fatalf("应固化 debate + 2 成员，实际 graph=%s members=%v", got.GraphType, got.Members)
	}
}

func TestAutoComposeFallsBackWhenPlanInvalid(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`我无法判断该用哪种架构`, // 没有 JSON → 规划失败
		`{"thought":"直接答","final":"兜底答案"}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "自动助手", AgentKey: "auto-fallback", GraphType: GraphAuto, MaxTurns: 2,
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, _ := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "随口问一句"})
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded || final.Result != "兜底答案" {
		t.Fatalf("规划失败应回落 react 并成功: status=%s result=%q err=%s", final.Status, final.Result, final.ErrorMsg)
	}
	types := typesOf(u, final)
	if types[EventOrchestrate] != 0 || types[EventError] == 0 {
		t.Fatalf("应记 error 而不是 orchestrate，实际 %v", types)
	}
}

func TestNormalizePlan(t *testing.T) {
	root := &AgentDef{AgentKey: "root-agent", GraphType: GraphDebate}

	// 单 Agent 图：模型给的成员一律忽略
	p := normalizePlan(&autoPlan{GraphType: GraphReact, Members: []autoMember{{AgentKey: "x"}}}, root)
	if p.GraphType != GraphReact || len(p.Members) != 0 {
		t.Fatalf("单 Agent 图不应保留成员: %+v", p)
	}

	// 非法图：回落用户显式配置
	p = normalizePlan(&autoPlan{GraphType: "not-a-graph"}, root)
	if p.GraphType != GraphDebate {
		t.Fatalf("非法图应回落配置值 debate，实际 %s", p.GraphType)
	}

	// orchestrator 超量裁剪到 5，且 key 清洗 / 去重 / 排除自身
	members := make([]autoMember, 0, 7)
	for _, k := range []string{"A One", "b_two", "c.three", "d-four", "e/five", "g-six", "root-agent"} {
		members = append(members, autoMember{AgentKey: k, Name: k})
	}
	p = normalizePlan(&autoPlan{GraphType: GraphOrchestrator, Members: members}, root)
	if len(p.Members) != 5 {
		t.Fatalf("orchestrator 成员应裁剪到 5，实际 %d", len(p.Members))
	}
	if p.Members[0].AgentKey != "a-one" || p.Members[1].AgentKey != "b-two" || p.Members[2].AgentKey != "c-three" {
		t.Fatalf("agent_key 清洗错误: %+v", p.Members)
	}

	// debate 超过 2 个裁剪到 2（顺序保留）
	p = normalizePlan(&autoPlan{GraphType: GraphDebate, Members: []autoMember{
		{AgentKey: "pro"}, {AgentKey: "con"}, {AgentKey: "third"},
	}}, root)
	if len(p.Members) != 2 || p.Members[0].AgentKey != "pro" || p.Members[1].AgentKey != "con" {
		t.Fatalf("debate 应只保留前两个成员: %+v", p.Members)
	}

	// 空 system 用职责兜底
	if strings.TrimSpace(p.Members[0].System) == "" {
		t.Fatalf("空 system 应有兜底内容")
	}
}

func TestMemberCapabilitiesSubset(t *testing.T) {
	def := &AgentDef{Capabilities: []string{"mcp:demo__calc", CapFactory, "agent:other", "mcp:demo__calc", "skill:writer"}}
	got := memberCapabilities(def)
	want := []string{"mcp:demo__calc", "skill:writer"}
	if len(got) != len(want) {
		t.Fatalf("成员能力应剔除 create_agent / agent:* 并去重: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("成员能力 = %v，期望 %v", got, want)
		}
	}
}
