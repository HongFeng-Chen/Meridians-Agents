// recovery.go 评测运行的存活判定与僵尸回收。
//
// 问题：executeRun 跑在进程内的 goroutine 里，进程被 kill -9 / OOM / 容器重启后
// 没有任何机会写终态，harness_runs 那一行会永久停在 running（pending 也可能——
// 行已创建但 goroutine 还没起来）。前端一直转圈，报告页永远「执行中」，
// 只能人工去库里 UPDATE（P4 收尾时就是这么干的）。
//
// 解法与任务租约同构：执行期间按固定间隔续 heartbeat_at，进程死亡即停止续跳；
// 巡检把「心跳超阈值」的 pending/running 行收为 failed。
// 多实例安全：活着的实例心跳新鲜，不会被别的实例误收。
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v3/log"
)

// staleRunReason 回收时写进 run.error 的原因（报告页直接可读）
const staleRunReason = "执行体失联：心跳超时（进程可能被强制终止）"

// startHeartbeat 起心跳协程，返回停止函数（可重复调用，只会真正关闭一次）。
func (u *UseCase) startHeartbeat(ctx context.Context, runID uint64) func() {
	if u.repo == nil || u.heartbeatEvery <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(u.heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				// 续跳用独立超时：StopRun 取消运行 ctx 后这次写也该尽量完成，
				// 否则「刚停掉就判失联」会给出一个误导性的错误
				c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := u.repo.TouchRun(c, runID, u.now()); err != nil {
					log.Warn("[harness] 心跳写入失败", "run", runID, "err", err)
				}
				cancel()
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// RecoverStaleRuns 回收僵尸运行：心跳早于 now-staleAfter 的 pending/running 行收为 failed。
//
// 启动时扫一次（上一次进程留下的行不用等一个周期），之后由 StartRecovery 周期巡检。
func (u *UseCase) RecoverStaleRuns(ctx context.Context) (int64, error) {
	if u.repo == nil || u.staleAfter <= 0 {
		return 0, nil
	}
	before := u.now().Add(-u.staleAfter)
	n, err := u.repo.ReapRuns(ctx, before, staleRunReason)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Warn("[harness] 回收僵尸运行（执行体失联）", "count", n,
			"before", before.Format(time.RFC3339), "stale_after", u.staleAfter.String())
	}
	return n, nil
}

// StartRecovery 启动周期巡检（幂等）。所有实例都会巡检，包括别人的僵尸行。
func (u *UseCase) StartRecovery() {
	u.recoverOnce.Do(func() {
		stop := make(chan struct{})
		done := make(chan struct{})
		u.mu.Lock()
		u.recoveryStop, u.recoveryDone = stop, done
		u.mu.Unlock()

		go func() {
			defer close(done)
			// 启动即扫：上一次进程被强杀留下的 running 行立刻可见地收尾
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if _, err := u.RecoverStaleRuns(ctx); err != nil {
				log.Warn("[harness] 启动回收僵尸运行失败", "err", err)
			}
			cancel()

			// 巡检间隔取失联阈值的一半：任何行的可见延迟不超过 1.5 × staleAfter
			every := u.staleAfter / 2
			if every <= 0 {
				every = time.Second
			}
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if _, err := u.RecoverStaleRuns(c); err != nil {
						log.Warn("[harness] 僵尸运行巡检失败", "err", err)
					}
					cancel()
				}
			}
		}()
	})
}

// StopRecovery 停止周期巡检（未启动时安全返回）
func (u *UseCase) StopRecovery() {
	u.mu.Lock()
	stop, done := u.recoveryStop, u.recoveryDone
	u.mu.Unlock()
	if stop == nil {
		return
	}
	select {
	case <-stop:
	default:
		close(stop)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

// newInstanceKey 生成执行实例标识：hostname-pid-随机串。
// 带随机后缀是刻意的——重启后的进程算新实例，run.runner_id 能区分
// 「同一进程跑过两次」和「两个进程各跑一次」。
func newInstanceKey() string {
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
