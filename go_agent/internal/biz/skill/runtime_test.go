package skill

// 运行时单测：调度、三模式执行、重试、幂等、异步派发。
//
// 全部用内存假实现（fakeStore / fakeWorker），不依赖 MySQL 与网络：
// 这些用例要验证的是「决策与账本」，不是 SQL 写法。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go_agent/internal/pkg/egress"
)

// —— 假仓储 ——

type fakeStore struct {
	mu        sync.Mutex
	instances map[string][]Instance
	execs     map[string]*Execution
	order     []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{instances: map[string][]Instance{}, execs: map[string]*Execution{}}
}

func (s *fakeStore) SaveInstance(_ context.Context, in Instance) (*Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.LastHeartbeat.IsZero() {
		in.LastHeartbeat = time.Now()
	}
	if in.Status == "" {
		in.Status = InstanceOnline
	}
	list := s.instances[in.SkillName]
	for i := range list {
		if list[i].ID == in.ID {
			in.Successes = list[i].Successes
			list[i] = in
			s.instances[in.SkillName] = list
			cp := in
			return &cp, nil
		}
	}
	s.instances[in.SkillName] = append(list, in)
	cp := in
	return &cp, nil
}

func (s *fakeStore) ListInstances(_ context.Context, skillName string) ([]Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.instances[skillName]
	out := make([]Instance, len(list))
	copy(out, list)
	return out, nil
}

func (s *fakeStore) GetInstance(_ context.Context, skillName, id string) (*Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, inst := range s.instances[skillName] {
		if inst.ID == id {
			cp := inst
			return &cp, nil
		}
	}
	return nil, ErrNoWorker
}

func (s *fakeStore) DeleteInstance(_ context.Context, skillName, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.instances[skillName]
	for i := range list {
		if list[i].ID == id {
			s.instances[skillName] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return ErrNoWorker
}

func (s *fakeStore) Heartbeat(_ context.Context, hb InstanceHeartbeat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.instances[hb.SkillName]
	for i := range list {
		if list[i].ID == hb.ID {
			list[i].Inflight = hb.Inflight
			list[i].LastHeartbeat = time.Now()
			if hb.Status != "" {
				list[i].Status = hb.Status
			}
			if hb.LatencyMS > 0 {
				list[i].LatencyMS = hb.LatencyMS
			}
			return nil
		}
	}
	return ErrNoWorker
}

func (s *fakeStore) RecordResult(_ context.Context, skillName, instanceID string, ok bool, latencyMS int64, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.instances[skillName]
	for i := range list {
		if list[i].ID != instanceID {
			continue
		}
		if ok {
			list[i].Failures = 0
			list[i].Successes++
			list[i].LastError = ""
			if latencyMS > 0 {
				list[i].LatencyMS = latencyMS
			}
		} else {
			list[i].Failures++
			list[i].LastError = errMsg
		}
	}
	return nil
}

func (s *fakeStore) CreateExecution(_ context.Context, e *Execution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *e
	s.execs[e.ID] = &cp
	s.order = append(s.order, e.ID)
	return nil
}

func (s *fakeStore) GetExecution(_ context.Context, id string) (*Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.execs[id]
	if !ok {
		return nil, ErrExecutionNotFound
	}
	cp := *e
	return &cp, nil
}

func (s *fakeStore) UpdateExecution(_ context.Context, e *Execution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.execs[e.ID]; !ok {
		return ErrExecutionNotFound
	}
	cp := *e
	s.execs[e.ID] = &cp
	return nil
}

func (s *fakeStore) ListExecutions(_ context.Context, f ExecutionFilter) ([]Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Execution, 0, len(s.order))
	for _, id := range s.order {
		e := s.execs[id]
		if f.SkillName != "" && e.SkillName != f.SkillName {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

func (s *fakeStore) FindExecutionByKey(_ context.Context, tenantID, skillName, key string) (*Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant := tenantID
	if tenant == "" {
		tenant = "public"
	}
	for _, id := range s.order {
		e := s.execs[id]
		et := e.TenantID
		if et == "" {
			et = "public"
		}
		if e.SkillName == skillName && e.IdempotencyKey == key && e.Status == ExecSucceeded && et == tenant {
			cp := *e
			return &cp, nil
		}
	}
	return nil, ErrExecutionNotFound
}

func (s *fakeStore) ClaimQueued(_ context.Context, limit int, lease time.Duration) ([]Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Execution, 0, limit)
	for _, id := range s.order {
		if len(out) >= limit {
			break
		}
		e := s.execs[id]
		if e.Status != ExecQueued {
			continue
		}
		now := time.Now()
		e.Status = ExecRunning
		e.StartedAt = &now
		l := now.Add(lease)
		e.LeaseUntil = &l
		out = append(out, *e)
	}
	return out, nil
}

func (s *fakeStore) RequeueStale(_ context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.order {
		if n >= limit {
			break
		}
		e := s.execs[id]
		if e.Status == ExecRunning && e.LeaseUntil != nil && e.LeaseUntil.Before(before) {
			e.Status = ExecQueued
			e.LeaseUntil = nil
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) PurgeExecutions(_ context.Context, _ time.Time, _ int) (int, error) {
	return 0, nil
}

// —— 假 Worker ——

type fakeWorker struct {
	mu    sync.Mutex
	calls []WorkerRequest
	// handler 按尝试次数决定行为：nil 表示成功并回显 input
	handler func(n int, req WorkerRequest) (*WorkerResponse, error)
}

func (w *fakeWorker) Execute(_ context.Context, inst Instance, req WorkerRequest) (*WorkerResponse, error) {
	w.mu.Lock()
	w.calls = append(w.calls, req)
	n := len(w.calls)
	w.mu.Unlock()
	if w.handler != nil {
		return w.handler(n, req)
	}
	return &WorkerResponse{
		TaskID: req.TaskID, ExecutionID: req.ExecutionID, Success: true,
		Output:   map[string]any{"echo": req.Input["q"], "worker": inst.ID},
		WorkerID: inst.ID,
	}, nil
}

func (w *fakeWorker) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

// allowPrivateEgress 测试里放行回环/私网：Worker 地址基本都是 127.0.0.1 或集群内 10.x，
// 生产环境的出站策略由 EGRESS_ALLOW_PRIVATE* 控制（默认拒绝），这里只测运行时逻辑。
func allowPrivateEgress(t *testing.T) {
	t.Helper()
	prev := egress.CurrentPolicy()
	egress.SetPolicy(egress.Policy{AllowPrivate: true, MaxRedirects: 5})
	t.Cleanup(func() { egress.SetPolicy(prev) })
}

// newRuntimeCase 构造一个带运行时依赖的用例（技能库为内存 fakeRepo）
func newRuntimeCase(t *testing.T, skills ...*Skill) (*UseCase, *fakeRepo, *fakeStore, *fakeWorker) {
	t.Helper()
	allowPrivateEgress(t)
	repo := newFakeRepo()
	for _, s := range skills {
		cp := *s
		repo.items[s.Name] = &cp
	}
	uc, err := New(repo, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store := newFakeStore()
	worker := &fakeWorker{}
	uc.SetRuntime(store, worker)
	return uc, repo, store, worker
}

func remoteSkill(name string, spec ExecutionSpec) *Skill {
	return &Skill{
		Name: name, Version: "1.0.0", Title: name, Enabled: true,
		Content: "# " + name, Execution: spec,
	}
}

// —— 用例 ——

func TestExecuteLocalReturnsContent(t *testing.T) {
	uc, _, _, worker := newRuntimeCase(t, &Skill{
		Name: "doc", Version: "1.0.0", Title: "文档", Enabled: true,
		Content: "# 工作流\n用 pdfplumber 提取文本",
	})
	res, err := uc.Execute(context.Background(), RuntimeRequest{SkillName: "doc", Purpose: "解析财报"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Mode != ModeLocal || res.Status != ExecSucceeded {
		t.Fatalf("local 结果异常: %+v", res)
	}
	if !strings.Contains(res.Text, "pdfplumber") || !strings.Contains(res.Text, "使用目的：解析财报") {
		t.Fatalf("文本缺少内容或目的: %q", res.Text)
	}
	if worker.count() != 0 {
		t.Fatal("local 模式不应调用 Worker")
	}
}

func TestExecuteRemoteSuccess(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote, TimeoutSec: 5})
	uc, _, store, worker := newRuntimeCase(t, sk)
	_, _ = store.SaveInstance(context.Background(), Instance{
		ID: "w1", SkillName: "pdf", Endpoint: "http://127.0.0.1:9001",
		MaxConcurrency: 2, Status: InstanceOnline, LastHeartbeat: time.Now(),
	})

	res, err := uc.Execute(context.Background(), RuntimeRequest{
		SkillName: "pdf", TaskID: "42", Input: map[string]any{"q": "hello"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != ExecSucceeded || res.WorkerID != "w1" || res.Attempts != 1 {
		t.Fatalf("远程结果异常: %+v", res)
	}
	if got := res.Output["echo"]; got != "hello" {
		t.Fatalf("output 未透传: %+v", res.Output)
	}
	if worker.count() != 1 {
		t.Fatalf("worker 调用次数 = %d, want 1", worker.count())
	}
	// 账本：落一条 succeeded 记录，且带回 task/tenant
	exec, err := store.GetExecution(context.Background(), res.ExecutionID)
	if err != nil {
		t.Fatalf("执行记录缺失: %v", err)
	}
	if exec.Status != ExecSucceeded || exec.TaskID != "42" || exec.WorkerID != "w1" {
		t.Fatalf("执行记录异常: %+v", exec)
	}
	// 实例健康度：成功计数 +1，连续失败清零
	inst, _ := store.GetInstance(context.Background(), "pdf", "w1")
	if inst.Successes != 1 || inst.Failures != 0 {
		t.Fatalf("实例指标异常: %+v", inst)
	}
}

func TestExecuteRemoteRetriesAnotherInstance(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote, TimeoutSec: 5, Retry: 1})
	uc, _, store, worker := newRuntimeCase(t, sk)
	for _, id := range []string{"bad", "good"} {
		_, _ = store.SaveInstance(context.Background(), Instance{
			ID: id, SkillName: "pdf", Endpoint: "http://127.0.0.1:9002",
			MaxConcurrency: 2, Status: InstanceOnline, LastHeartbeat: time.Now(),
		})
	}
	worker.handler = func(n int, req WorkerRequest) (*WorkerResponse, error) {
		if n == 1 {
			return nil, errors.New("连接被拒绝")
		}
		return &WorkerResponse{Success: true, Output: map[string]any{"ok": true}, WorkerID: "good"}, nil
	}

	res, err := uc.Execute(context.Background(), RuntimeRequest{SkillName: "pdf"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != ExecSucceeded || res.Attempts != 2 {
		t.Fatalf("应重试后成功: %+v", res)
	}
	if worker.count() != 2 {
		t.Fatalf("worker 调用次数 = %d, want 2", worker.count())
	}
	// 失败实例被计数：下一次调度会避开（熔断阈值 3 次）
	bad, _ := store.GetInstance(context.Background(), "pdf", "bad")
	if bad.Failures != 1 {
		t.Fatalf("失败实例未被计数: %+v", bad)
	}
}

func TestExecuteIdempotentReplay(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote, TimeoutSec: 5, Idempotent: true})
	uc, _, store, worker := newRuntimeCase(t, sk)
	_, _ = store.SaveInstance(context.Background(), Instance{
		ID: "w1", SkillName: "pdf", Endpoint: "http://127.0.0.1:9003",
		MaxConcurrency: 2, Status: InstanceOnline, LastHeartbeat: time.Now(),
	})
	req := RuntimeRequest{
		SkillName: "pdf", TaskID: "7", Input: map[string]any{"q": "x"},
		IdempotencyKey: "task-7:pdf",
	}
	first, err := uc.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("首次执行: %v", err)
	}
	second, err := uc.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("重复执行: %v", err)
	}
	if second.Replayed != true || second.ExecutionID != first.ExecutionID {
		t.Fatalf("应回放首次结果: %+v", second)
	}
	if worker.count() != 1 {
		t.Fatalf("幂等键未生效，worker 调用 %d 次", worker.count())
	}
}

// 幂等键是调用方给的字符串，租户间撞键不能互通结果
func TestExecuteIdempotentReplayIsTenantScoped(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote, TimeoutSec: 5, Idempotent: true})
	uc, _, store, worker := newRuntimeCase(t, sk)
	_, _ = store.SaveInstance(context.Background(), Instance{
		ID: "w1", SkillName: "pdf", Endpoint: "http://127.0.0.1:9013",
		MaxConcurrency: 2, Status: InstanceOnline, LastHeartbeat: time.Now(),
	})
	base := RuntimeRequest{SkillName: "pdf", TaskID: "7", Input: map[string]any{"q": "x"}, IdempotencyKey: "same-key"}

	a := base
	a.TenantID = "t1"
	first, err := uc.Execute(context.Background(), a)
	if err != nil {
		t.Fatalf("t1 首次执行: %v", err)
	}

	b := base
	b.TenantID = "t2"
	second, err := uc.Execute(context.Background(), b)
	if err != nil {
		t.Fatalf("t2 执行: %v", err)
	}
	if second.Replayed {
		t.Fatalf("t2 不应回放 t1 的结果: %+v", second)
	}
	if second.ExecutionID == first.ExecutionID {
		t.Fatal("两个租户拿到了同一条执行记录")
	}
	if worker.count() != 2 {
		t.Fatalf("worker 调用次数 = %d, want 2（每个租户各跑一次）", worker.count())
	}
}

// 旧实例心跳过期后仍留在表里，直连兜底不能因此失效
func TestExecuteStaticFallbackWhenInstancesAllStale(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote, Endpoint: "http://127.0.0.1:9014", TimeoutSec: 5})
	uc, _, store, worker := newRuntimeCase(t, sk)
	_, _ = store.SaveInstance(context.Background(), Instance{
		ID: "old-worker", SkillName: "pdf", Endpoint: "http://127.0.0.1:9999",
		MaxConcurrency: 2, Status: InstanceOnline,
		LastHeartbeat: time.Now().Add(-10 * time.Minute), // 早过期 → offline
	})
	res, err := uc.Execute(context.Background(), RuntimeRequest{SkillName: "pdf"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != ExecSucceeded {
		t.Fatalf("过期实例不应挡住直连兜底: %+v", res)
	}
	if worker.count() != 1 {
		t.Fatalf("worker 调用次数 = %d, want 1", worker.count())
	}
}

func TestExecuteAsyncQueuedThenDispatched(t *testing.T) {
	sk := remoteSkill("video", ExecutionSpec{Mode: ModeAsync, TimeoutSec: 5})
	uc, _, store, worker := newRuntimeCase(t, sk)
	_, _ = store.SaveInstance(context.Background(), Instance{
		ID: "w1", SkillName: "video", Endpoint: "http://127.0.0.1:9004",
		MaxConcurrency: 2, Status: InstanceOnline, LastHeartbeat: time.Now(),
	})

	res, err := uc.Execute(context.Background(), RuntimeRequest{SkillName: "video", Input: map[string]any{"q": "v"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Queued || res.Status != ExecQueued {
		t.Fatalf("异步技能应立即入队: %+v", res)
	}
	if worker.count() != 0 {
		t.Fatal("入队阶段不应调用 Worker")
	}

	n, err := uc.DispatchOnce(context.Background())
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if n != 1 || worker.count() != 1 {
		t.Fatalf("派发数量异常: n=%d calls=%d", n, worker.count())
	}
	exec, err := store.GetExecution(context.Background(), res.ExecutionID)
	if err != nil {
		t.Fatalf("执行记录缺失: %v", err)
	}
	if exec.Status != ExecSucceeded {
		t.Fatalf("派发后状态 = %s, want succeeded（error=%s）", exec.Status, exec.Error)
	}
}

func TestExecuteRemoteNoWorker(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote, TimeoutSec: 2})
	uc, _, _, _ := newRuntimeCase(t, sk)
	res, err := uc.Execute(context.Background(), RuntimeRequest{SkillName: "pdf"})
	if err != nil {
		t.Fatalf("无实例不应直接报错（要让 Agent 看到失败原因）: %v", err)
	}
	if res.Status != ExecFailed || !strings.Contains(res.Text, "没有可用的技能执行实例") {
		t.Fatalf("失败结果异常: %+v", res)
	}
}

func TestExecuteStaticEndpointFallback(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote, Endpoint: "http://127.0.0.1:9005", TimeoutSec: 5})
	uc, _, _, worker := newRuntimeCase(t, sk)
	// 没有注册实例，但技能配了直连地址：应照样能跑
	res, err := uc.Execute(context.Background(), RuntimeRequest{SkillName: "pdf"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != ExecSucceeded {
		t.Fatalf("直连地址未生效: %+v", res)
	}
	if worker.count() != 1 {
		t.Fatalf("worker 调用次数 = %d, want 1", worker.count())
	}
}

func TestExecuteDisabledSkill(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote})
	sk.Enabled = false
	uc, _, _, _ := newRuntimeCase(t, sk)
	if _, err := uc.Execute(context.Background(), RuntimeRequest{SkillName: "pdf"}); !errors.Is(err, ErrSkillDisabled) {
		t.Fatalf("停用技能应拒绝执行: %v", err)
	}
}

func TestListInstancesMarksStaleOffline(t *testing.T) {
	sk := remoteSkill("pdf", ExecutionSpec{Mode: ModeRemote})
	uc, _, store, _ := newRuntimeCase(t, sk)
	uc.sched = NewScheduler(50*time.Millisecond, 3)
	_, _ = store.SaveInstance(context.Background(), Instance{
		ID: "w1", SkillName: "pdf", Endpoint: "http://127.0.0.1:9006",
		MaxConcurrency: 2, Status: InstanceOnline, LastHeartbeat: time.Now().Add(-time.Minute),
	})
	list, err := uc.ListInstances(context.Background(), "pdf")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(list) != 1 || list[0].Status != InstanceOffline {
		t.Fatalf("心跳过期实例应标记离线: %+v", list)
	}
}

func TestSetExecutionKeepsContent(t *testing.T) {
	uc, repo, _, _ := newRuntimeCase(t, &Skill{
		Name: "doc", Version: "1.0.0", Title: "文档", Enabled: true, Content: "# 正文",
	})
	sk, err := uc.SetExecution(context.Background(), "doc", ExecutionSpec{
		Mode: ModeRemote, Endpoint: "http://127.0.0.1:9007",
	})
	if err != nil {
		t.Fatalf("SetExecution: %v", err)
	}
	if sk.Mode() != ModeRemote || sk.Content != "# 正文" {
		t.Fatalf("执行配置未更新或内容被覆盖: %+v", sk)
	}
	if stored := repo.items["doc"]; stored.Mode() != ModeRemote || stored.Content != "# 正文" {
		t.Fatalf("仓储里的技能被改坏: %+v", stored)
	}
}

func TestSpecValidation(t *testing.T) {
	if err := (ExecutionSpec{Mode: "weird"}).Valid(); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("非法模式应报错: %v", err)
	}
	if err := (ExecutionSpec{Mode: ModeRemote, Endpoint: "file:///etc/passwd"}).Valid(); err == nil {
		t.Fatal("非法 endpoint 应报错")
	}
	if got := (ExecutionSpec{}).Normalized(); got.Mode != ModeLocal || got.TimeoutSec == 0 {
		t.Fatalf("默认值补齐异常: %+v", got)
	}
}
