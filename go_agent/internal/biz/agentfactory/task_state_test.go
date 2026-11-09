package agentfactory

// P1-05：任务状态迁移的并发安全（CAS）。

import (
	"context"
	"errors"
	"testing"

	"go_agent/internal/pkg/identity"
)

// 状态机本身：非法迁移要被 CanTransit 拒绝
func TestTaskStateMachine(t *testing.T) {
	ok := [][2]string{
		{TaskPending, TaskRunning}, {TaskPending, TaskCanceled}, {TaskPending, TaskFailed},
		{TaskRunning, TaskSucceeded}, {TaskRunning, TaskFailed},
		{TaskRunning, TaskPaused}, {TaskRunning, TaskCanceled},
		{TaskPaused, TaskPending}, {TaskPaused, TaskFailed}, {TaskPaused, TaskCanceled},
	}
	for _, c := range ok {
		if !CanTransit(c[0], c[1]) {
			t.Fatalf("%s → %s 应合法", c[0], c[1])
		}
	}
	bad := [][2]string{
		{TaskSucceeded, TaskRunning}, {TaskCanceled, TaskPending},
		{TaskPaused, TaskRunning}, {TaskSucceeded, TaskPaused},
	}
	for _, c := range bad {
		if CanTransit(c[0], c[1]) {
			t.Fatalf("%s → %s 不应合法", c[0], c[1])
		}
	}
}

// 取消「读到的 pending」时 worker 刚好认领成功：不能把 running 覆盖成 canceled，
// 否则库里显示已取消、实际上副作用已经发生。
func TestCancelTaskDoesNotOverwriteClaim(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	def, err := u.CreateDef(ctx, AgentInput{Name: "x", AgentKey: "cancel-race-agent", GraphType: GraphSimple})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: def.AgentKey, Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 在读状态与 CAS 之间，模拟 worker 认领成功
	repo.mu.Lock()
	repo.transitionHook = func() {
		repo.mu.Lock()
		tk := repo.tasks[task.ID]
		tk.Status = TaskRunning
		repo.tasks[task.ID] = tk
		repo.mu.Unlock()
	}
	repo.mu.Unlock()

	got, err := u.CancelTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if got.Status != TaskRunning {
		t.Fatalf("并发认领后应走 running 分支，实际 %s", got.Status)
	}
	stored, err := u.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != TaskRunning {
		t.Fatalf("库内状态不应被取消覆盖，实际 %s", stored.Status)
	}
}

// 审批期间任务被取消：批准不能把 canceled 拉回 pending。
func TestApproveTaskRejectsConcurrentCancel(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought":"算一下","action":{"name":"mcp:demo__calc","args":{"a":1,"b":2}}}`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	ctx := context.Background()
	def, err := u.CreateDef(ctx, AgentInput{
		Name: "受控助手", AgentKey: "approve-race-agent", GraphType: GraphReact,
		Capabilities:    []string{"mcp:demo__calc"},
		RequireApproval: []string{"mcp:demo__*"},
	})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: def.AgentKey, Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if final := runTask(t, u, task.ID); final.Status != TaskPaused {
		t.Fatalf("应挂起，实际 %s", final.Status)
	}

	// 读状态之后、CAS 之前，任务被另一个请求取消
	repo.mu.Lock()
	repo.transitionHook = func() {
		repo.mu.Lock()
		tk := repo.tasks[task.ID]
		tk.Status = TaskCanceled
		repo.tasks[task.ID] = tk
		repo.mu.Unlock()
	}
	repo.mu.Unlock()

	if _, err := u.ApproveTask(ctx, task.ID, "approve", "同意"); !errors.Is(err, ErrTaskStateDeny) {
		t.Fatalf("并发取消后批准应被拒绝，got %v", err)
	}
	stored, err := u.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != TaskCanceled {
		t.Fatalf("任务不应被批准拉回 pending，实际 %s", stored.Status)
	}
}

// 终态写入必须带源状态守卫：worker 落 succeeded/failed 时如果任务已被
// 别的路径改了状态，只能放弃写入，不能把对方的结论覆盖掉。
func TestWorkerTerminalWriteIsGuarded(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	def, err := u.CreateDef(ctx, AgentInput{Name: "x", AgentKey: "terminal-guard-agent", GraphType: GraphSimple})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: def.AgentKey, Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	_ = runTask(t, u, task.ID)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.transitionFroms) == 0 {
		t.Fatal("终态写入没有走 TransitionTask（无源状态守卫）")
	}
	for _, from := range repo.transitionFroms {
		if len(from) == 0 {
			t.Fatalf("存在不带源状态守卫的状态写入：%v", repo.transitionFroms)
		}
	}
}

// 任务在 worker 执行期间被置为 canceled：worker 完成后的成功终态不能把它
// 复活成 succeeded（那会让「已取消」的任务带着副作用出现在历史里）。
func TestWorkerTerminalDoesNotResurrectCanceled(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	def, err := u.CreateDef(ctx, AgentInput{Name: "x", AgentKey: "terminal-canceled-agent", GraphType: GraphSimple})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: def.AgentKey, Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 认领之后、落终态之前，状态被外部改成 canceled（模拟管理面强杀）
	repo.mu.Lock()
	repo.transitionHook = func() {
		repo.mu.Lock()
		tk := repo.tasks[task.ID]
		tk.Status = TaskCanceled
		repo.tasks[task.ID] = tk
		repo.mu.Unlock()
	}
	repo.mu.Unlock()

	_ = runTask(t, u, task.ID)
	stored, err := u.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != TaskCanceled {
		t.Fatalf("已取消的任务不应被 worker 改成 %s", stored.Status)
	}
}

// P1-01（任务侧）：提交时固化租户，列表按租户过滤。
// 两个租户即使用同一个 userID，也互相列不到对方的任务。
func TestTaskScopedByTenant(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	def, err := u.CreateDef(context.Background(), AgentInput{Name: "x", AgentKey: "tenant-agent", GraphType: GraphSimple})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}

	ctxA := identity.With(context.Background(), identity.Info{UserID: "alice", TenantID: "t1"})
	ctxB := identity.With(context.Background(), identity.Info{UserID: "alice", TenantID: "t2"})
	taskA, err := u.SubmitTask(ctxA, TaskInput{AgentKey: def.AgentKey, Input: "跑一下"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if taskA.TenantID != "t1" {
		t.Fatalf("任务归属租户应为 t1，实际 %q", taskA.TenantID)
	}

	mine, err := u.ListTasks(ctxA, TaskFilter{TenantID: "t1", UserID: "alice", Limit: 10})
	if err != nil {
		t.Fatalf("ListTasks A: %v", err)
	}
	if len(mine) != 1 {
		t.Fatalf("租户 A 应看到自己的 1 个任务，实际 %d", len(mine))
	}
	other, err := u.ListTasks(ctxB, TaskFilter{TenantID: "t2", UserID: "alice", Limit: 10})
	if err != nil {
		t.Fatalf("ListTasks B: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("同 userID 的另一租户不应看到任务，实际 %d 个", len(other))
	}
}
