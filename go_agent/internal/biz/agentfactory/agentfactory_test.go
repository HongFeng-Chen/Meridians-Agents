package agentfactory

// 用例层单测：内存 fake 依赖，覆盖工厂护栏、执行图、多 Agent 委派、trace 与任务生命周期。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/skill"
)

// ———— fakes ————

type fakeRepo struct {
	mu      sync.Mutex
	defs    map[uint64]AgentDef
	nextDef uint64
	tasks   map[uint64]Task
	nextTsk uint64
	events  []TaskEvent
	nextEvt uint64
	// checkpoints 步骤级快照（O2/O3 测试断言用）
	checkpoints []TaskCheckpoint
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{defs: map[uint64]AgentDef{}, tasks: map[uint64]Task{}}
}

func (r *fakeRepo) ListDefs(_ context.Context, f DefFilter) ([]AgentDef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AgentDef, 0)
	for _, d := range r.defs {
		if f.Enabled != nil && d.Enabled != *f.Enabled {
			continue
		}
		if f.Source != "" && d.Source != f.Source {
			continue
		}
		if len(f.IDs) > 0 {
			found := false
			for _, id := range f.IDs {
				if d.ID == id {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		if f.Keyword != "" && !strings.Contains(d.Name+d.AgentKey+d.Description, f.Keyword) {
			continue
		}
		out = append(out, d)
	}
	if len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (r *fakeRepo) GetDef(_ context.Context, id uint64) (*AgentDef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[id]
	if !ok {
		return nil, ErrDefNotFound
	}
	return &d, nil
}

func (r *fakeRepo) GetDefByKey(_ context.Context, key string) (*AgentDef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.defs {
		if d.AgentKey == key {
			cp := d
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) CountDefs(_ context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int64(len(r.defs)), nil
}

func (r *fakeRepo) CreateDef(_ context.Context, def *AgentDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextDef++
	def.ID = r.nextDef
	r.defs[def.ID] = *def
	return nil
}

func (r *fakeRepo) UpdateDef(_ context.Context, def *AgentDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.defs[def.ID]; !ok {
		return ErrDefNotFound
	}
	r.defs[def.ID] = *def
	return nil
}

func (r *fakeRepo) DeleteDef(_ context.Context, id uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.defs, id)
	return nil
}

func (r *fakeRepo) SetDefEnabled(_ context.Context, id uint64, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[id]
	if !ok {
		return ErrDefNotFound
	}
	d.Enabled = enabled
	r.defs[id] = d
	return nil
}

func (r *fakeRepo) MarkDefsVectorized(_ context.Context, ids []uint64, ok bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		d, exists := r.defs[id]
		if exists {
			d.Vectorized = ok
			r.defs[id] = d
		}
	}
	return nil
}

func (r *fakeRepo) UpdateDefSchedule(_ context.Context, id uint64, next, last time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, exists := r.defs[id]
	if !exists {
		return nil
	}
	d.NextRunAt = next
	if !last.IsZero() {
		d.LastRunAt = last
	}
	r.defs[id] = d
	return nil
}

func (r *fakeRepo) CreateTask(_ context.Context, t *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextTsk++
	t.ID = r.nextTsk
	r.tasks[t.ID] = *t
	return nil
}

func (r *fakeRepo) SaveTask(_ context.Context, t *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[t.ID]; !ok {
		return ErrTaskNotFound
	}
	r.tasks[t.ID] = *t
	return nil
}

func (r *fakeRepo) ClaimTask(_ context.Context, id uint64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok {
		return false, nil
	}
	if t.Status != TaskPending {
		return false, nil
	}
	t.Status = TaskRunning
	t.StartedAt = time.Now()
	r.tasks[id] = t
	return true, nil
}

func (r *fakeRepo) ListTasks(_ context.Context, f TaskFilter) ([]Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Task, 0)
	for _, t := range r.tasks {
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		if f.ParentTaskID > 0 && t.ParentTaskID != f.ParentTaskID {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (r *fakeRepo) GetTask(_ context.Context, id uint64) (*Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return &t, nil
}

func (r *fakeRepo) CountTasksByStatus(_ context.Context) (map[string]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int64{}
	for _, t := range r.tasks {
		out[t.Status]++
	}
	return out, nil
}

func (r *fakeRepo) RequeueInterrupted(_ context.Context) ([]uint64, error) { return nil, nil }

// —— checkpoint（O2/O3 测试用：内存保存全部快照，支持恢复与续跑断言）——

func (r *fakeRepo) SaveCheckpoint(_ context.Context, cp *TaskCheckpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp.ID = uint64(len(r.checkpoints) + 1)
	r.checkpoints = append(r.checkpoints, *cp)
	return nil
}

func (r *fakeRepo) LatestCheckpoint(_ context.Context, taskID uint64) (*TaskCheckpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest *TaskCheckpoint
	for i := range r.checkpoints {
		if r.checkpoints[i].TaskID == taskID {
			latest = &r.checkpoints[i]
		}
	}
	if latest == nil {
		return nil, nil
	}
	cp := *latest
	return &cp, nil
}

func (r *fakeRepo) TrimCheckpoints(_ context.Context, taskID uint64, keep int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if keep <= 0 {
		kept := r.checkpoints[:0]
		for _, c := range r.checkpoints {
			if c.TaskID != taskID {
				kept = append(kept, c)
			}
		}
		r.checkpoints = kept
		return nil
	}
	// 按 ID 保留最近 keep 条（测试数据量小，直接线性数）
	counts := map[uint64]int{}
	for _, c := range r.checkpoints {
		if c.TaskID == taskID {
			counts[taskID]++
		}
	}
	drop := counts[taskID] - keep
	if drop <= 0 {
		return nil
	}
	kept := r.checkpoints[:0]
	for _, c := range r.checkpoints {
		if c.TaskID == taskID && drop > 0 {
			drop--
			continue
		}
		kept = append(kept, c)
	}
	r.checkpoints = kept
	return nil
}

func (r *fakeRepo) FinishTask(_ context.Context, t *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[t.ID]; !ok {
		return ErrTaskNotFound
	}
	r.tasks[t.ID] = *t
	return nil
}

func (r *fakeRepo) ReviveTask(_ context.Context, t *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Status = TaskPending
	r.tasks[t.ID] = *t
	return nil
}

func (r *fakeRepo) AddEvent(_ context.Context, e *TaskEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextEvt++
	e.ID = r.nextEvt
	r.events = append(r.events, *e)
	return nil
}

func (r *fakeRepo) ListEvents(_ context.Context, taskID uint64, limit int) ([]TaskEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TaskEvent, 0)
	for _, e := range r.events {
		if e.TaskID == taskID {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeRepo) taskIDsByParent(parent uint64) []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []uint64{}
	for _, t := range r.tasks {
		if t.ParentTaskID == parent {
			out = append(out, t.ID)
		}
	}
	return out
}

func (r *fakeRepo) eventTypes(taskID uint64) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, e := range r.events {
		if e.TaskID == taskID {
			out = append(out, e.Type)
		}
	}
	return out
}

// fakeLock 可控的任务锁：默认放行；deny 后 Acquire 失败（验证互斥与释放）
type fakeLock struct {
	mu                 sync.Mutex
	deny               bool
	held               map[uint64]string
	acquired, released int
}

func newFakeLock() *fakeLock { return &fakeLock{held: map[uint64]string{}} }

func (l *fakeLock) Acquire(_ context.Context, taskID uint64, _ time.Duration) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[uint64]string{}
	}
	l.acquired++
	if l.deny || l.held[taskID] != "" {
		return "", false
	}
	token := fmt.Sprintf("tok-%d-%d", taskID, l.acquired)
	l.held[taskID] = token
	return token, true
}

func (l *fakeLock) Release(_ context.Context, taskID uint64, token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[taskID] == token {
		delete(l.held, taskID)
		l.released++
	}
}

func (l *fakeLock) Extend(_ context.Context, taskID uint64, token string, _ time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held[taskID] == token
}

type fakeVec struct{}

func (fakeVec) EnsureCollection(context.Context) error                { return nil }
func (fakeVec) Upsert(context.Context, []AgentDef, [][]float32) error { return nil }
func (fakeVec) Search(_ context.Context, _ []float32, topK int, _ VectorFilter) ([]VectorHit, error) {
	return nil, nil
}
func (fakeVec) Delete(context.Context, []uint64) error { return nil }
func (fakeVec) Count(context.Context) (int64, error)   { return 0, nil }

type fakeCache struct {
	mu    sync.Mutex
	defs  map[uint64]*AgentDef
	queue []uint64
}

func newFakeCache() *fakeCache { return &fakeCache{defs: map[uint64]*AgentDef{}} }

func (c *fakeCache) GetDef(_ context.Context, id uint64) (*AgentDef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.defs[id]
	return d, ok
}
func (c *fakeCache) SetDef(_ context.Context, def *AgentDef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defs[def.ID] = def
}
func (c *fakeCache) DeleteDefs(_ context.Context, ids []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.defs, id)
	}
}
func (c *fakeCache) GetTask(context.Context, uint64) (*Task, bool) { return nil, false }
func (c *fakeCache) SetTask(context.Context, *Task)                {}
func (c *fakeCache) EnqueueTask(_ context.Context, id uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = append(c.queue, id)
	return nil
}
func (c *fakeCache) DequeueTask(context.Context, time.Duration) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return 0, false
	}
	id := c.queue[0]
	c.queue = c.queue[1:]
	return id, true
}

// fakeChatter 脚本化 LLM：按顺序弹出响应
type fakeChatter struct {
	mu       sync.Mutex
	script   []string
	calls    []string
	systems  []string // 完整 system prompt（用于断言能力候选集）
	srcCalls []string // ChatSource 收到的数据源名（断言 llm_source 路由）
}

func (f *fakeChatter) Chat(ctx context.Context, system, user string) (string, error) {
	return f.ChatTemp(ctx, system, user, 0)
}

func (f *fakeChatter) ChatSource(ctx context.Context, source, system, user string, temperature float64) (string, error) {
	f.mu.Lock()
	f.srcCalls = append(f.srcCalls, source)
	f.mu.Unlock()
	return f.ChatTemp(ctx, system, user, temperature)
}

func (f *fakeChatter) ChatTemp(_ context.Context, system, _ string, _ float64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, system[:min(30, len(system))])
	f.systems = append(f.systems, system)
	if len(f.script) == 0 {
		return "", errors.New("脚本耗尽")
	}
	out := f.script[0]
	f.script = f.script[1:]
	return out, nil
}

func (f *fakeChatter) ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error) {
	if o.Source != "" {
		f.mu.Lock()
		f.srcCalls = append(f.srcCalls, o.Source)
		f.mu.Unlock()
	}
	return f.ChatTemp(ctx, o.System, o.User, o.Temperature)
}

func (f *fakeChatter) Model() string { return "fake-model" }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type fakeEmb struct{}

func (fakeEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 4)
	}
	return out, nil
}
func (fakeEmb) Dim() int { return 4 }

type fakeTools struct{}

func (fakeTools) ListTools(_ context.Context, _ mcp.ToolFilter) ([]mcp.Tool, error) {
	return []mcp.Tool{{ID: 1, UniqueName: "demo__calc", Description: "演示计算器", Enabled: true}}, nil
}

func (fakeTools) CallTool(_ context.Context, _ uint64, uniqueName string, args map[string]any) (*mcp.CallResult, error) {
	if uniqueName != "demo__calc" {
		return nil, fmt.Errorf("未知工具 %s", uniqueName)
	}
	a, _ := args["a"].(float64)
	b, _ := args["b"].(float64)
	return &mcp.CallResult{Content: []mcp.ContentPart{{Type: "text", Text: fmt.Sprintf("%v", a+b)}}}, nil
}

// newTestUseCase 组装被测用例
func newTestUseCase(t *testing.T, chat Chatter) (*UseCase, *fakeRepo, *fakeCache) {
	t.Helper()
	repo := newFakeRepo()
	cache := newFakeCache()
	u, err := New(repo, fakeVec{}, cache, &fakeLock{}, chat, fakeEmb{}, fakeTools{}, nil, nil, Options{MaxAgents: 10, DefaultMaxTurns: 6, WorkerCount: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return u, repo, cache
}

func runTask(t *testing.T, u *UseCase, id uint64) *Task {
	t.Helper()
	task, err := u.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	def, err := u.repo.GetDef(context.Background(), task.AgentDefID)
	if err != nil {
		t.Fatalf("GetDef: %v", err)
	}
	_, _ = u.executeTaskNow(context.Background(), task, def)
	final, err := u.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask after run: %v", err)
	}
	return final
}

// ———— 工厂护栏 ————

func TestFactoryGuardrails(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})

	// 白名单拒绝未知能力
	if _, err := u.Factory().Create(context.Background(), SourceManual, AgentInput{
		Name: "x", AgentKey: "x-agent", Capabilities: []string{"mcp:not_exists"},
	}); !errors.Is(err, ErrCapabilityDeny) {
		t.Fatalf("未知能力应被拒绝, got %v", err)
	}

	// 防自举：LLM 源不能携带 create_agent
	if _, err := u.Factory().Create(context.Background(), SourceLLM, AgentInput{
		Name: "self", AgentKey: "self-agent", Capabilities: []string{CapFactory},
	}); !errors.Is(err, ErrBootStrapDeny) {
		t.Fatalf("LLM 自举应被拒绝, got %v", err)
	}

	// 人工源可以带 create_agent（管理员权限）
	d1, err := u.Factory().Create(context.Background(), SourceManual, AgentInput{
		Name: "管理员", AgentKey: "admin-agent", Capabilities: []string{CapFactory},
	})
	if err != nil {
		t.Fatalf("manual create_agent 应允许: %v", err)
	}
	if d1.Sandbox {
		t.Fatal("manual 创建默认不进沙箱")
	}

	// 沙箱不能带委派能力（先建一个可委派目标）
	if _, err := u.Factory().Create(context.Background(), SourceManual, AgentInput{
		Name: "调研", AgentKey: "research-agent", Description: "调研助手",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := u.Factory().Create(context.Background(), SourceManual, AgentInput{
		Name: "沙箱", AgentKey: "sandbox-agent", Sandbox: true, Capabilities: []string{"agent:research-agent"},
	}); !errors.Is(err, ErrInvalidDef) {
		t.Fatalf("沙箱带委派能力应被拒绝, got %v", err)
	}

	// 沙箱与委派互斥：更新路径同样拦截（先建非沙箱可委派，再切沙箱）
	upd, err := u.Factory().Create(context.Background(), SourceManual, AgentInput{
		Name: "调度", AgentKey: "dispatch-agent", Capabilities: []string{"agent:research-agent"},
	})
	if err != nil {
		t.Fatalf("create dispatch: %v", err)
	}
	if _, err := u.UpdateDef(context.Background(), upd.ID, AgentInput{
		Name: "调度", AgentKey: "dispatch-agent", Sandbox: true, Capabilities: []string{"agent:research-agent"},
	}); !errors.Is(err, ErrInvalidDef) {
		t.Fatalf("更新为沙箱时带委派能力应被拒绝, got %v", err)
	}
	// 关闭委派后再切沙箱应放行
	if _, err := u.UpdateDef(context.Background(), upd.ID, AgentInput{
		Name: "调度", AgentKey: "dispatch-agent", Sandbox: true, Capabilities: nil,
	}); err != nil {
		t.Fatalf("移除委派能力后切沙箱应允许: %v", err)
	}
	if _, err := u.UpdateDef(context.Background(), upd.ID, AgentInput{
		Name: "调度", AgentKey: "dispatch-agent", Sandbox: false, Capabilities: []string{"agent:research-agent"},
	}); err != nil {
		t.Fatalf("关闭沙箱后可恢复委派能力: %v", err)
	}

	// 配额：当前已 3 个（admin / research / dispatch），补 7 个到上限 10
	for i := 0; i < 7; i++ {
		if _, err := u.Factory().Create(context.Background(), SourceManual, AgentInput{
			Name: fmt.Sprintf("填充%d", i), AgentKey: fmt.Sprintf("filler-%d", i),
		}); err != nil {
			t.Fatalf("create filler %d: %v", i, err)
		}
	}
	if _, err := u.Factory().Create(context.Background(), SourceManual, AgentInput{
		Name: "超编", AgentKey: "over-agent",
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("配额应拒绝, got %v", err)
	}
	if n, _ := repo.CountDefs(context.Background()); n != 10 {
		t.Fatalf("配额 10, got %d", n)
	}
}

// ———— ReAct 执行图 ————

func TestReactFlow(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"thought": "先算一下", "action": {"name": "mcp:demo__calc", "args": {"a": 1, "b": 2}}}`,
		`{"thought": "有结果了", "final": "计算结果是 3"}`,
	}}
	u, _, cache := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "计算助手", AgentKey: "calc-agent", Capabilities: []string{"mcp:demo__calc"}, MaxTurns: 5,
	}); err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "calc-agent", Input: "算 1+2"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if len(cache.queue) != 1 {
		t.Fatalf("任务应已入队")
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("status=%s err=%s", final.Status, final.ErrorMsg)
	}
	if final.Result != "计算结果是 3" {
		t.Fatalf("result=%q", final.Result)
	}
	if final.Progress != 100 {
		t.Fatalf("progress=%d", final.Progress)
	}
	types := u.repo.(*fakeRepo).eventTypes(final.ID)
	// status → thought → tool_call → tool_result → thought → final → status
	expect := []string{EventStatus, EventThought, EventToolCall, EventToolResult, EventThought, EventFinal, EventStatus}
	if len(types) != len(expect) {
		t.Fatalf("事件序列 = %v, 期望 %v", types, expect)
	}
	for i := range expect {
		if types[i] != expect[i] {
			t.Fatalf("事件[%d] = %s, 期望 %s（全部=%v）", i, types[i], expect[i], types)
		}
	}
}

// ———— 多 Agent 委派 ————

func TestDelegation(t *testing.T) {
	// worker-agent 委派 research-agent（simple 图）
	chat := &fakeChatter{script: []string{
		// worker 第 1 轮：委派
		`{"thought": "需要调研", "action": {"name": "agent:research-agent", "args": {"input": "调研 MCP"}}}`,
		// research-agent（simple 图，内联执行，紧随其后消费）
		`调研完成：MCP 一切正常`,
		// worker 第 2 轮：汇总
		`{"thought": "汇总完成", "final": "调研结论：MCP 可行"}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "调研助手", AgentKey: "research-agent", GraphType: GraphSimple, Description: "专职调研",
	}); err != nil {
		t.Fatalf("create research: %v", err)
	}
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "工作者", AgentKey: "worker-agent", Capabilities: []string{"agent:research-agent"},
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "worker-agent", Input: "搞定 MCP"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded || final.Result != "调研结论：MCP 可行" {
		t.Fatalf("worker final: status=%s result=%q err=%s", final.Status, final.Result, final.ErrorMsg)
	}
	// 子任务链路
	kids := u.repo.(*fakeRepo).taskIDsByParent(final.ID)
	if len(kids) != 1 {
		t.Fatalf("应有 1 个子任务, got %v", kids)
	}
	child, _ := u.repo.GetTask(context.Background(), kids[0])
	if child.AgentKey != "research-agent" || child.Depth != 1 || child.Status != TaskSucceeded {
		t.Fatalf("子任务异常: %+v", child)
	}
	if child.Result != "调研完成：MCP 一切正常" {
		t.Fatalf("子任务 result=%q", child.Result)
	}
	// 父任务 trace 有 delegate 事件
	types := u.repo.(*fakeRepo).eventTypes(final.ID)
	found := false
	for _, e := range types {
		if e == EventDelegate {
			found = true
		}
	}
	if !found {
		t.Fatalf("父任务 trace 缺少 delegate 事件: %v", types)
	}
}

// ———— 会话链路 conv_id 传递 ————

func TestConvIDInheritance(t *testing.T) {
	// 会话编排提交的任务：根任务带 ConvID，委派产生的子任务必须继承，
	// 否则任务内的 MCP / 记忆 / RAG 落 op_histories 时 conv_id 为空（HANDOVER 5.2）。
	chat := &fakeChatter{script: []string{
		// worker 第 1 轮：委派
		`{"thought": "需要调研", "action": {"name": "agent:research-agent", "args": {"input": "调研 MCP"}}}`,
		// research-agent（simple 图，内联执行）
		`调研完成`,
		// worker 第 2 轮：汇总
		`{"thought": "汇总完成", "final": "done"}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "调研助手", AgentKey: "research-agent", GraphType: GraphSimple, Description: "专职调研",
	}); err != nil {
		t.Fatalf("create research: %v", err)
	}
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "工作者", AgentKey: "worker-agent", Capabilities: []string{"agent:research-agent"},
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "worker-agent", Input: "搞定 MCP", ConvID: "conv-42"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if task.ConvID != "conv-42" {
		t.Fatalf("根任务 ConvID 应为 conv-42, got %q", task.ConvID)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded || final.ConvID != "conv-42" {
		t.Fatalf("根任务: status=%s conv=%q", final.Status, final.ConvID)
	}
	kids := u.repo.(*fakeRepo).taskIDsByParent(final.ID)
	if len(kids) != 1 {
		t.Fatalf("应有 1 个子任务, got %v", kids)
	}
	child, _ := u.repo.GetTask(context.Background(), kids[0])
	if child.ConvID != "conv-42" {
		t.Fatalf("子任务应继承 ConvID=conv-42, got %q", child.ConvID)
	}
}

// ———— 沙箱拒绝委派 ————

func TestSandboxDenyDelegation(t *testing.T) {
	// 沙箱 Agent 在白名单校验时已经不能带委派能力；这里验证运行时兜底：
	// 人工把已存白名单的 Agent 改成 sandbox（UpdateDef 校验会拒绝），因此绕过路径只剩运行时检查。
	chat := &fakeChatter{script: []string{
		`{"thought": "试试委派", "action": {"name": "agent:research-agent", "args": {"input": "x"}}}`,
		`{"thought": "只能直答", "final": "直接回答"}`,
		`子任务不应执行`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "调研", AgentKey: "research-agent", GraphType: GraphSimple,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := u.CreateDef(context.Background(), AgentInput{
		Name: "受限", AgentKey: "limited-agent",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 直接篡改 repo 模拟「白名单校验后被降级为沙箱」的兜底场景
	repo := u.repo.(*fakeRepo)
	repo.mu.Lock()
	for id, d := range repo.defs {
		if d.AgentKey == "limited-agent" {
			d.Sandbox = true
			repo.defs[id] = d
		}
	}
	repo.mu.Unlock()

	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "limited-agent", Input: "试试"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded || final.Result != "直接回答" {
		t.Fatalf("沙箱委派失败后应继续直答: status=%s result=%q err=%s", final.Status, final.Result, final.ErrorMsg)
	}
	kids := u.repo.(*fakeRepo).taskIDsByParent(final.ID)
	if len(kids) != 0 {
		t.Fatalf("沙箱不应产生子任务, got %v", kids)
	}
}

// ———— 取消 ————

func TestCancelPendingTask(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	if _, err := u.CreateDef(context.Background(), AgentInput{Name: "闲聊", AgentKey: "chat-agent", GraphType: GraphSimple}); err != nil {
		t.Fatalf("create: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "chat-agent", Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	cancelled, err := u.CancelTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if cancelled.Status != TaskCanceled {
		t.Fatalf("status=%s", cancelled.Status)
	}
	// 重试后回到 pending 并重新入队
	retried, err := u.RetryTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	if retried.Status != TaskPending {
		t.Fatalf("retry status=%s", retried.Status)
	}
}

// ———— LLM 元编程入口 ————

func TestCreateByLLM(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"name":"合规审查助手","agent_key":"compliance-agent","description":"审查合同条款风险","system":"你是资深合规专家","graph_type":"react","capabilities":["mcp:demo__calc"],"max_turns":8}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	def, raw, err := u.Factory().CreateByLLM(context.Background(), "我需要一个审查合同的 Agent")
	if err != nil {
		t.Fatalf("CreateByLLM: %v raw=%s", err, raw)
	}
	if def.Source != SourceLLM || !def.Sandbox {
		t.Fatalf("LLM 造的 Agent 应 source=llm sandbox=true, got %+v", def)
	}
	if def.AgentKey != "compliance-agent" || def.MaxTurns != 8 {
		t.Fatalf("字段未落齐: %+v", def)
	}
	// LLM 输出 create_agent 能力 → 拒绝
	chat2 := &fakeChatter{script: []string{
		`{"name":"造物主","agent_key":"creator-agent","description":"x","graph_type":"react","capabilities":["create_agent"],"max_turns":5}`,
	}}
	u2, _, _ := newTestUseCase(t, chat2)
	if _, _, err := u2.Factory().CreateByLLM(context.Background(), "造一个全能 Agent"); !errors.Is(err, ErrBootStrapDeny) {
		t.Fatalf("LLM 造带 create_agent 的 Agent 应被拒, got %v", err)
	}
}

// TestCreateByLLMPeriodicIntent 需求里写了「每小时自动巡检」：LLM 输出的 auto_run/cron/
// orchestration 必须原样落库（回归：factoryArgs 缺字段导致定时意图被静默丢弃）。
func TestCreateByLLMPeriodicIntent(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"name":"K8s集群健康巡检","agent_key":"k8s-health-inspector","description":"定期巡检集群","system":"你是K8s运维专家","graph_type":"plan_execute","capabilities":["mcp:demo__calc"],"max_turns":12,"orchestration":"auto","auto_run":true,"cron_expr":"0 * * * *","cron_input":"执行一次 K8s 集群健康巡检并输出报告"}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	def, raw, err := u.Factory().CreateByLLM(context.Background(),
		"帮我做一个 K8s 集群健康巡检 Agent，每小时自动跑一次")
	if err != nil {
		t.Fatalf("CreateByLLM: %v raw=%s", err, raw)
	}
	if !def.AutoRun || def.CronExpr != "0 * * * *" {
		t.Fatalf("定时意图未落库: auto_run=%v cron=%q", def.AutoRun, def.CronExpr)
	}
	if def.CronInput != "执行一次 K8s 集群健康巡检并输出报告" {
		t.Fatalf("cron_input 未落库: %q", def.CronInput)
	}
	if def.Orchestration != OrchestrationAuto {
		t.Fatalf("编排方式未落库: %q", def.Orchestration)
	}
	if def.NextRunAt.IsZero() {
		t.Fatal("auto_run Agent 创建后应立即建立排期 NextRunAt")
	}
	if def.Role != RoleRoot {
		t.Fatalf("工厂创建的 Agent 应为 root（定时语义仅 root 生效）: %q", def.Role)
	}
}

// TestCreateByLLMBadCronDegrades cron 非法：不应让整次创建失败，而是关闭自动运行并在
// raw 输出里给出提示；缺 cron_input 时用需求原文补齐。
func TestCreateByLLMBadCronDegrades(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"name":"巡检员","agent_key":"inspector","description":"巡检","system":"你是巡检专家","graph_type":"react","capabilities":[],"max_turns":8,"auto_run":true,"cron_expr":"every hour","cron_input":""}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	def, raw, err := u.Factory().CreateByLLM(context.Background(), "每小时巡检一次集群节点")
	if err != nil {
		t.Fatalf("cron 非法不应导致创建失败: %v", err)
	}
	if def.AutoRun || def.CronExpr != "" || def.CronInput != "" {
		t.Fatalf("非法 cron 应回退为手动运行: auto_run=%v cron=%q", def.AutoRun, def.CronExpr)
	}
	if !strings.Contains(raw, "cron 表达式不合法") {
		t.Fatalf("raw 输出应包含降级提示: %s", raw)
	}
}

// TestCreateByLLMCronInputFallback auto_run 开了但 LLM 没给 cron_input：用需求原文补齐。
func TestCreateByLLMCronInputFallback(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"name":"巡检员","agent_key":"inspector-2","description":"巡检","system":"你是巡检专家","graph_type":"react","capabilities":[],"max_turns":8,"auto_run":true,"cron_expr":"*/30 * * * *"}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	def, _, err := u.Factory().CreateByLLM(context.Background(), "每 30 分钟巡检一次集群")
	if err != nil {
		t.Fatalf("CreateByLLM: %v", err)
	}
	if def.CronInput == "" || !strings.Contains(def.CronInput, "每 30 分钟巡检一次集群") {
		t.Fatalf("cron_input 应回退为需求原文: %q", def.CronInput)
	}
}

// TestCreateByLLMBadOrchestration 编排值非法：降级为 auto，不阻断创建。
func TestCreateByLLMBadOrchestration(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"name":"巡检员","agent_key":"inspector-3","description":"巡检","system":"你是巡检专家","graph_type":"react","capabilities":[],"max_turns":8,"orchestration":"全自动流水线"}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	def, raw, err := u.Factory().CreateByLLM(context.Background(), "做一个巡检 Agent")
	if err != nil {
		t.Fatalf("编排非法不应导致创建失败: %v", err)
	}
	if def.Orchestration != OrchestrationAuto {
		t.Fatalf("非法编排应降级为 auto: %q", def.Orchestration)
	}
	if !strings.Contains(raw, "编排方式不合法") {
		t.Fatalf("raw 输出应包含降级提示: %s", raw)
	}
}

// TestCreateByLLMIntentFallback 模型完全漏填意图字段（auto_run/cron/auto_compose/
// orchestration 一个都没给）：服务端必须从需求原文推导出「每小时排期 + 自动装配」，
// 而不是静默丢掉用户意图（回归：线上「每小时巡检 K8s」创建后 cron 为空、画布为空）。
// 第二段脚本是创建期预装配的规划调用：断言子 Agent 在创建时即落库（父子立即可见）。
func TestCreateByLLMIntentFallback(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"name":"K8s定时巡检诊断Agent","agent_key":"k8s-periodic-inspector","system":"你是K8s巡检诊断Agent","graph_type":"plan_execute","capabilities":["mcp:demo__calc"],"max_turns":20}`,
		`{"graph_type":"orchestrator","reason":"采集-分析-建议分工","members":[
			{"agent_key":"k8s-collector","name":"采集Agent","description":"查询K8s资源","duty":"采集","system":"你是采集Agent"},
			{"agent_key":"k8s-analyzer","name":"分析Agent","description":"定位根因","duty":"分析","system":"你是分析Agent"},
			{"agent_key":"k8s-advisor","name":"建议Agent","description":"给出修复建议","duty":"建议","system":"你是建议Agent"}]}`,
	}}
	u, repo, _ := newTestUseCase(t, chat)
	def, raw, err := u.Factory().CreateByLLM(context.Background(),
		"创建一个每小时巡检K8s命名空间的Agent，多个子Agent协同完成任务，诊断异常并给出优化建议")
	if err != nil {
		t.Fatalf("CreateByLLM: %v raw=%s", err, raw)
	}
	if !def.AutoRun || def.CronExpr != "0 * * * *" {
		t.Fatalf("周期意图未从需求推导: auto_run=%v cron=%q", def.AutoRun, def.CronExpr)
	}
	if strings.TrimSpace(def.CronInput) == "" {
		t.Fatal("cron_input 应回退为需求原文，不能为空（调度器要求非空）")
	}
	if !def.AutoCompose {
		t.Fatal("协同意图未从需求推导: auto_compose 应为 true")
	}
	// 创建期预装配：成员立即落库并回写根定义（画布/列表父子立即可见）
	if len(def.Members) != 3 || def.GraphType != GraphOrchestrator {
		t.Fatalf("创建期预装配未生效: members=%v graph=%s", def.Members, def.GraphType)
	}
	for _, k := range def.Members {
		sub, err := repo.GetDefByKey(context.Background(), k)
		if err != nil || sub == nil {
			t.Fatalf("子 Agent %s 未落库: %v", k, err)
		}
		if sub.Role != RoleSub || !sub.Sandbox {
			t.Fatalf("子 Agent %s 应 role=sub sandbox=true: %+v", k, sub)
		}
	}
	if !strings.Contains(raw, "[factory]") {
		t.Fatalf("推导动作应通过 [factory] 提示回传前端, raw=%s", raw)
	}
}

// TestDeriveCronFromRequirement 需求原文 → cron 的映射表
func TestDeriveCronFromRequirement(t *testing.T) {
	cases := []struct {
		req  string
		want string
	}{
		{"每小时巡检K8s命名空间", "0 * * * *"},
		{"每 30 分钟同步一次告警", "*/30 * * * *"},
		{"每15分钟检查一次队列", "*/15 * * * *"},
		{"每2小时汇总一次指标", "0 */2 * * *"},
		{"每天 9 点生成日报", "0 9 * * *"},
		{"每日早上8点推送摘要", "0 8 * * *"},
		{"工作日9点开晨会准备", "0 9 * * 1-5"},
		{"每周一 8 点出周报", "0 8 * * 1"},
		{"每周三10点做数据备份", "0 10 * * 3"},
		{"定期巡检集群健康状态", "0 * * * *"},
		{"写一份合并复盘报告", ""}, // 无周期语义 → 不推导
		{"", ""},
	}
	for _, c := range cases {
		got, _ := deriveCronFromRequirement(c.req)
		if got != c.want {
			t.Errorf("deriveCron(%q) = %q, want %q", c.req, got, c.want)
		}
	}
	if !requirementImpliesCollab("多个子Agent协同完成任务") {
		t.Error("协同意图未识别")
	}
	if requirementImpliesCollab("写一个查天气的Agent") {
		t.Error("单 Agent 需求误判为协同")
	}
}

// TestCreateByLLMStripsDelegation 沙箱 Agent 不可委派：候选能力清单里不应出现 agent:*，
// 即使模型仍选了，也应剔除后正常创建，而不是整单失败（回归：候选集与沙箱护栏自相矛盾）。
func TestCreateByLLMStripsDelegation(t *testing.T) {
	chat := &fakeChatter{script: []string{
		`{"name":"执行链路评分员","agent_key":"trace-scorer","description":"对执行链路打分","system":"你是评分专家","graph_type":"react","capabilities":["mcp:demo__calc","agent:research-agent"],"max_turns":8}`,
	}}
	u, _, _ := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{Name: "调研", AgentKey: "research-agent", Description: "调研助手"}); err != nil {
		t.Fatalf("create research-agent: %v", err)
	}

	def, raw, err := u.Factory().CreateByLLM(context.Background(), "做一个执行链路评分 Agent")
	if err != nil {
		t.Fatalf("沙箱下选了委派能力不应导致创建失败: %v raw=%s", err, raw)
	}
	if !def.Sandbox {
		t.Fatal("LLM 造的 Agent 必须进沙箱")
	}
	for _, c := range def.Capabilities {
		if strings.HasPrefix(c, CapAgentPrefix) {
			t.Fatalf("委派能力应被剔除, got %v", def.Capabilities)
		}
	}
	if len(def.Capabilities) != 1 || def.Capabilities[0] != "mcp:demo__calc" {
		t.Fatalf("非委派能力应保留, got %v", def.Capabilities)
	}
	// 候选集里不应再列 agent:*（从源头避免模型选中）
	if len(chat.systems) == 0 {
		t.Fatal("未捕获 system prompt")
	}
	if strings.Contains(chat.systems[len(chat.systems)-1], "agent:research-agent") {
		t.Fatal("候选能力清单不应包含委派能力 agent:*")
	}
}

// ———— 任务锁（Redlock）与终态归档 ————

func TestTaskLockLifecycle(t *testing.T) {
	lock := newFakeLock()
	repo := newFakeRepo()
	cache := newFakeCache()
	u, err := New(repo, fakeVec{}, cache, lock, &fakeChatter{script: []string{`{"final":"ok"}`}}, fakeEmb{}, fakeTools{}, nil, nil, Options{DefaultMaxTurns: 6, WorkerCount: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := u.CreateDef(context.Background(), AgentInput{Name: "闲聊", AgentKey: "chat-agent", GraphType: GraphSimple}); err != nil {
		t.Fatalf("create: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "chat-agent", Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	runTask(t, u, task.ID)
	lock.mu.Lock()
	acquired, released, held := lock.acquired, lock.released, len(lock.held)
	lock.mu.Unlock()
	if acquired != 1 || released != 1 || held != 0 {
		t.Fatalf("锁生命周期异常: acquired=%d released=%d held=%d", acquired, released, held)
	}
}

func TestTaskLockDenyBlocksExecution(t *testing.T) {
	lock := newFakeLock()
	lock.deny = true
	repo := newFakeRepo()
	cache := newFakeCache()
	u, err := New(repo, fakeVec{}, cache, lock, &fakeChatter{}, fakeEmb{}, fakeTools{}, nil, nil, Options{DefaultMaxTurns: 6, WorkerCount: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := u.CreateDef(context.Background(), AgentInput{Name: "闲聊", AgentKey: "chat-agent", GraphType: GraphSimple}); err != nil {
		t.Fatalf("create: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "chat-agent", Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	got, err := u.repo.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	def, _ := u.repo.GetDef(context.Background(), got.AgentDefID)
	if _, err := u.executeTaskNow(context.Background(), got, def); !errors.Is(err, ErrTaskLockBusy) {
		t.Fatalf("锁被占用应返回 ErrTaskLockBusy, got %v", err)
	}
	// 任务保持 pending，未被执行体污染
	after, _ := u.repo.GetTask(context.Background(), task.ID)
	if after.Status != TaskPending {
		t.Fatalf("锁被占用时任务应保持 pending, got %s", after.Status)
	}
}

// ———— 优雅停机 ————

// blockChatter 首次对话阻塞，直到 release 关闭或被 ctx 取消；用于模拟「执行中」的任务
type blockChatter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockChatter) Chat(ctx context.Context, system, user string) (string, error) {
	return b.ChatTemp(ctx, system, user, 0)
}

func (b *blockChatter) ChatSource(ctx context.Context, _ string, system, user string, temperature float64) (string, error) {
	return b.ChatTemp(ctx, system, user, temperature)
}

func (b *blockChatter) ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error) {
	return b.ChatTemp(ctx, o.System, o.User, o.Temperature)
}

func (b *blockChatter) ChatTemp(ctx context.Context, _, _ string, _ float64) (string, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return `{"final":"ok"}`, nil
}

func (b *blockChatter) Model() string { return "block-model" }

// TestDrainWaitsForInFlightTask 优雅停机等执行中的任务跑完：Drain 期间任务不被中断，
// 结束后任务正常成功，Drain 才返回。
func TestDrainWaitsForInFlightTask(t *testing.T) {
	chat := &blockChatter{started: make(chan struct{}), release: make(chan struct{})}
	u, repo, _ := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{Name: "闲聊", AgentKey: "chat-agent", GraphType: GraphSimple}); err != nil {
		t.Fatalf("create: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "chat-agent", Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if err := u.StartWorkers(); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
	defer u.Stop()

	select {
	case <-chat.started:
	case <-time.After(3 * time.Second):
		t.Fatal("任务未开始执行")
	}

	drained := make(chan struct{})
	go func() {
		_ = u.Drain(context.Background())
		close(drained)
	}()

	// 任务还卡在 LLM 调用上，Drain 必须等着
	select {
	case <-drained:
		t.Fatal("任务未结束时 Drain 不应返回")
	case <-time.After(200 * time.Millisecond):
	}
	if u.draining.Load() != true {
		t.Fatal("Drain 后 draining 应为 true")
	}

	close(chat.release) // 放行任务
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("任务结束后 Drain 应返回")
	}

	got, err := repo.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != TaskSucceeded {
		t.Fatalf("优雅停机期间任务应正常完成, status=%s err=%s", got.Status, got.ErrorMsg)
	}
}

// TestDrainRejectsNewTasks 停机中不再接收新任务（避免任务入队后无人执行）
func TestDrainRejectsNewTasks(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{script: []string{`{"final":"ok"}`}})
	if _, err := u.CreateDef(context.Background(), AgentInput{Name: "闲聊", AgentKey: "chat-agent", GraphType: GraphSimple}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := u.StartWorkers(); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
	if err := u.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	defer u.Stop()

	if _, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "chat-agent", Input: "hi"}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("停机中提交任务应返回 ErrShuttingDown, got %v", err)
	}
}

// TestDrainTimeoutKeepsTaskRunning 等待超时后强停：任务保持 running（下次启动重跑），
// 不会被误标成「已取消」。
func TestDrainTimeoutKeepsTaskRunning(t *testing.T) {
	chat := &blockChatter{started: make(chan struct{}), release: make(chan struct{})}
	u, repo, _ := newTestUseCase(t, chat)
	if _, err := u.CreateDef(context.Background(), AgentInput{Name: "闲聊", AgentKey: "chat-agent", GraphType: GraphSimple}); err != nil {
		t.Fatalf("create: %v", err)
	}
	task, err := u.SubmitTask(context.Background(), TaskInput{AgentKey: "chat-agent", Input: "hi"})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if err := u.StartWorkers(); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
	select {
	case <-chat.started:
	case <-time.After(3 * time.Second):
		t.Fatal("任务未开始执行")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := u.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	got, err := repo.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != TaskRunning {
		t.Fatalf("超时强停后任务应保持 running（等下次启动重跑），got %s", got.Status)
	}
	if !strings.Contains(got.ErrorMsg, "服务停止") {
		t.Fatalf("应记录停机中断原因, got %q", got.ErrorMsg)
	}
}

// fakeSkills 技能库 fake（测 skill: 能力目录与调用）
type fakeSkills struct {
	skills []skill.Skill
}

func (f *fakeSkills) List(_ context.Context, fl skill.Filter) ([]skill.Skill, error) {
	out := make([]skill.Skill, 0, len(f.skills))
	for _, s := range f.skills {
		if fl.Enabled != nil && s.Enabled != *fl.Enabled {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeSkills) Get(_ context.Context, name string) (*skill.Skill, error) {
	for i := range f.skills {
		if f.skills[i].Name == name {
			return &f.skills[i], nil
		}
	}
	return nil, fmt.Errorf("技能 %s 不存在", name)
}

// 技能能力：目录列启用技能、调用加载 SKILL.md、未启用报错
func TestSkillCapability(t *testing.T) {
	chat := &fakeChatter{script: []string{`{"final":"ok"}`, `{"final":"ok"}`, `{"final":"ok"}`}}
	u, _, _ := newTestUseCase(t, chat)
	u.catalog = NewCatalog(fakeTools{}, func(ctx context.Context) ([]AgentDef, error) { return nil, nil },
		&fakeSkills{skills: []skill.Skill{
			{Name: "document-pro", Version: "1.0.0", Title: "文档处理", Description: "pdf 解析工作流", Enabled: true, Content: "# 工作流：用 pdfplumber 提取文本"},
			{Name: "disabled-one", Version: "0.9.0", Title: "停用技能", Enabled: false, Content: "x"},
		}})

	ctx := context.Background()
	// 目录：只含启用技能
	schemas, err := u.catalog.Schemas(ctx)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	var names []string
	for _, s := range schemas {
		if s.Type == "skill" {
			names = append(names, s.Name)
		}
	}
	if len(names) != 1 || names[0] != "skill:document-pro" {
		t.Fatalf("skill schemas = %v, want [skill:document-pro]", names)
	}

	// 调用：返回 SKILL.md 内容
	out, err := u.InvokeDispatch(ctx, "skill:document-pro", map[string]any{"purpose": "解析财报"}, nil)
	if err != nil {
		t.Fatalf("InvokeDispatch(skill): %v", err)
	}
	if !strings.Contains(out, "pdfplumber") || !strings.Contains(out, "使用目的：解析财报") {
		t.Fatalf("技能内容缺失: %q", truncateRunes(out, 120))
	}

	// 未启用技能报错
	if _, err := u.InvokeDispatch(ctx, "skill:disabled-one", nil, nil); err == nil {
		t.Fatal("调用未启用技能应报错")
	}

	// 白名单校验走 Known（目录里有 → 通过）
	ok, err := u.catalog.Known(ctx, "skill:document-pro")
	if err != nil || !ok {
		t.Fatalf("Known(skill:document-pro) = %v, %v", ok, err)
	}
}

// chatWith 路由：LLMSource 非空走 ChatSource，空则走 ChatTemp
func TestChatWithRoutesByLLMSource(t *testing.T) {
	chat := &fakeChatter{script: []string{`{"final":"ok"}`, `{"final":"ok"}`}}
	u, _, _ := newTestUseCase(t, chat)

	def := &AgentDef{Name: "a", LLMSource: "qwen", GraphType: "simple"}
	if _, err := u.chatWith(context.Background(), def, "sys", "hi"); err != nil {
		t.Fatalf("chatWith(source): %v", err)
	}
	if len(chat.srcCalls) != 1 || chat.srcCalls[0] != "qwen" {
		t.Fatalf("srcCalls = %v, want [qwen]", chat.srcCalls)
	}

	def2 := &AgentDef{Name: "b", GraphType: "simple"}
	if _, err := u.chatWith(context.Background(), def2, "sys", "hi"); err != nil {
		t.Fatalf("chatWith(default): %v", err)
	}
	if len(chat.srcCalls) != 1 {
		t.Fatalf("未指定数据源不应走 ChatSource, srcCalls = %v", chat.srcCalls)
	}
}
