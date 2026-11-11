package agentfactory

// 能力白名单的运行期强制校验（SEC-001 / SEC-002）
//
// 这组用例的价值在于「证明越权请求根本没有到达工具层」，而不只是「返回了错误」：
// 只断言 error 非空，即使校验写在了工具调用之后也照样能过。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go_agent/internal/biz/mcp"
)

// recordingTools 记录真实调用次数，用来判断请求是否穿透到了工具层
type recordingTools struct {
	calls int
	last  string
}

func (r *recordingTools) ListTools(_ context.Context, _ mcp.ToolFilter) ([]mcp.Tool, error) {
	return []mcp.Tool{
		{ID: 1, UniqueName: "demo__calc", Description: "演示计算器", Enabled: true},
		{ID: 2, UniqueName: "prod-db__drop_table", Description: "删除表", Enabled: true},
	}, nil
}

func (r *recordingTools) CallTool(_ context.Context, _ uint64, uniqueName string, _ map[string]any) (*mcp.CallResult, error) {
	r.calls++
	r.last = uniqueName
	return &mcp.CallResult{Content: []mcp.ContentPart{{Type: "text", Text: "ok"}}}, nil
}

func newRec(u *UseCase, def *AgentDef) *Recorder {
	return &Recorder{u: u, task: &Task{ID: 1}, def: def}
}

// SEC-001：Agent 调用白名单外的 MCP 工具 → 拒绝，且不产生副作用
func TestCapabilityDenyMCPOutsideWhitelist(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	tools := &recordingTools{}
	u.tools = tools

	def := &AgentDef{AgentKey: "reader", Name: "只读 Agent", Capabilities: []string{"mcp:demo__calc"}}
	rec := newRec(u, def)
	ctx := context.Background()

	// 白名单外的高危工具：必须被拒，且工具层零调用
	_, err := u.InvokeDispatch(ctx, "mcp:prod-db__drop_table", map[string]any{"table": "billing"}, rec)
	if !errors.Is(err, ErrCapabilityDeny) {
		t.Fatalf("白名单外工具应返回 ErrCapabilityDeny，实际 %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("越权请求穿透到了工具层：calls=%d last=%s", tools.calls, tools.last)
	}

	// 白名单内：正常放行
	if _, err := u.InvokeDispatch(ctx, "mcp:demo__calc", map[string]any{"a": 1}, rec); err != nil {
		t.Fatalf("白名单内工具应放行: %v", err)
	}
	if tools.calls != 1 || tools.last != "demo__calc" {
		t.Fatalf("白名单内调用未落到工具层：calls=%d last=%s", tools.calls, tools.last)
	}
}

// SEC-002：Agent 调用白名单外的 Agent（委派）→ 拒绝
func TestCapabilityDenyDelegateOutsideWhitelist(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	def := &AgentDef{AgentKey: "solo", Name: "独立 Agent", Capabilities: []string{"mcp:demo__calc"}}
	rec := newRec(u, def)

	_, err := u.InvokeDispatch(context.Background(), "agent:other-agent", map[string]any{"input": "帮我跑一下"}, rec)
	if !errors.Is(err, ErrCapabilityDeny) {
		t.Fatalf("白名单外委派应被拒绝，实际 %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "未获授权") {
		t.Fatalf("错误文案应说明未授权，实际 %v", err)
	}
}

// 没有 Agent 上下文时不允许直接分发：否则任何调用方都能绕过白名单
func TestCapabilityDenyWithoutContext(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	tools := &recordingTools{}
	u.tools = tools

	if _, err := u.InvokeDispatch(context.Background(), "mcp:demo__calc", nil, nil); !errors.Is(err, ErrCapabilityDeny) {
		t.Fatalf("缺少 rec 应拒绝，实际 %v", err)
	}
	// rec 存在但 def 为空，同样拒绝（比如手工构造的执行器）
	rec := &Recorder{u: u, task: &Task{ID: 1}}
	if _, err := u.InvokeDispatch(context.Background(), "mcp:demo__calc", nil, rec); !errors.Is(err, ErrCapabilityDeny) {
		t.Fatalf("缺少 def 应拒绝，实际 %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("无上下文请求穿透到了工具层：calls=%d", tools.calls)
	}
}

// 沙箱 Agent：白名单里即便写了 create_agent / agent:* 也不放行
// （创建期会剔除，这里挡的是历史数据与手工改库）
func TestSandboxCannotEscalate(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	def := &AgentDef{
		AgentKey: "sandboxed", Name: "沙箱 Agent", Sandbox: true,
		Capabilities: []string{CapFactory, "agent:other", "mcp:demo__calc"},
	}
	rec := newRec(u, def)
	ctx := context.Background()

	if _, err := u.InvokeDispatch(ctx, CapFactory, map[string]any{"name": "x"}, rec); !errors.Is(err, ErrSandboxDeny) {
		t.Fatalf("沙箱自举应被拒绝，实际 %v", err)
	}
	if _, err := u.InvokeDispatch(ctx, "agent:other", map[string]any{"input": "x"}, rec); !errors.Is(err, ErrSandboxDeny) {
		t.Fatalf("沙箱委派应被拒绝，实际 %v", err)
	}
}

// 白名单精确匹配：前缀 / 通配都不能算授权
func TestCapabilityAllowedIsExactMatch(t *testing.T) {
	def := &AgentDef{Capabilities: []string{"mcp:demo__calc"}}

	if !capabilityAllowed(nil, def, "mcp:demo__calc") {
		t.Fatal("精确匹配应放行")
	}
	for _, name := range []string{"mcp:demo__calc_all", "mcp:demo__", "mcp:*", "demo__calc", "MCP:demo__calc"} {
		if capabilityAllowed(nil, def, name) {
			t.Fatalf("%q 不应被视为已授权（白名单不支持前缀/通配/大小写模糊）", name)
		}
	}
}
