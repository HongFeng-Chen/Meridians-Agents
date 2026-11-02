package agentfactory

import (
	"context"
	"go_agent/internal/biz/llm"
	"strings"
	"sync"
	"testing"
	"time"
)

// wfChatter 按 system 提示词分流回答，用于编排/监察路径（不依赖真实模型）
type wfChatter struct {
	mu      sync.Mutex
	calls   int
	systems []string
}

func (c *wfChatter) Chat(ctx context.Context, system, user string) (string, error) {
	return c.ChatTemp(ctx, system, user, 0)
}

func (c *wfChatter) ChatSource(ctx context.Context, _ string, system, user string, temperature float64) (string, error) {
	return c.ChatTemp(ctx, system, user, temperature)
}

func (c *wfChatter) ChatTemp(_ context.Context, system, _ string, _ float64) (string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.mu.Lock()
	c.systems = append(c.systems, system)
	c.mu.Unlock()
	switch {
	case strings.Contains(system, "## 汇总要求"):
		return "FINAL-ANSWER", nil
	case strings.Contains(system, "监察 Agent"):
		return `{"passed":true,"issues":"","suggestion":""}`, nil
	case strings.Contains(system, "风险"):
		return `{"level":"high","reason":"删除操作不可逆","allow":false}`, nil
	default:
		return "CHILD-ANSWER", nil
	}
}

func (c *wfChatter) ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error) {
	return c.ChatTemp(ctx, o.System, o.User, o.Temperature)
}

func (c *wfChatter) Model() string { return "wf-fake" }

// TestWorkflowParallelWithScopedSupervisor 并行编排 + 按范围监察
func TestWorkflowParallelWithScopedSupervisor(t *testing.T) {
	u, repo, cache := newTestUseCase(t, &wfChatter{})
	ctx := context.Background()

	for _, key := range []string{"sub-a", "sub-b"} {
		if _, err := u.CreateDef(ctx, AgentInput{Name: key, AgentKey: key, GraphType: GraphSimple}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}
	if _, err := u.CreateDef(ctx, AgentInput{
		Name: "审查员", AgentKey: "reviewer", Role: RoleSupervisor,
		SupervisionScope: []string{"sub-a"},
		GraphType:        GraphSimple,
	}); err != nil {
		t.Fatalf("create supervisor: %v", err)
	}
	root, err := u.CreateDef(ctx, AgentInput{
		Name: "编排根", AgentKey: "root-agent", Role: RoleRoot,
		Orchestration: OrchestrationParallel,
		Members:       []string{"sub-a", "sub-b"},
		SupervisorKey: "reviewer",
		GraphType:     GraphReact,
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	task, err := u.SubmitTask(ctx, TaskInput{AgentKey: "root-agent", Input: "并行调研并汇总"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	final := runTask(t, u, task.ID)
	if final.Status != TaskSucceeded {
		t.Fatalf("任务应成功，实际 %s / %s", final.Status, final.ErrorMsg)
	}
	if final.Result != "FINAL-ANSWER" {
		ch := u.chat.(*wfChatter)
		for i, sys := range ch.systems {
			t.Logf("LLM#%d system: %s", i, sys)
		}
		t.Fatalf("应使用汇总答案，实际 %q", final.Result)
	}

	// 两个成员任务都挂在根任务下
	children := 0
	repo.mu.Lock()
	for _, tk := range repo.tasks {
		if tk.ParentTaskID == task.ID && tk.AgentKey != root.AgentKey {
			children++
		}
	}
	events := append([]TaskEvent(nil), repo.events...)
	repo.mu.Unlock()
	if children != 2 {
		t.Fatalf("应有 2 个子任务，实际 %d", children)
	}

	delegates, supervises := 0, 0
	for _, e := range events {
		if e.TaskID != task.ID {
			continue
		}
		switch e.Type {
		case EventDelegate:
			delegates++
		case EventSupervise:
			supervises++
		}
	}
	if delegates != 2 {
		t.Fatalf("应有 2 个委派事件，实际 %d", delegates)
	}
	// 监察范围只含 sub-a → 只审查 1 个成员
	if supervises != 1 {
		t.Fatalf("监察范围只含 sub-a，应只有 1 次审查，实际 %d", supervises)
	}
	_ = cache
}

// TestScheduleTickCreatesTask cron 到期自动建任务并入队
func TestScheduleTickCreatesTask(t *testing.T) {
	u, repo, cache := newTestUseCase(t, &wfChatter{})
	ctx := context.Background()

	def, err := u.CreateDef(ctx, AgentInput{
		Name: "日报机器人", AgentKey: "daily-bot",
		AutoRun: true, CronExpr: "*/10 * * * *", CronInput: "生成今天的日报",
		GraphType: GraphSimple,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if def.NextRunAt.IsZero() {
		t.Fatal("创建后应已排期 next_run_at")
	}

	// 第一次 tick：未到期，不建任务
	u.tickSchedules(ctx)
	if got := queueLen(cache); got != 0 {
		t.Fatalf("未到期不应入队，实际队列 %d", got)
	}

	// 人为把排期改到过去，模拟到期
	repo.mu.Lock()
	d := repo.defs[def.ID]
	d.NextRunAt = time.Now().Add(-time.Minute)
	repo.defs[def.ID] = d
	repo.mu.Unlock()

	u.tickSchedules(ctx)

	tasks, err := u.ListTasks(ctx, TaskFilter{AgentKey: "daily-bot", Limit: 10})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("到期后应创建 1 个任务，实际 %d", len(tasks))
	}
	if !strings.HasPrefix(tasks[0].Title, "定时运行") {
		t.Fatalf("任务标题应为定时运行前缀，实际 %q", tasks[0].Title)
	}
	if tasks[0].Input != "生成今天的日报" {
		t.Fatalf("定时输入应来自 cron_input，实际 %q", tasks[0].Input)
	}
	if got := queueLen(cache); got != 1 {
		t.Fatalf("任务应入队，实际队列 %d", got)
	}

	// 处理完应推进到下一次
	repo.mu.Lock()
	next := repo.defs[def.ID].NextRunAt
	repo.mu.Unlock()
	if !next.After(time.Now()) {
		t.Fatalf("下次运行时间应被推进，实际 %v", next)
	}
}

func queueLen(c *fakeCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}
