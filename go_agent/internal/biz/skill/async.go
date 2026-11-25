package skill

// 异步派发：把 queued 的执行记录投递给 Worker。
//
// 为什么用「数据库队列 + 租约」而不是直接上 Kafka：
//   - 执行记录本身就是账本，任务状态与账本同源，不需要「队列里有、库里没有」的
//     对账逻辑；
//   - MySQL 的 SELECT ... FOR UPDATE SKIP LOCKED 已经能支撑多副本抢占，
//     租约过期即回收，等价于 MQ 的可见性超时；
//   - 部署形态不变：不引入新中间件，单机也能跑（SKILL_RUNTIME_WORKERS=0 关闭）。
//
// 换 Kafka 只需要替换 RuntimeStore 的 ClaimQueued/RequeueStale 实现，
// 运行时（runRemote）与协议都不动——这也是「执行与传递解耦」的意义。

import (
	"context"
	"time"

	"go_agent/internal/pkg/identity"

	"github.com/go-kratos/kratos/v3/log"
)

// StartRuntime 启动异步派发协程（幂等；未接线或 workers<=0 时静默不启）。
func (u *UseCase) StartRuntime() error {
	if u.rt == nil || u.worker == nil {
		log.Info("[skill] 技能运行时未接线：仅 local 模式可用，异步派发不启动")
		return nil
	}
	if u.workers <= 0 {
		log.Info("[skill] 异步派发未启用（SKILL_RUNTIME_WORKERS=0）")
		if u.keep > 0 {
			u.wg.Add(1)
			go u.purgeLoop()
		}
		return nil
	}
	for i := 0; i < u.workers; i++ {
		u.wg.Add(1)
		go u.dispatchLoop(i)
	}
	if u.keep > 0 {
		u.wg.Add(1)
		go u.purgeLoop()
	}
	log.Info("[skill] 技能运行时已启动", "workers", u.workers, "poll", u.poll.String())
	return nil
}

// StopRuntime 停止派发并等待在途任务收尾（优雅停机用）
func (u *UseCase) StopRuntime() {
	u.stopOnce.Do(func() { close(u.stopCh) })
	u.wg.Wait()
}

// dispatchLoop 单个派发协程：轮询 → 领取 → 执行
func (u *UseCase) dispatchLoop(id int) {
	defer u.wg.Done()
	ticker := time.NewTicker(u.poll)
	defer ticker.Stop()
	for {
		select {
		case <-u.stopCh:
			log.Info("[skill] 派发协程退出", "worker", id)
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), u.poll*5)
		n, err := u.DispatchOnce(ctx)
		cancel()
		if err != nil {
			log.Warn("[skill] 派发失败", "worker", id, "err", err)
			continue
		}
		if n > 0 {
			log.Info("[skill] 已投递异步执行", "worker", id, "count", n)
		}
	}
}

// DispatchOnce 处理一轮：回收过期租约 → 领取 1 条 → 执行。
// 单条而不是批量：单个任务的超时不该拖住同批其它任务；吞吐靠协程数（workers）。
func (u *UseCase) DispatchOnce(ctx context.Context) (int, error) {
	if u.rt == nil || u.worker == nil {
		return 0, ErrNotExecutable
	}
	// 租约过期 = 上一个派发进程崩了（或卡死），退回 queued 让别的副本接手。
	// 这里不区分「业务失败」与「进程退出」：租约本身就是活性证明。
	if n, err := u.rt.RequeueStale(ctx, time.Now(), 50); err != nil {
		log.Warn("[skill] 回收过期执行租约失败", "err", err)
	} else if n > 0 {
		log.Warn("[skill] 回收过期租约的执行记录", "count", n)
	}

	lease := 5 * time.Minute
	list, err := u.rt.ClaimQueued(ctx, 1, lease)
	if err != nil {
		return 0, err
	}
	done := 0
	for i := range list {
		exec := list[i]
		if err := u.runClaimed(ctx, &exec); err != nil {
			log.Warn("[skill] 异步执行失败", "execution", exec.ID, "skill", exec.SkillName, "err", err)
		}
		done++
	}
	return done, nil
}

// runClaimed 执行一条已领取的记录
func (u *UseCase) runClaimed(ctx context.Context, exec *Execution) error {
	sk, err := u.repo.Get(ctx, exec.SkillName)
	if err != nil {
		// 技能在排队期间被卸载：明确失败，而不是留在 queued 里永远转圈
		exec.Status = ExecFailed
		exec.Error = "技能已不存在: " + err.Error()
		now := time.Now()
		exec.FinishedAt = &now
		exec.UpdatedAt = now
		return u.rt.UpdateExecution(ctx, exec)
	}
	spec := sk.Execution.Normalized()

	// 异步任务不继承派发协程的 ctx（那是轮询用的短超时），
	// 自己的超时来自技能配置——长任务本来就要跑几分钟。
	taskCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(spec.TimeoutSec)*time.Second+10*time.Second)
	defer cancel()
	taskCtx = identity.With(taskCtx, identity.Info{
		UserID:   exec.UserID,
		TenantID: exec.TenantID,
	})

	req := RuntimeRequest{
		SkillName:      exec.SkillName,
		TaskID:         exec.TaskID,
		Input:          decodeOutput(exec.Input),
		TenantID:       exec.TenantID,
		UserID:         exec.UserID,
		AgentKey:       exec.AgentKey,
		SessionID:      exec.SessionID,
		IdempotencyKey: exec.IdempotencyKey,
	}
	_, _ = u.runRemote(taskCtx, sk, spec, exec, req)
	return nil
}

// purgeLoop 执行记录保留策略：终态记录超过保留期即删（账本不是审计，审计在 audit_events）
func (u *UseCase) purgeLoop() {
	defer u.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-u.stopCh:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		n, err := u.rt.PurgeExecutions(ctx, time.Now().Add(-u.keep), 500)
		cancel()
		if err != nil {
			log.Warn("[skill] 清理执行记录失败", "err", err)
		} else if n > 0 {
			log.Info("[skill] 已清理过期执行记录", "count", n)
		}
	}
}
