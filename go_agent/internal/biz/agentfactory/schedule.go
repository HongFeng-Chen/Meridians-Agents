package agentfactory

// 后台自动运行：按 cron 表达式周期提交根 Agent 任务。
//
// 调度器随 worker 池一起启动（main.go 的 kratos BeforeStart 钩子），每 30s 扫一次：
//   - 只处理 role=root、auto_run=true、cron 可解析且已启用的定义；
//   - next_run_at 为空则先排期；到期则用 cron_input 建任务并入队（走正常 worker 执行）；
//   - 处理完推进 next_run_at，并记录 last_run_at。
//
// 多节点安全：调度动作本身用 Redlock（与任务锁同一实现，key 空间错开）保护，
// 同一时刻只有一个节点能推进某个定义的排期，避免重复建任务。

import (
	"context"
	"time"

	"go_agent/internal/pkg/identity"

	"github.com/go-kratos/kratos/v3/log"
)

// scheduleLockBase 调度锁的 ID 偏移，避免与真实任务 ID 冲突
const scheduleLockBase = uint64(1) << 62

func scheduleLockID(defID uint64) uint64 { return scheduleLockBase | defID }

// startScheduler 启动 cron 调度循环（ctx 取消即退出）
func (u *UseCase) startScheduler(ctx context.Context) {
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		u.tickSchedules(ctx) // 启动先扫一次：进程停机期间错过的排期立即补上
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				u.tickSchedules(ctx)
			}
		}
	}()
}

// tickSchedules 扫描一轮定时定义
func (u *UseCase) tickSchedules(ctx context.Context) {
	defs, err := u.repo.ListDefs(ctx, DefFilter{Enabled: boolPtr(true), Limit: 1000})
	if err != nil {
		u.warn("定时调度扫描失败", "err", err)
		return
	}
	now := time.Now()
	for i := range defs {
		d := &defs[i]
		if !d.AutoRun || d.CronExpr == "" || roleOrDefault(d.Role) != RoleRoot {
			continue
		}
		if _, err := ParseCron(d.CronExpr); err != nil {
			continue // 表达式非法（历史数据）直接跳过，不让调度循环报错
		}
		u.tickOneSchedule(ctx, d, now)
	}
}

// tickOneSchedule 处理单个定义：排期 / 到期提交。返回是否做了动作。
func (u *UseCase) tickOneSchedule(ctx context.Context, d *AgentDef, now time.Time) bool {
	// 分布式互斥：同一定义同一 tick 只允许一个节点处理
	release, err := u.acquireTaskLock(ctx, scheduleLockID(d.ID))
	if err != nil {
		return false
	}
	defer release()

	if d.NextRunAt.IsZero() {
		next := nextRunTime(d.CronExpr, now)
		if !next.IsZero() {
			_ = u.repo.UpdateDefSchedule(ctx, d.ID, next, d.LastRunAt)
			log.Info("[agentfactory] 定时排期已建立", "agent", d.AgentKey, "next", next.Format(time.RFC3339))
		}
		return true
	}
	if now.Before(d.NextRunAt) {
		return false
	}
	if d.CronInput == "" {
		u.warn("定时 Agent 缺少 cron_input，跳过", "agent", d.AgentKey)
		return false
	}

	task := &Task{
		AgentDefID: d.ID,
		AgentKey:   d.AgentKey,
		Title:      "定时运行 · " + truncateRunes(d.Name, 60),
		Input:      d.CronInput,
		Status:     TaskPending,
		// 定时任务无请求身份（baseCtx）→ 落在 default 作用域，运维可见
		UserID: identity.UserID(ctx),
	}
	if err := u.repo.CreateTask(ctx, task); err != nil {
		u.warn("定时任务创建失败", "agent", d.AgentKey, "err", err)
		return false
	}
	if err := u.cache.EnqueueTask(ctx, task.ID); err != nil {
		u.warn("定时任务入队失败（恢复扫描会自动拾起）", "agent", d.AgentKey, "task", task.ID, "err", err)
	}
	next := nextRunTime(d.CronExpr, now)
	_ = u.repo.UpdateDefSchedule(ctx, d.ID, next, now)
	log.Info("[agentfactory] 定时任务已入队", "agent", d.AgentKey, "task", task.ID, "next", next.Format(time.RFC3339))
	return true
}
