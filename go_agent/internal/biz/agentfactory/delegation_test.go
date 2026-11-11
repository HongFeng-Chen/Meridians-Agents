package agentfactory

// P3 委派上下文测试：多级委派单调收紧、通配覆盖、深度闸门、归属继承。

import (
	"context"
	"errors"
	"testing"
)

func TestDelegatedCapsRootUnrestricted(t *testing.T) {
	child := &AgentDef{AgentKey: "powerful", Capabilities: []string{"mcp:db__drop", "agent:x"}}
	// 根任务（DelegatedCaps == nil）不额外约束：子 Agent 按自身声明执行，
	// 委派授权由 `agent:<key>` + 运行时判权兜底。
	if got := DelegatedCaps(&Task{ID: 1}, child); got != nil {
		t.Fatalf("根任务不应限制子能力，got %v", got)
	}
	if got := DelegatedCaps(nil, child); got != nil {
		t.Fatalf("无父任务不应限制，got %v", got)
	}
}

func TestDelegatedCapsChainTightens(t *testing.T) {
	parent := &Task{ID: 1, DelegatedCaps: []string{"mcp:read_*", "mcp:a", "agent:x"}}
	child := &AgentDef{AgentKey: "child", Capabilities: []string{
		"mcp:a", "mcp:b", "mcp:read_doc", "agent:x", "agent:y", "create_agent",
	}}
	got := DelegatedCaps(parent, child)
	want := []string{"mcp:a", "mcp:read_doc", "agent:x"}
	if len(got) != len(want) {
		t.Fatalf("交集不符: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("交集不符: %v", got)
		}
	}

	// 父级收紧到空：子任务一项都拿不到（非 nil 空切片，表示「受限且全禁」）
	empty := DelegatedCaps(&Task{DelegatedCaps: []string{"mcp:z"}}, child)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("应为受限空集，got %#v", empty)
	}
}

func TestCapCoveredWildcardRules(t *testing.T) {
	cases := []struct {
		name   string
		parent []string
		cap    string
		want   bool
	}{
		{"精确匹配", []string{"mcp:a"}, "mcp:a", true},
		{"父级通配覆盖子级精确", []string{"mcp:read_*"}, "mcp:read_doc", true},
		{"父级通配不覆盖别的前缀", []string{"mcp:read_*"}, "mcp:write_doc", false},
		{"父级是子级的前缀但无通配", []string{"mcp:read"}, "mcp:read_doc", false},
		{"父级与子级通配一致", []string{"mcp:read_*"}, "mcp:read_*", true},
		{"通配套通配不推导", []string{"mcp:r*"}, "mcp:read_*", false},
		{"空能力名不算覆盖", []string{"mcp:a"}, "", false},
	}
	for _, c := range cases {
		if got := capCovered(c.parent, c.cap); got != c.want {
			t.Fatalf("%s: capCovered(%v, %q) = %v, want %v", c.name, c.parent, c.cap, got, c.want)
		}
	}
}

func TestApplyDelegation(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	parent := &Task{ID: 10, UserID: "u1", TenantID: "acme", ConvID: "conv-1", Depth: 1}
	childDef := &AgentDef{AgentKey: "child", Capabilities: []string{"mcp:a"}}

	// 未受限父任务：子任务继承归属，能力不额外限制
	child := &Task{Depth: 2}
	if err := u.ApplyDelegation(parent, child, childDef); err != nil {
		t.Fatalf("ApplyDelegation: %v", err)
	}
	if child.DelegatedCaps != nil {
		t.Fatalf("根链路不应受限，got %v", child.DelegatedCaps)
	}
	if child.UserID != "u1" || child.TenantID != "acme" || child.ConvID != "conv-1" {
		t.Fatalf("归属未继承: %+v", child)
	}

	// 受限父任务：交集为空且子 Agent 有声明能力 → 直接拒绝建任务
	restricted := &Task{ID: 11, UserID: "u1", TenantID: "acme", DelegatedCaps: []string{"mcp:z"}}
	if err := u.ApplyDelegation(restricted, &Task{Depth: 2}, childDef); !errors.Is(err, ErrDelegationDeny) {
		t.Fatalf("交集为空应拒绝，got %v", err)
	}

	// 深度超限：默认 5，Depth=6 直接拒绝
	deep := &Task{ID: 12}
	if err := u.ApplyDelegation(deep, &Task{Depth: MaxDelegationDepth + 1}, childDef); !errors.Is(err, ErrDelegationDeny) {
		t.Fatalf("深度超限应拒绝，got %v", err)
	}
}

// 执行期的能力判定必须走委派上下文：受限子任务拿不到未下放的能力。
func TestCapabilityAllowedHonorsDelegation(t *testing.T) {
	def := &AgentDef{AgentKey: "child", Capabilities: []string{"mcp:a", "mcp:b"}}
	restricted := &Task{ID: 1, DelegatedCaps: []string{"mcp:a"}}

	if !capabilityAllowed(restricted, def, "mcp:a") {
		t.Fatal("已下放的能力应放行")
	}
	if capabilityAllowed(restricted, def, "mcp:b") {
		t.Fatal("未下放的能力不应放行（定义里写了也不行）")
	}
	// 未受限任务仍按定义
	if !capabilityAllowed(nil, def, "mcp:b") {
		t.Fatal("未受限任务应按定义放行")
	}
}

// 受限上下文里「空清单」= 全禁，不能退化成「不限」。
func TestEmptyDelegationDeniesAll(t *testing.T) {
	def := &AgentDef{AgentKey: "child", Capabilities: []string{"mcp:a"}}
	task := &Task{ID: 1, DelegatedCaps: []string{}}
	if capabilityAllowed(task, def, "mcp:a") {
		t.Fatal("空委派清单不应放行任何能力")
	}
	// 对照：nil 才是「未受限」，此时按定义放行
	if !capabilityAllowed(&Task{ID: 2}, def, "mcp:a") {
		t.Fatal("nil 委派清单应按定义放行")
	}
}

func TestDelegationRuntimeDeniesCapability(t *testing.T) {
	// 端到端：受限子任务调用未下放的工具 → 能力授权拒绝，任务失败
	chat := &fakeChatter{script: []string{
		`{"thought":"调工具","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	tools := &recordingTools{}
	useTools(u, tools)

	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "子", AgentKey: "child", GraphType: GraphReact, Capabilities: []string{"mcp:demo__calc"},
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task := &Task{
		AgentDefID: def.ID, AgentKey: def.AgentKey, Title: "受限执行", Input: "算一下",
		Status: TaskPending, UserID: "u1", TenantID: "acme",
		DelegatedCaps: []string{}, // 上游收紧到全禁
	}
	if err := repo.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskFailed {
		t.Fatalf("受限任务应失败，实际 %s", final.Status)
	}
	if tools.calls != 0 {
		t.Fatalf("未下放的工具不应被调用，实际 %d 次", tools.calls)
	}
}
