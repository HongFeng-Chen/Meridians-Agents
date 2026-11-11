package agentfactory

// P3 运行时二次授权测试：未登记放行、登记后按当前主体判权、
// 判权存储故障拒绝（fail-closed）。

import (
	"context"
	"errors"
	"testing"

	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/principal"
)

// fakeAuthorizer 可控判权端口
type fakeAuthorizer struct {
	registered map[string]bool
	denied     map[string]bool
	regErr     error
	checkErr   error
	// checked 记录实际判权的资源 ID
	checked []string
}

func (f *fakeAuthorizer) Registered(_ context.Context, resourceID string) (bool, error) {
	if f.regErr != nil {
		return false, f.regErr
	}
	return f.registered[resourceID], nil
}

func (f *fakeAuthorizer) Check(_ context.Context, _ *principal.Principal, resourceID, action string) (bool, error) {
	if f.checkErr != nil {
		return false, f.checkErr
	}
	f.checked = append(f.checked, resourceID+"#"+action)
	return !f.denied[resourceID], nil
}

func withPrincipal(ctx context.Context, tenant, user string) context.Context {
	return identity.With(ctx, identity.Info{UserID: user, TenantID: tenant})
}

func TestRuntimeAuthorization(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	ctx := withPrincipal(context.Background(), "acme", "u1")
	def := &AgentDef{AgentKey: "writer", Name: "写作"}

	// ① 未注入判权端口：不做资源级判权（单测 / 未接线部署）
	if err := u.authorizeTaskRuntime(ctx, &Task{ID: 1}, def); err != nil {
		t.Fatalf("未注入应放行: %v", err)
	}

	auth := &fakeAuthorizer{registered: map[string]bool{}, denied: map[string]bool{}}
	u.SetResourceAuthorizer(auth)

	// ② 资源未登记：兼容放行（全量收口前的旧数据）
	if err := u.authorizeTaskRuntime(ctx, &Task{ID: 1}, def); err != nil {
		t.Fatalf("未登记资源应放行: %v", err)
	}
	if len(auth.checked) != 0 {
		t.Fatalf("未登记不应进入判权，got %v", auth.checked)
	}

	// ③ 已登记且允许：放行，且按 execute 动作判权
	auth.registered["agent:writer"] = true
	if err := u.authorizeTaskRuntime(ctx, &Task{ID: 1}, def); err != nil {
		t.Fatalf("已授权应放行: %v", err)
	}
	if len(auth.checked) != 1 || auth.checked[0] != "agent:writer#execute" {
		t.Fatalf("判权参数不符: %v", auth.checked)
	}

	// ④ 已登记且拒绝：ErrRuntimeDenied
	auth.denied["agent:writer"] = true
	err := u.authorizeTaskRuntime(ctx, &Task{ID: 1}, def)
	if !errors.Is(err, ErrRuntimeDenied) {
		t.Fatalf("应返回 ErrRuntimeDenied，got %v", err)
	}

	// ⑤ 判权存储故障：fail-closed（宁可失败，也不带着不确定的权限继续跑）
	auth.regErr = errors.New("mysql down")
	if err := u.authorizeTaskRuntime(ctx, &Task{ID: 1}, def); err == nil || errors.Is(err, ErrRuntimeDenied) {
		t.Fatalf("存储故障应返回基础设施错误，got %v", err)
	}
}

// 能力名到资源 ID 的映射：工具 / 技能 / 委派目标各走各的前缀。
func TestCapabilityRuntimeResourceMapping(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	auth := &fakeAuthorizer{registered: map[string]bool{}, denied: map[string]bool{}}
	for _, id := range []string{"tool:mail_send", "skill:translate", "agent:research"} {
		auth.registered[id] = true
	}
	u.SetResourceAuthorizer(auth)

	ctx := withPrincipal(context.Background(), "acme", "u1")
	rec := &Recorder{u: u, task: &Task{ID: 5}, def: &AgentDef{AgentKey: "root"}}

	for _, name := range []string{"mcp:mail_send", "skill:translate", "agent:research"} {
		if err := u.authorizeCapabilityRuntime(ctx, rec, name); err != nil {
			t.Fatalf("%s 应放行: %v", name, err)
		}
	}
	want := []string{"tool:mail_send#execute", "skill:translate#execute", "agent:research#execute"}
	if len(auth.checked) != len(want) {
		t.Fatalf("判权次数不符: %v", auth.checked)
	}
	for i := range want {
		if auth.checked[i] != want[i] {
			t.Fatalf("映射不符: %v", auth.checked)
		}
	}

	// 非资源型能力（create_agent）不做资源判权
	auth.checked = nil
	if err := u.authorizeCapabilityRuntime(ctx, rec, CapFactory); err != nil {
		t.Fatalf("create_agent 不应判权: %v", err)
	}
	if len(auth.checked) != 0 {
		t.Fatalf("create_agent 不应产生判权调用，got %v", auth.checked)
	}
}
