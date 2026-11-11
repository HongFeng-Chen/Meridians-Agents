package agentfactory

// 执行链路治理单测（V2-P2）：租约回收、重试退避、超时终态、执行体判活。
//
// 这些用例的共同前提是「执行体死了但没人告诉它」：进程被 OOM kill 不会有
// 退出钩子，所以只能靠租约过期被清扫发现。测试直接构造「running + 租约已过期」
// 的库内状态，再调 sweepOnce 验证收敛结果。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go_agent/internal/biz/llm"
)

// ———— 执行体注册表伪实现 ————

type fakeWorkerRepo struct {
	mu     sync.Mutex
	list   []Worker
	status map[string]string
	stale  int64
	// upserted 最近一次注册的实例（断言注册内容）
	upserted *Worker
}

func newFakeWorkerRepo() *fakeWorkerRepo {
	return &fakeWorkerRepo{status: map[string]string{}}
}

func (f *fakeWorkerRepo) UpsertWorker(_ context.Context, w Worker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := w
	f.upserted = &cp
	f.list = append(f.list, w)
	return nil
}

func (f *fakeWorkerRepo) HeartbeatWorker(_ context.Context, key, status string, running int, expiresAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[key] = status
	for i := range f.list {
		if f.list[i].Key == key {
			f.list[i].Status = status
			f.list[i].Running = running
			f.list[i].ExpiresAt = expiresAt
		}
	}
	return nil
}

func (f *fakeWorkerRepo) SetWorkerStatus(_ context.Context, key, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[key] = status
	for i := range f.list {
		if f.list[i].Key == key {
			f.list[i].Status = status
		}
	}
	return nil
}

func (f *fakeWorkerRepo) ListWorkers(_ context.Context) ([]Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Worker(nil), f.list...), nil
}

func (f *fakeWorkerRepo) MarkStaleWorkers(_ context.Context, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stale, nil
}

// withWorkers 给用例接上执行体注册表（New 不带注册表，避免改动既有测试构造）
func withWorkers(t *testing.T, u *UseCase, w WorkerRepo) *UseCase {
	t.Helper()
	u.workers = w
	return u
}

// seedRunningTask 造一条「正在执行、租约已过期」的任务（模拟执行体失联）
func seedRunningTask(r *fakeRepo, attempt, maxAttempts int, leaseExpired time.Duration) Task {
	now := time.Now()
	t := Task{
		ID:             r.nextTsk + 1,
		AgentDefID:     1,
		AgentKey:       "chat-agent",
		Title:          "巡检",
		Input:          "巡检",
		Status:         TaskRunning,
		Attempt:        attempt,
		MaxAttempts:    maxAttempts,
		LeaseOwner:     "gone-worker",
		LeaseExpiresAt: now.Add(-leaseExpired),
		StartedAt:      now.Add(-time.Hour),
		TenantID:       "acme",
		UserID:         "u1",
	}
	r.nextTsk = t.ID
	r.tasks[t.ID] = t
	return t
}

func TestRetryBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 30 * time.Second},
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 2 * time.Minute},
		{5, 8 * time.Minute},
		{9, 10 * time.Minute},  // 封顶
		{20, 10 * time.Minute}, // 超大 attempt 不溢出
	}
	for _, c := range cases {
		if got := RetryBackoff(c.attempt); got != c.want {
			t.Errorf("RetryBackoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

// TestSweepRequeuesLostTask 租约过期且还有重试机会 → retry_wait，退避到期后回 pending 入队
func TestSweepRequeuesLostTask(t *testing.T) {
	u, repo, cache := newTestUseCase(t, &fakeChatter{})
	task := seedRunningTask(repo, 1, 0, time.Minute)

	u.sweepOnce(context.Background())

	got, _ := repo.GetTask(context.Background(), task.ID)
	if got.Status != TaskRetryWait {
		t.Fatalf("失联任务应转 retry_wait, got %s", got.Status)
	}
	if got.LeaseOwner != "" || !got.LeaseExpiresAt.IsZero() {
		t.Fatalf("转 retry_wait 必须清空租约, got owner=%q expires=%v", got.LeaseOwner, got.LeaseExpiresAt)
	}
	if got.NextAttemptAt.Before(time.Now()) {
		t.Fatalf("退避到期时间应晚于当前, got %v", got.NextAttemptAt)
	}
	if len(cache.queue) != 0 {
		t.Fatalf("退避未到期不应立刻入队, queue=%v", cache.queue)
	}

	// 模拟退避到期：改早到期时间后再次清扫 → 回 pending 并入队
	stale := *got
	stale.NextAttemptAt = time.Now().Add(-time.Second)
	repo.tasks[task.ID] = stale
	u.sweepOnce(context.Background())

	after, _ := repo.GetTask(context.Background(), task.ID)
	if after.Status != TaskPending {
		t.Fatalf("退避到期应回 pending, got %s", after.Status)
	}
	if len(cache.queue) != 1 || cache.queue[0] != task.ID {
		t.Fatalf("重排后应重新入队, queue=%v", cache.queue)
	}
}

// TestSweepDeadLettersExhaustedTask 重试次数用尽 → 终态 failed + 死信原因
func TestSweepDeadLettersExhaustedTask(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	u.opt.MaxAttempts = 3
	task := seedRunningTask(repo, 3, 0, time.Minute)

	u.sweepOnce(context.Background())

	got, _ := repo.GetTask(context.Background(), task.ID)
	if got.Status != TaskFailed {
		t.Fatalf("重试用尽应落 failed, got %s", got.Status)
	}
	if got.DeadLetterReason != "worker_lost" {
		t.Fatalf("死信原因应为 worker_lost, got %q", got.DeadLetterReason)
	}
	// 死信必须留事件：事后只看任务表不知道「为什么没救回来」
	events, _ := repo.ListEvents(context.Background(), task.ID, 10)
	if len(events) == 0 {
		t.Fatal("死信任务应写一条 error 事件")
	}

	dlq, err := u.ListDeadLetters(context.Background(), "acme", 10)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(dlq) != 1 || dlq[0].ID != task.ID {
		t.Fatalf("死信列表应包含该任务, got %+v", dlq)
	}
}

// TestSweepTimesOutOverdueTask 超过截止时间 → timeout 终态（不是 canceled）
func TestSweepTimesOutOverdueTask(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	now := time.Now()
	task := Task{
		ID: 9, AgentDefID: 1, AgentKey: "chat-agent", Title: "长跑", Input: "长跑",
		Status: TaskRunning, Attempt: 1, StartedAt: now.Add(-2 * time.Hour),
		DeadlineAt: now.Add(-time.Minute), TenantID: "acme", UserID: "u1",
	}
	repo.nextTsk = task.ID
	repo.tasks[task.ID] = task

	u.sweepOnce(context.Background())

	got, _ := repo.GetTask(context.Background(), task.ID)
	if got.Status != TaskTimeout {
		t.Fatalf("超时任务应落 timeout, got %s", got.Status)
	}
	if got.Progress != 100 {
		t.Fatalf("终态进度应为 100, got %d", got.Progress)
	}
	if !IsTerminalStatus(got.Status) {
		t.Fatalf("timeout 必须是终态（要归档进历史表）")
	}
}

// TestSweepMarksStaleWorkers 清扫协程顺带把心跳过期的执行体下线
func TestSweepMarksStaleWorkers(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	wr := newFakeWorkerRepo()
	wr.stale = 2
	withWorkers(t, u, wr)

	u.sweepOnce(context.Background())
	if wr.stale != 2 {
		t.Fatal("清扫应调用 MarkStaleWorkers")
	}
}

// TestListWorkersAlive 执行体判活：状态 offline 或心跳过期一律不算活
func TestListWorkersAlive(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	now := time.Now()
	wr := newFakeWorkerRepo()
	wr.list = []Worker{
		{Key: "alive", Status: WorkerOnline, ExpiresAt: now.Add(time.Minute)},
		{Key: "expired", Status: WorkerOnline, ExpiresAt: now.Add(-time.Minute)},
		{Key: "offline", Status: WorkerOffline, ExpiresAt: now.Add(time.Minute)},
	}
	withWorkers(t, u, wr)

	list, err := u.ListWorkers(context.Background())
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	alive := map[string]bool{}
	for _, w := range list {
		alive[w.Key] = w.Alive
	}
	if !alive["alive"] || alive["expired"] || alive["offline"] {
		t.Fatalf("判活结果错误: %+v", alive)
	}
}

// TestListWorkersWithoutRepo 未接注册表时不报错（单机/测试态）
func TestListWorkersWithoutRepo(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	list, err := u.ListWorkers(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("无注册表时应返回空列表, got %v err=%v", list, err)
	}
}

// TestSubmitTaskRejectsInvalidMaxAttempts 重试上限有硬顶，防止把队列打成雪崩
func TestSubmitTaskRejectsInvalidMaxAttempts(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	if err := repo.CreateDef(ctx, &AgentDef{
		AgentKey: "chat-agent", Name: "对话", Enabled: true, Role: RoleRoot,
		GraphType: GraphSimple, MaxTurns: 2,
	}); err != nil {
		t.Fatalf("CreateDef: %v", err)
	}

	if _, err := u.SubmitTask(ctx, TaskInput{AgentKey: "chat-agent", Input: "hi", MaxAttempts: MaxAttemptsLimit + 1}); !errors.Is(err, ErrInvalidTask) {
		t.Fatalf("超出上限应被拒绝, got %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "chat-agent", Input: "hi", MaxAttempts: 2})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if task.MaxAttempts != 2 {
		t.Fatalf("max_attempts 应落到任务上, got %d", task.MaxAttempts)
	}
}

// TestBusinessFailureGoesRetryWait 显式要求重试时，业务失败进 retry_wait 而不是终态
func TestBusinessFailureGoesRetryWait(t *testing.T) {
	// 空脚本 → 第一次调用就报错（确定性失败，仅用于验证状态分流）
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	if err := repo.CreateDef(ctx, &AgentDef{
		AgentKey: "chat-agent", Name: "对话", Enabled: true, Role: RoleRoot,
		GraphType: GraphSimple, MaxTurns: 2,
	}); err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "chat-agent", Input: "hi", MaxAttempts: 3})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	got := runTask(t, u, task.ID)
	if got.Status != TaskRetryWait {
		t.Fatalf("允许重试时业务失败应进 retry_wait, got %s (%s)", got.Status, got.ErrorMsg)
	}
	if got.Attempt != 1 {
		t.Fatalf("首次失败 attempt 应为 1, got %d", got.Attempt)
	}
	if got.DeadLetterReason != "" {
		t.Fatalf("未到上限不应是死信, got %q", got.DeadLetterReason)
	}
	if got.LeaseOwner != "" {
		t.Fatalf("转 retry_wait 应释放租约, got %q", got.LeaseOwner)
	}
}

// TestBusinessFailureWithoutRetryFailsImmediately 默认不重试：一次失败即终态
func TestBusinessFailureWithoutRetryFailsImmediately(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	if err := repo.CreateDef(ctx, &AgentDef{
		AgentKey: "chat-agent", Name: "对话", Enabled: true, Role: RoleRoot,
		GraphType: GraphSimple, MaxTurns: 2,
	}); err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "chat-agent", Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	got := runTask(t, u, task.ID)
	if got.Status != TaskFailed {
		t.Fatalf("未要求重试时应直接 failed, got %s", got.Status)
	}
}

// slowChatter 阻塞到 ctx 结束，用来验证任务级截止时间
type slowChatter struct{ delay time.Duration }

func (s slowChatter) Chat(ctx context.Context, _, _ string) (string, error) {
	return s.ChatTemp(ctx, "", "", 0)
}

func (s slowChatter) ChatTemp(ctx context.Context, _, _ string, _ float64) (string, error) {
	select {
	case <-time.After(s.delay):
		return `{"final":"ok"}`, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s slowChatter) ChatSource(ctx context.Context, _, _, _ string, _ float64) (string, error) {
	return s.ChatTemp(ctx, "", "", 0)
}

func (s slowChatter) ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error) {
	return s.ChatTemp(ctx, o.System, o.User, o.Temperature)
}

func (s slowChatter) Model() string { return "slow" }

// TestTaskDeadlineBecomesTimeout 超过任务截止时间落 timeout（而不是被判成 canceled）
func TestTaskDeadlineBecomesTimeout(t *testing.T) {
	repo := newFakeRepo()
	cache := newFakeCache()
	u, err := New(repo, fakeVec{}, cache, &fakeLock{}, slowChatter{delay: 2 * time.Second}, fakeEmb{}, fakeTools{}, nil, nil,
		Options{MaxAgents: 10, DefaultMaxTurns: 2, WorkerCount: 1, TaskTimeout: 80 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := repo.CreateDef(ctx, &AgentDef{
		AgentKey: "chat-agent", Name: "对话", Enabled: true, Role: RoleRoot,
		GraphType: GraphSimple, MaxTurns: 2,
	}); err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "chat-agent", Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	got := runTask(t, u, task.ID)
	if got.Status != TaskTimeout {
		t.Fatalf("超过 deadline 应落 timeout, got %s (%s)", got.Status, got.ErrorMsg)
	}
	if got.DeadlineAt.IsZero() {
		t.Fatal("认领时应写入 deadline_at")
	}
}
