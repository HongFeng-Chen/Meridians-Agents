package harness

// harness.go 评测用例集与运行的编排逻辑。
//
// 执行模型：StartRun 创建运行行并异步执行 ——
//   用例按启用状态载入 → 并发池（RunConfig.Concurrency）逐例执行 →
//   每例：Runner 黑盒执行 → 规则断言 + LLM Judge 判分 → 落 RunItem →
//   推进 Run 进度（done/passed/failed/score）→ 全部完成聚合终态。
// 取消：StopRun 记录取消标记，worker 在每个用例边界检查（不中断进行中的单例）。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v3/log"
)

// UseCase 评测 Harness 用例
type UseCase struct {
	repo   Repo
	runner Runner
	chat   Chatter       // LLM Judge；nil 时 llm_judge 断言按失败处理并在 Detail 说明
	scores ScoreRecorder // 提示词版本得分回写；nil 时忽略（提示词评测闭环）

	now clock

	mu      sync.Mutex                    // 保护以下字段
	cancels map[uint64]context.CancelFunc // runID → 取消函数
}

// New 构造评测 Harness
func New(repo Repo, runner Runner, chat Chatter, scores ScoreRecorder) *UseCase {
	return &UseCase{
		repo:    repo,
		runner:  runner,
		chat:    chat,
		scores:  scores,
		now:     time.Now,
		cancels: make(map[uint64]context.CancelFunc),
	}
}

// —— 用例集 ——

// ListSets 列出用例集（附带启用用例数）
func (u *UseCase) ListSets(ctx context.Context, f SetFilter) ([]Set, error) {
	sets, err := u.repo.ListSets(ctx, f)
	if err != nil {
		return nil, err
	}
	for i := range sets {
		n, err := u.repo.CountCases(ctx, sets[i].ID, true)
		if err != nil {
			return nil, err
		}
		sets[i].CaseCount = n
	}
	return sets, nil
}

// GetSet 用例集详情（含全部用例，含停用）
func (u *UseCase) GetSet(ctx context.Context, id uint64) (*Set, []Case, error) {
	s, err := u.repo.GetSet(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	cases, err := u.repo.ListCases(ctx, id, false)
	if err != nil {
		return nil, nil, err
	}
	return s, cases, nil
}

// SaveSetInput 新建/修改用例集
type SaveSetInput struct {
	ID          uint64   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Target      string   `json:"target"`
	Tags        []string `json:"tags"`
	Enabled     bool     `json:"enabled"`
}

// SaveSet 新建（ID=0）或更新用例集
func (u *UseCase) SaveSet(ctx context.Context, in SaveSetInput) (*Set, error) {
	if in.Name == "" {
		return nil, ErrEmptySet
	}
	if in.Target != "" && !validTarget(in.Target) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidTarget, in.Target)
	}
	now := u.now()
	s := &Set{
		ID:          in.ID,
		Name:        in.Name,
		Description: in.Description,
		Target:      in.Target,
		Tags:        in.Tags,
		Enabled:     in.Enabled,
		UpdatedAt:   now,
	}
	if s.ID == 0 {
		s.CreatedAt = now
		if err := u.repo.CreateSet(ctx, s); err != nil {
			return nil, err
		}
	} else if err := u.repo.SaveSet(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// DeleteSet 删除用例集（有进行中运行时拒绝）
func (u *UseCase) DeleteSet(ctx context.Context, id uint64) error {
	runs, err := u.repo.ListRuns(ctx, RunFilter{SetID: id, Status: RunRunning})
	if err != nil {
		return err
	}
	if len(runs) > 0 {
		return ErrRunActive
	}
	return u.repo.DeleteSet(ctx, id)
}

// —— 用例 ——

// SaveCaseInput 新建/修改用例
type SaveCaseInput struct {
	ID      uint64   `json:"id"`
	SetID   uint64   `json:"set_id"`
	Name    string   `json:"name"`
	Input   string   `json:"input"`
	Context string   `json:"context"`
	Expect  string   `json:"expect"`
	Checks  []Check  `json:"checks"`
	Tags    []string `json:"tags"`
	Enabled bool     `json:"enabled"`
}

// SaveCase 新建（ID=0）或更新用例
func (u *UseCase) SaveCase(ctx context.Context, in SaveCaseInput) (*Case, error) {
	if in.Input == "" {
		return nil, ErrEmptyCase
	}
	for _, c := range in.Checks {
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("用例 %q 断言不合法: %w", in.Name, err)
		}
	}
	now := u.now()
	c := &Case{
		ID:        in.ID,
		SetID:     in.SetID,
		Name:      in.Name,
		Input:     in.Input,
		Context:   in.Context,
		Expect:    in.Expect,
		Checks:    in.Checks,
		Tags:      in.Tags,
		Enabled:   in.Enabled,
		UpdatedAt: now,
	}
	if c.ID == 0 {
		c.CreatedAt = now
		if err := u.repo.CreateCase(ctx, c); err != nil {
			return nil, err
		}
	} else if err := u.repo.SaveCase(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (u *UseCase) DeleteCase(ctx context.Context, id uint64) error { return u.repo.DeleteCase(ctx, id) }

func (u *UseCase) ListCases(ctx context.Context, setID uint64) ([]Case, error) {
	return u.repo.ListCases(ctx, setID, false)
}

// —— 运行 ——

// StartRun 启动一次评测：创建运行行后异步执行，立即返回（进度轮询 GetRun）。
func (u *UseCase) StartRun(ctx context.Context, setID uint64, cfg RunConfig) (*Run, error) {
	set, err := u.repo.GetSet(ctx, setID)
	if err != nil {
		return nil, err
	}
	cases, err := u.repo.ListCases(ctx, setID, true)
	if err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, ErrNoCases
	}
	target := cfg.Target
	if target == "" {
		target = set.Target
	}
	if !validTarget(target) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidTarget, target)
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.PerCaseTimeoutSec <= 0 {
		cfg.PerCaseTimeoutSec = 120
	}

	run := &Run{
		SetID:     setID,
		SetName:   set.Name,
		Target:    target,
		Status:    RunPending,
		Total:     len(cases),
		Config:    cfg,
		CreatedAt: u.now(),
	}
	if err := u.repo.CreateRun(ctx, run); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	u.mu.Lock()
	u.cancels[run.ID] = cancel
	u.mu.Unlock()

	go u.executeRun(runCtx, *run, cases)
	return run, nil
}

// StopRun 取消运行：当前用例执行完即停止（不打断进行中的单例）
func (u *UseCase) StopRun(ctx context.Context, runID uint64) error {
	u.mu.Lock()
	cancel, ok := u.cancels[runID]
	u.mu.Unlock()
	if !ok {
		return ErrRunNotFound
	}
	cancel()
	return nil
}

func (u *UseCase) GetRun(ctx context.Context, id uint64) (*Run, error) {
	return u.repo.GetRun(ctx, id)
}

func (u *UseCase) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	return u.repo.ListRuns(ctx, f)
}

// GetRunReport 运行详情：运行行 + 逐例结果（前端报告页数据源）
func (u *UseCase) GetRunReport(ctx context.Context, runID uint64) (*Run, []RunItem, error) {
	r, err := u.repo.GetRun(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	items, err := u.repo.ListItems(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	return r, items, nil
}

func (u *UseCase) DeleteRun(ctx context.Context, id uint64) error {
	u.mu.Lock()
	_, active := u.cancels[id]
	u.mu.Unlock()
	if active {
		return ErrRunActive
	}
	return u.repo.DeleteRun(ctx, id)
}

// —— 执行引擎 ——

// executeRun 评测运行主循环（goroutine 内执行，不返回错误，错误写回 Run 行）
func (u *UseCase) executeRun(ctx context.Context, run Run, cases []Case) {
	start := u.now()
	run.Status = RunRunning
	_ = u.repo.SaveRun(ctx, &run)
	_ = u.repo.DeleteItemsOfRun(ctx, run.ID)

	perCase := time.Duration(run.Config.PerCaseTimeoutSec) * time.Second
	sem := make(chan struct{}, run.Config.Concurrency)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex // 保护 run 计数聚合
		canceled bool
		scoreSum float64
		scoreN   int
	)

	for i := range cases {
		c := cases[i]
		// 取消检查：未开始的用例直接跳过
		if ctx.Err() != nil {
			canceled = true
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			item := u.runCase(ctx, run, c, perCase)

			mu.Lock()
			run.Done++
			switch {
			case item.Error != "":
				run.Errored++
			case item.Passed:
				run.Passed++
			default:
				run.Failed++
			}
			if item.Score >= 0 {
				scoreSum += item.Score
				scoreN++
			}
			if scoreN > 0 {
				run.Score = scoreSum / float64(scoreN)
			}
			snapshot := run
			mu.Unlock()

			_ = u.repo.CreateItem(ctx, item)
			_ = u.repo.SaveRun(ctx, &snapshot)
		}()
	}
	wg.Wait()

	// 终态
	u.mu.Lock()
	delete(u.cancels, run.ID)
	u.mu.Unlock()
	mu.Lock()
	run.CostMS = u.now().Sub(start).Milliseconds()
	run.FinishedAt = u.now()
	switch {
	case canceled:
		run.Status = RunCanceled
	case run.Errored == run.Total:
		run.Status = RunFailed
		run.Error = "全部用例执行失败，请检查被测对象与执行器接线"
	default:
		run.Status = RunSucceeded
	}
	final := run
	mu.Unlock()
	_ = u.repo.SaveRun(ctx, &final)

	// 提示词版本评测闭环：带 prompt 标注的运行，终态把加权得分回写提示词工程。
	// 异步 + 独立超时，回写失败只告警，不影响运行结果本身。
	if u.scores != nil && final.Config.PromptKey != "" && final.Config.PromptVersion > 0 && final.Score >= 0 {
		go func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := u.scores.RecordPromptScore(c, final.Config.PromptKey, final.Config.PromptVersion, final.ID, final.Score, final.Done); err != nil {
				log.Warn("[harness] 提示词版本得分回写失败", "key", final.Config.PromptKey, "version", final.Config.PromptVersion, "err", err)
			}
		}()
	}
}

// runCase 执行单用例：黑盒调用 → 判分 → 组装 RunItem（不落库，由调用方落）
func (u *UseCase) runCase(ctx context.Context, run Run, c Case, timeout time.Duration) *RunItem {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	item := &RunItem{
		RunID:     run.ID,
		CaseID:    c.ID,
		CaseName:  c.Name,
		CreatedAt: u.now(),
	}
	input := c.Input
	if c.Context != "" {
		input = c.Context + "\n\n" + c.Input
	}

	begin := u.now()
	output, err := u.runner.Run(ctx, run.Target, input)
	item.LatencyMS = u.now().Sub(begin).Milliseconds()
	if err != nil {
		item.Error = err.Error()
		return item
	}
	item.Output = output

	// 判分：规则断言 + LLM Judge（judge.go）
	item.Checks = u.judge(ctx, run.Config, c, output)
	if s := Score(item.Checks); s >= 0 {
		item.Score = s
		item.Passed = s >= 100
	} else {
		// 无断言：只采集不判分，输出非空即视为通过
		item.Passed = output != ""
		item.Score = -1
	}
	return item
}

// validTarget 校验被测对象格式
func validTarget(t string) bool {
	if len(t) <= len(TargetAgentPrefix) && len(t) <= len(TargetModelPrefix) {
		return false
	}
	if len(t) > len(TargetAgentPrefix) && t[:len(TargetAgentPrefix)] == TargetAgentPrefix {
		return t[len(TargetAgentPrefix):] != ""
	}
	if len(t) > len(TargetModelPrefix) && t[:len(TargetModelPrefix)] == TargetModelPrefix {
		return t[len(TargetModelPrefix):] != ""
	}
	return false
}
