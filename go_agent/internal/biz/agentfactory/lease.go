package agentfactory

// 执行链路治理（V2-P2）：执行体注册 / 心跳 / 任务租约 / 超时与重试清扫。
//
// 解决的真实问题：任务被某个进程认领后，如果该进程被 OOM kill、被 k8s 驱逐、
// 或者机器直接没了，agent_tasks 里那条记录会永远停在 running —— 任务既不成功
// 也不失败，重启扫描只能靠「进程自己起来」，多副本部署时更没人负责回收。
//
// 三个机制配合：
//
//	① worker_instances 心跳：区分「活着但空闲」与「进程已经没了」；
//	② 任务租约：认领时写 lease_owner + lease_expires_at，执行期间自动续租，
//	   进程一死续租就停，租约到期即视为失联；
//	③ 清扫协程：收回过期租约 → retry_wait（退避）→ 重排；次数用尽 → 死信。
//
// 死信不是「丢掉」：任务落终态进历史表并写 dead_letter_reason，
// /api/v2/tasks/dead-letters 可查，人工确认后再走重试接口复活。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-kratos/kratos/v3/log"
)

// Worker 执行体实例（一个进程 = 一个实例）
type Worker struct {
	// Key 实例唯一标识（默认 hostname-pid-随机后缀；重启即新实例，便于审计）
	Key string `json:"key"`
	// Host 主机名
	Host string `json:"host"`
	// Capacity 并发容量（worker 协程数）
	Capacity int `json:"capacity"`
	// Running 当前正在执行的任务数
	Running int `json:"running"`
	// Status online | draining | offline
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	// HeartbeatAt 最近一次心跳
	HeartbeatAt time.Time `json:"heartbeat_at"`
	// ExpiresAt 心跳租约到期时间（超过即判失联）
	ExpiresAt time.Time `json:"expires_at"`
	// Alive 是否在心跳有效期内（由 ListWorkers 计算，不落库）
	Alive bool `json:"alive"`
}

// 执行体状态
const (
	WorkerOnline   = "online"
	WorkerDraining = "draining"
	WorkerOffline  = "offline"
)

// Lease 任务执行租约：认领时写入，执行期间续租，进程死亡即自然过期
type Lease struct {
	// Owner 持有者（执行体 key）
	Owner string
	// ExpiresAt 到期时间
	ExpiresAt time.Time
	// Deadline 任务执行截止时间（超时落 timeout 终态）
	Deadline time.Time
	// Attempt 本次是第几次认领（从 1 开始）
	Attempt int
}

// RetryBackoff 重试退避时长：30s 起，每次翻倍，10 分钟封顶。
//
// 退避而不是立即重排：失联多半是节点级故障（OOM / 网络分区），
// 立刻重排会让刚崩的节点被瞬间重新打满。
func RetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := 30 * time.Second
	for i := 1; i < attempt; i++ {
		if d >= 10*time.Minute {
			return 10 * time.Minute
		}
		d *= 2
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// sweepBatch 单次清扫批量上限：避免一次扫全表把数据库拖住
const sweepBatch = 200

// newWorkerKey 生成实例标识：hostname-pid-随机串。
// 带随机后缀是刻意的——重启后的进程是「新实例」，旧实例记录保留为历史，
// 事后能区分「这台机器崩过三次」和「同一个进程重启过」。
func newWorkerKey() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(buf))
}

// maxAttempts 任务的有效重试上限：任务自身优先，其次全局配置，兜底 1（不重试）
func (u *UseCase) maxAttempts(t *Task) int {
	if t != nil && t.MaxAttempts > 0 {
		return t.MaxAttempts
	}
	if u.opt.MaxAttempts > 0 {
		return u.opt.MaxAttempts
	}
	return 1
}

// shouldRetryBusiness 业务失败是否重试。
//
// 只有调用方显式要求（max_attempts > 1）才重试：大模型任务的失败大多是确定性的
// （提示词错、能力不存在、参数非法），无脑重试只会把 token 额度烧在同一个坑里。
// 基础设施故障（租约过期 / 进程消失）不受此限制，由 maxAttempts 判定。
func (u *UseCase) shouldRetryBusiness(t *Task) bool {
	return t.MaxAttempts > 1 && t.Attempt < u.maxAttempts(t)
}

// retryableBusinessError 排除「重试也没用」的错误类型
func retryableBusinessError(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrInvalidTask), errors.Is(err, ErrTaskStateDeny),
		errors.Is(err, ErrDefNotFound), errors.Is(err, ErrApprovalNeeded),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return true
}

// leaseFor 计算本次认领应写入的租约
func (u *UseCase) leaseFor(t *Task, now time.Time) Lease {
	l := Lease{Owner: u.leaseOwner, Attempt: t.Attempt + 1}
	if u.opt.LeaseTTL > 0 {
		l.ExpiresAt = now.Add(u.opt.LeaseTTL)
	}
	if u.opt.TaskTimeout > 0 {
		l.Deadline = now.Add(u.opt.TaskTimeout)
	}
	return l
}

// registerWorker 注册执行体（幂等）
func (u *UseCase) registerWorker(ctx context.Context) {
	if u.workers == nil {
		return
	}
	w := Worker{
		Key:       u.leaseOwner,
		Host:      hostname(),
		Capacity:  u.opt.WorkerCount,
		Running:   int(u.inFlight.Load()),
		Status:    WorkerOnline,
		StartedAt: u.workerStarted,
		ExpiresAt: time.Now().Add(u.opt.HeartbeatTTL),
	}
	if err := u.workers.UpsertWorker(ctx, w); err != nil {
		u.warn("执行体注册失败（不影响任务执行，但失联回收会看不到本实例）", "err", err)
		return
	}
	log.Info("执行体已注册", "worker", u.leaseOwner, "capacity", w.Capacity)
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// heartbeatLoop 定期上报心跳与在执行任务数；心跳过期即被判失联，手上的任务会被收回
func (u *UseCase) heartbeatLoop(ctx context.Context) {
	defer u.wg.Done()
	interval := u.opt.HeartbeatInterval
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if u.workers == nil {
				continue
			}
			status := WorkerOnline
			if u.draining.Load() {
				status = WorkerDraining
			}
			expires := time.Now().Add(u.opt.HeartbeatTTL)
			hbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interval)
			err := u.workers.HeartbeatWorker(hbCtx, u.leaseOwner, status, int(u.inFlight.Load()), expires)
			cancel()
			if err != nil {
				u.warn("执行体心跳失败", "worker", u.leaseOwner, "err", err)
			}
		}
	}
}

// sweepLoop 清扫协程：回收过期租约、超时任务、到期重试任务，并下线失联执行体。
func (u *UseCase) sweepLoop(ctx context.Context) {
	defer u.wg.Done()
	interval := u.opt.SweepInterval
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.sweepOnce(ctx)
		}
	}
}

// sweepOnce 执行一轮清扫（可被测试直接调用）
func (u *UseCase) sweepOnce(ctx context.Context) {
	now := time.Now()
	// ① 失联执行体手上的任务：重排或死信
	if expired, err := u.repo.ListExpiredLeases(ctx, now, sweepBatch); err != nil {
		u.warn("租约过期扫描失败", "err", err)
	} else {
		for i := range expired {
			u.recoverLostTask(ctx, &expired[i], now, true)
		}
	}
	// ② 超过截止时间的任务 → timeout 终态
	if overdue, err := u.repo.ListOverdueTasks(ctx, now, sweepBatch); err != nil {
		u.warn("超时任务扫描失败", "err", err)
	} else {
		for i := range overdue {
			u.timeoutTask(ctx, &overdue[i], now)
		}
	}
	// ③ 退避到期的重试任务置回 pending 并重新入队
	if ids, err := u.repo.PromoteRetryWait(ctx, now, sweepBatch); err != nil {
		u.warn("重试任务重排失败", "err", err)
	} else {
		for _, id := range ids {
			if err := u.cache.EnqueueTask(ctx, id); err != nil {
				u.warn("重试任务入队失败（下一轮清扫会再次重排）", "task", id, "err", err)
			}
		}
		if len(ids) > 0 {
			log.Info("重试任务已重新入队", "count", len(ids))
		}
	}
	// ④ 心跳过期的执行体置 offline
	if u.workers != nil {
		if n, err := u.workers.MarkStaleWorkers(ctx, now); err != nil {
			u.warn("执行体失联扫描失败", "err", err)
		} else if n > 0 {
			log.Warn("执行体心跳超时，已标记下线", "count", n)
		}
	}
}

// recoverLostTask 收回失联执行体的任务：未超上限 → retry_wait，超限 → 死信。
//
// backoff=false 用于「本进程启动时发现上个进程遗留的 running」：这些任务刚被
// 中断，立即重排更符合预期；backoff=true 用于运行期清扫，避免刚崩的节点被瞬间打满。
func (u *UseCase) recoverLostTask(ctx context.Context, t *Task, now time.Time, backoff bool) {
	max := u.maxAttempts(t)
	if t.Attempt >= max {
		t.Status = TaskFailed
		t.ErrorMsg = fmt.Sprintf("执行体失联且已达重试上限（%d 次），任务转入死信", max)
		t.DeadLetterReason = "worker_lost"
		t.Progress = 100
		t.FinishedAt = now
		t.LeaseOwner, t.LeaseExpiresAt, t.NextAttemptAt = "", time.Time{}, time.Time{}
		// running → failed 走 CAS：期间可能已被用户取消或已成功，不要覆盖别人的结论
		ok, err := u.repo.TransitionTask(ctx, t.ID, []string{TaskRunning}, t)
		if err != nil {
			u.warn("失联任务落终态失败", "task", t.ID, "err", err)
			return
		}
		if !ok {
			return
		}
		u.addTaskEvent(ctx, t.ID, EventError, map[string]any{
			"message":            t.ErrorMsg,
			"dead_letter":        true,
			"dead_letter_reason": t.DeadLetterReason,
			"attempt":            t.Attempt,
		})
		log.Warn("任务进入死信", "task", t.ID, "attempt", t.Attempt, "reason", t.DeadLetterReason)
		return
	}

	t.Status = TaskRetryWait
	t.ErrorMsg = fmt.Sprintf("执行体失联（租约过期），第 %d 次尝试中断，等待重排", t.Attempt)
	t.LeaseOwner, t.LeaseExpiresAt = "", time.Time{}
	t.FinishedAt = time.Time{}
	if backoff {
		t.NextAttemptAt = now.Add(RetryBackoff(t.Attempt))
	} else {
		t.NextAttemptAt = now
	}
	ok, err := u.repo.TransitionTask(ctx, t.ID, []string{TaskRunning}, t)
	if err != nil {
		u.warn("失联任务重排失败", "task", t.ID, "err", err)
		return
	}
	if !ok {
		return
	}
	u.addTaskEvent(ctx, t.ID, EventError, map[string]any{
		"message":          t.ErrorMsg,
		"retry_wait_until": t.NextAttemptAt.Format(time.RFC3339),
		"attempt":          t.Attempt,
	})
	log.Warn("任务执行体失联，已转入重试等待", "task", t.ID, "attempt", t.Attempt, "next", t.NextAttemptAt.Format(time.RFC3339))
}

// timeoutTask 超时任务落 timeout 终态（与 canceled 区分：超时不是用户取消）
func (u *UseCase) timeoutTask(ctx context.Context, t *Task, now time.Time) {
	t.Status = TaskTimeout
	t.ErrorMsg = fmt.Sprintf("任务执行超时（截止 %s）", t.DeadlineAt.Format(time.RFC3339))
	t.Progress = 100
	t.FinishedAt = now
	t.LeaseOwner, t.LeaseExpiresAt = "", time.Time{}
	ok, err := u.repo.TransitionTask(ctx, t.ID, []string{TaskRunning}, t)
	if err != nil {
		u.warn("超时任务落终态失败", "task", t.ID, "err", err)
		return
	}
	if !ok {
		return
	}
	u.addTaskEvent(ctx, t.ID, EventError, map[string]any{
		"message":  t.ErrorMsg,
		"timeout":  true,
		"deadline": t.DeadlineAt.Format(time.RFC3339),
	})
	log.Warn("任务执行超时", "task", t.ID, "deadline", t.DeadlineAt.Format(time.RFC3339))

}

// addTaskEvent 落一条任务事件（不依赖 Recorder：清扫协程里没有执行上下文）
func (u *UseCase) addTaskEvent(ctx context.Context, taskID uint64, typ string, payload any) {
	_ = u.repo.AddEvent(ctx, &TaskEvent{
		TaskID:    taskID,
		Type:      typ,
		Payload:   marshalPayload(payload),
		CreatedAt: time.Now(),
	})
}

// startLeaseRenewal 启动续租协程：TTL/3 一次，续不上说明租约已被收回（别的执行体
// 可能已接手），立刻取消本次执行，避免两个执行体同时跑同一条任务产生重复副作用。
func (u *UseCase) startLeaseRenewal(ctx context.Context, taskID uint64, cancel context.CancelFunc) func() {
	if u.opt.LeaseTTL <= 0 || u.leaseOwner == "" {
		return func() {}
	}
	renewCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	var done chan struct{} = make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(u.opt.LeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				hbCtx, hbCancel := context.WithTimeout(renewCtx, u.opt.LeaseTTL/3)
				ok, err := u.repo.RenewLease(hbCtx, taskID, u.leaseOwner, time.Now().Add(u.opt.LeaseTTL))
				hbCancel()
				if err != nil {
					u.warn("任务续租失败", "task", taskID, "err", err)
					continue
				}
				if !ok {
					u.warn("任务租约已被收回，取消本次执行", "task", taskID)
					cancel()
					return
				}
			}
		}
	}()
	return func() {
		stop()
		<-done
	}
}

// setWorkerStatus 更新实例状态（draining / offline）；未接注册表时为空操作
func (u *UseCase) setWorkerStatus(status string) {
	if u.workers == nil || u.leaseOwner == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := u.workers.SetWorkerStatus(ctx, u.leaseOwner, status); err != nil {
		u.warn("执行体状态更新失败", "worker", u.leaseOwner, "status", status, "err", err)
	}
}

// deregisterWorker 进程退出前置 offline：让看板立刻反映下线，
// 而不是等心跳过期（最多 HeartbeatTTL）。
func (u *UseCase) deregisterWorker() { u.setWorkerStatus(WorkerOffline) }

// ListWorkers 执行体列表：按心跳是否过期标记在线状态
func (u *UseCase) ListWorkers(ctx context.Context) ([]Worker, error) {
	if u.workers == nil {
		return []Worker{}, nil
	}
	list, err := u.workers.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range list {
		list[i].Alive = list[i].Status != WorkerOffline && list[i].ExpiresAt.After(now)
	}
	return list, nil
}

// ListDeadLetters 死信任务（重试次数用尽仍无法完成，已归档）
func (u *UseCase) ListDeadLetters(ctx context.Context, tenantID string, limit int) ([]Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return u.repo.ListDeadLetters(ctx, tenantID, limit)
}
