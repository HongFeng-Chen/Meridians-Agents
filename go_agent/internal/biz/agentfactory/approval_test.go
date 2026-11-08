package agentfactory

// P1-04 人工审批的单元测试：
//   - SEC-011 审批绑定「这一次具体参数」，不是整个工具
//   - 凭据有效期 / 策略版本失效
//   - 高危操作不能自批（四眼原则）
//   - 风险评估不可用时 fail-closed，不再当成「没有风险」
//   - 闸门覆盖 router 路径（旧实现只在工具循环里拦，router 可绕过）
//   - 子任务挂起向上传播，父任务不继续、不假装完成

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/pkg/identity"
)

// riskDownChatter 主模型正常，但「风险评估」这一路永远失败。
// 判定依据是风险评估提示词里的输出协议片段，避免误伤普通对话。
type riskDownChatter struct{ inner *fakeChatter }

func (c riskDownChatter) Chat(ctx context.Context, system, user string) (string, error) {
	return c.inner.Chat(ctx, system, user)
}

func (c riskDownChatter) ChatTemp(ctx context.Context, system, user string, temp float64) (string, error) {
	return c.inner.ChatTemp(ctx, system, user, temp)
}

func (c riskDownChatter) ChatSource(ctx context.Context, source, system, user string, temp float64) (string, error) {
	return c.inner.ChatSource(ctx, source, system, user, temp)
}

func (c riskDownChatter) ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error) {
	if strings.Contains(o.System, `"level":"low|medium|high"`) {
		return "", errors.New("风险评估服务 503")
	}
	return c.inner.ChatOpts(ctx, o)
}

func (c riskDownChatter) Model() string { return "fake" }

// useTools 换掉工具供给：能力目录（CreateDef 校验白名单用）与执行期调用必须指向同一份，
// 否则会出现「创建时说能力不存在、运行时又能调」的错觉
func useTools(u *UseCase, tools ToolPort) {
	u.tools = tools
	if u.catalog != nil {
		u.catalog.tools = tools
	}
}

// adminCtx 管理员身份（跨用户审批用）
func adminCtx() context.Context {
	return identity.With(context.Background(), identity.Info{UserID: "ops-admin", TenantID: "public", Auth: "jwt", Admin: true})
}

// tasksByParent 取某父任务下的子任务
func tasksByParent(repo *fakeRepo, parentID uint64) []Task {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	out := make([]Task, 0, 1)
	for _, t := range repo.tasks {
		if t.ParentTaskID == parentID {
			out = append(out, t)
		}
	}
	return out
}

// SEC-011：同一能力、不同参数必须重新审批。
// 旧实现按能力名记账，第一次批准后第二次换成别的 id 会自动放行。
func TestApprovalBoundToArgs(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought":"先算 1+2","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
		`{"thought":"已批准，再算一次","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
		`{"thought":"换成 9+9","action":{"name":"mcp:demo__calc","args":{"a":9,"b":9}}}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	tools := &recordingTools{}
	useTools(u, tools)

	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "算两次", AgentKey: "args-bound-agent", GraphType: GraphReact,
		Capabilities:    []string{"mcp:demo__calc"},
		RequireApproval: []string{"mcp:demo__*"},
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "算两次"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 第一次：命中闸门 → 挂起，工具尚未执行
	if final := runTask(t, u, task.ID); final.Status != TaskPaused {
		t.Fatalf("首次应挂起，实际 %s（err=%s）", final.Status, final.ErrorMsg)
	}
	if tools.calls != 0 {
		t.Fatalf("挂起期间不应执行工具，实际调用 %d 次", tools.calls)
	}

	// 批准 {a:1,b:2} 这一次
	if _, err := u.ApproveTask(context.Background(), task.ID, "approve", "同意这一次"); err != nil {
		t.Fatalf("ApproveTask: %v", err)
	}

	// 续跑：相同参数放行并执行；换参数 → 再次挂起
	final := runTask(t, u, task.ID)
	if final.Status != TaskPaused {
		t.Fatalf("参数变化后应再次挂起，实际 %s（err=%s）", final.Status, final.ErrorMsg)
	}
	if tools.calls != 1 {
		t.Fatalf("只有被批准的那一次可以执行，实际调用 %d 次", tools.calls)
	}
}

// 凭据的三个维度：参数、策略版本、有效期，任一不符即视为未批准。
func TestApprovalGrantValidity(t *testing.T) {
	args := map[string]any{"id": float64(100)}
	other := map[string]any{"id": float64(999999)}
	const ability = "mcp:prod-db__delete"

	st := newState("删一行")
	if _, ok := st.approvedCall(ability, args); ok {
		t.Fatal("未批准不应视为已批准")
	}
	st.grant(ApprovalGrant{
		Ability: ability, ArgsHash: canonicalArgsHash(args), PolicyVersion: approvalPolicyVersion,
		Requester: "alice", Approver: "ops", Decision: "approve",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if _, ok := st.approvedCall(ability, args); !ok {
		t.Fatal("批准后同一参数应放行")
	}
	if _, ok := st.approvedCall(ability, other); ok {
		t.Fatal("参数不同不能复用同一条审批（SEC-011）")
	}

	// 过期
	expired := newState("x")
	expired.grant(ApprovalGrant{
		Ability: ability, ArgsHash: canonicalArgsHash(args), PolicyVersion: approvalPolicyVersion,
		ExpiresAt: time.Now().Add(-time.Second),
	})
	if _, ok := expired.approvedCall(ability, args); ok {
		t.Fatal("过期凭据不应放行")
	}

	// 策略版本不同（升级审批策略后旧授权自动失效）
	oldPolicy := newState("x")
	oldPolicy.grant(ApprovalGrant{
		Ability: ability, ArgsHash: canonicalArgsHash(args), PolicyVersion: "approval/v1",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if _, ok := oldPolicy.approvedCall(ability, args); ok {
		t.Fatal("旧策略版本下的凭据不应在新策略下生效")
	}
}

// 高危操作不能自己批自己；换管理员可以批。
func TestApprovalHighRiskSelfApproveDenied(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought":"删表","action":{"name":"mcp:prod-db__drop_table","args":{"table":"orders"}}}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	tools := &recordingTools{}
	useTools(u, tools)

	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "危险助手", AgentKey: "danger-agent", GraphType: GraphReact,
		Capabilities: []string{"mcp:prod-db__drop_table"},
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "删掉订单表"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if final := runTask(t, u, task.ID); final.Status != TaskPaused {
		t.Fatalf("静态高危工具应挂起（未写 require_approval 也要拦），实际 %s", final.Status)
	}
	if tools.calls != 0 {
		t.Fatalf("挂起期间不应执行工具，实际 %d 次", tools.calls)
	}

	// 请求人（default）自批 → 拒绝
	if _, err := u.ApproveTask(context.Background(), task.ID, "approve", "我自己批"); !errors.Is(err, ErrApprovalSelfApprove) {
		t.Fatalf("高危自批应被拒绝，got %v", err)
	}

	// 管理员跨用户审批 → 允许，且凭据标记 HighRisk
	approved, err := u.ApproveTask(adminCtx(), task.ID, "approve", "已核对变更单")
	if err != nil {
		t.Fatalf("管理员审批应通过: %v", err)
	}
	if approved.Status != TaskPending {
		t.Fatalf("批准后应为 pending，实际 %s", approved.Status)
	}
}

// 风险评估不可用 = 风险未知：配了人工闸门就转人工，没配就阻止本次调用。
// 旧实现在这里返回 nil，调用方按「未启用评估」处理，等于评估器一挂全放行。
func TestRiskAssessFailureFailsClosed(t *testing.T) {
	setup := func(t *testing.T, pauseLevel string) (*UseCase, *recordingTools, uint64) {
		t.Helper()
		chat := riskDownChatter{inner: &fakeChatter{script: []string{
			`{"thought":"调用工具","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
			`{"thought":"只能如实说明","final":"未能执行：风险服务不可用"}`,
		}}}
		u, _, _ := newTestUseCase(t, chat)
		tools := &recordingTools{}
		useTools(u, tools)

		riskDef, err := u.CreateDef(context.Background(), AgentInput{
			Name: "风险官", AgentKey: "risk-agent", Role: RoleRisk, GraphType: GraphSimple,
		})
		if err != nil {
			t.Fatalf("创建风险 Agent: %v", err)
		}
		def, err := u.CreateDef(context.Background(), AgentInput{
			Name: "执行者", AgentKey: "risk-user-agent", GraphType: GraphReact,
			Capabilities:   []string{"mcp:demo__calc"},
			RiskAgentKey:   riskDef.AgentKey,
			RiskPauseLevel: pauseLevel,
		})
		if err != nil {
			t.Fatalf("CreateDef: %v", err)
		}
		task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "算 1+2"})
		if err != nil {
			t.Fatalf("SubmitTask: %v", err)
		}
		return u, tools, task.ID
	}

	// 没有人工闸门：直接阻止本次调用（fail-closed），工具没有被执行
	u, tools, id := setup(t, "")
	final := runTask(t, u, id)
	if tools.calls != 0 {
		t.Fatalf("风险评估不可用时不得执行工具，实际调用 %d 次", tools.calls)
	}
	if final.Status == TaskPaused {
		t.Fatalf("未配置人工闸门时不该挂起（无人可批），实际 %s", final.Status)
	}

	// 配了人工闸门：转人工
	u2, tools2, id2 := setup(t, "high")
	if final := runTask(t, u2, id2); final.Status != TaskPaused {
		t.Fatalf("配置 risk_pause_level=high 时应转人工，实际 %s（err=%s）", final.Status, final.ErrorMsg)
	}
	if tools2.calls != 0 {
		t.Fatalf("转人工期间不得执行工具，实际 %d 次", tools2.calls)
	}
}

// router 图不经过 runToolLoop：闸门必须挂在 InvokeDispatch 才覆盖得到。
func TestRouterPathGatedByApproval(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"target":"mcp:prod-db__drop_table","args":{"table":"orders"},"reason":"用户要求删表"}`,
		`{"target":"mcp:prod-db__drop_table","args":{"table":"orders"},"reason":"用户要求删表"}`,
		`已删除 orders 表`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	tools := &recordingTools{}
	useTools(u, tools)

	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "路由器", AgentKey: "router-danger-agent", GraphType: GraphRouter,
		Capabilities: []string{"mcp:prod-db__drop_table"},
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: def.AgentKey, Input: "把订单表删了"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskPaused {
		t.Fatalf("router 调高危工具应挂起，实际 %s（err=%s）", final.Status, final.ErrorMsg)
	}
	if tools.calls != 0 {
		t.Fatalf("挂起期间不应执行工具，实际 %d 次", tools.calls)
	}

	// 批准后续跑：这条路径没有预置 RunState，凭据要靠挂起时补写的 checkpoint 传递
	if _, err := u.ApproveTask(adminCtx(), task.ID, "approve", "同意删测试表"); err != nil {
		t.Fatalf("ApproveTask: %v", err)
	}
	final = runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("批准后应续跑成功，实际 %s（err=%s）", final.Status, final.ErrorMsg)
	}
	if tools.calls != 1 {
		t.Fatalf("批准后应恰好执行一次，实际 %d 次", tools.calls)
	}
}

// plan_execute 命中审批应就地挂起：不重规划、不推进步骤、批准后续跑。
func TestPlanExecutePausesWithoutReplan(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"steps":[{"goal":"算 1+2"}]}`,
		`{"thought":"算一下","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
		`{"thought":"算一下","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
		`{"thought":"好","final":"1+2=3"}`,
		`答案是 3`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	tools := &recordingTools{}
	useTools(u, tools)

	def, err := u.CreateDef(context.Background(), AgentInput{
		Name: "计划者", AgentKey: "plan-gated-agent", GraphType: GraphPlanExecute,
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

	final := runTask(t, u, task.ID)
	if final.Status != TaskPaused {
		t.Fatalf("应挂起，实际 %s（err=%s）", final.Status, final.ErrorMsg)
	}
	types := typesOf(u, final)
	if types[EventPlan] != 1 {
		t.Fatalf("审批挂起不应触发重规划（plan 事件应只有 1 条），实际 %v", types)
	}
	if types[EventError] != 0 {
		t.Fatalf("审批挂起不应记成步骤失败，实际 %v", types)
	}

	if _, err := u.ApproveTask(context.Background(), task.ID, "approve", "同意"); err != nil {
		t.Fatalf("ApproveTask: %v", err)
	}
	final = runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("批准后应续跑成功，实际 %s（err=%s）", final.Status, final.ErrorMsg)
	}
	if final.Result != "答案是 3" {
		t.Fatalf("result=%q", final.Result)
	}
	if tools.calls != 1 {
		t.Fatalf("整个任务只应真正执行一次工具，实际 %d 次", tools.calls)
	}
}

// 子任务挂起：父任务必须停下并指明是哪个子任务在等审批，
// 不能把「等审批」折成普通失败继续推理（那等于换条路绕过审批）。
func TestChildPauseStopsParent(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought":"交给子 Agent","action":{"name":"agent:child-gated-agent","args":{"input":"算 1+2"}}}`,
		`{"thought":"算一下","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
		`{"thought":"换个办法","final":"父任务不该走到这里"}`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	tools := &recordingTools{}
	useTools(u, tools)

	child, err := u.CreateDef(context.Background(), AgentInput{
		Name: "子 Agent", AgentKey: "child-gated-agent", GraphType: GraphReact,
		Capabilities:    []string{"mcp:demo__calc"},
		RequireApproval: []string{"mcp:demo__*"},
	})
	if err != nil {
		t.Fatalf("创建子 Agent: %v", err)
	}
	parent, err := u.CreateDef(context.Background(), AgentInput{
		Name: "父 Agent", AgentKey: "parent-agent", GraphType: GraphReact,
		Capabilities: []string{"agent:" + child.AgentKey},
	})
	if err != nil {
		t.Fatalf("创建父 Agent: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: parent.AgentKey, Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	final := runTask(t, u, task.ID)
	if final.Status != TaskFailed {
		t.Fatalf("父任务应显式失败（子任务在等审批），实际 %s", final.Status)
	}
	if !strings.Contains(final.ErrorMsg, "等待人工审批") {
		t.Fatalf("父任务错误信息应说明子任务在等审批，实际 %q", final.ErrorMsg)
	}
	if tools.calls != 0 {
		t.Fatalf("子任务挂起期间不应执行工具，实际 %d 次", tools.calls)
	}
	kids := tasksByParent(repo, task.ID)
	if len(kids) != 1 || kids[0].Status != TaskPaused {
		t.Fatalf("子任务应处于 paused，实际 %+v", kids)
	}
	if len(chat.calls) != 2 {
		t.Fatalf("父任务不应在子任务挂起后继续调用模型（期望 2 次），实际 %d", len(chat.calls))
	}
}

var _ = mcp.RiskDestructive
