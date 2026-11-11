package harness

// recovery_test.go 僵尸运行回收：进程被强杀后不能留下永久 running 的行。

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRecoverStaleRuns(t *testing.T) {
	repo := newFakeRepo()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-5 * time.Minute)

	// 1 活着的运行：心跳 5s 前
	repo.runs[1] = &Run{ID: 1, Status: RunRunning, CreatedAt: old, HeartbeatAt: now.Add(-5 * time.Second)}
	// 2 失联：心跳 5 分钟前
	repo.runs[2] = &Run{ID: 2, Status: RunRunning, CreatedAt: old, HeartbeatAt: old}
	// 3 刚创建就崩：从未心跳，只能靠创建时间判定
	repo.runs[3] = &Run{ID: 3, Status: RunPending, CreatedAt: old}
	// 4 已成功：终态行不能被改写
	repo.runs[4] = &Run{ID: 4, Status: RunSucceeded, CreatedAt: old, HeartbeatAt: old}
	// 5 人工取消：同上
	repo.runs[5] = &Run{ID: 5, Status: RunCanceled, CreatedAt: old}

	u := New(repo, nil, nil, nil)
	u.now = func() time.Time { return now }
	u.SetRecovery(90*time.Second, 15*time.Second)

	n, err := u.RecoverStaleRuns(context.Background())
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if n != 2 {
		t.Fatalf("应回收 2 行（失联 running + 未开始 pending），实际 %d", n)
	}

	if got := repo.runs[1]; got.Status != RunRunning {
		t.Fatalf("心跳新鲜的运行被误收: %s", got.Status)
	}
	for _, id := range []uint64{2, 3} {
		got := repo.runs[id]
		if got.Status != RunFailed {
			t.Fatalf("run %d 应回收为 failed，实际 %s", id, got.Status)
		}
		if !strings.Contains(got.Error, "失联") {
			t.Fatalf("run %d 的回收原因不可读: %q", id, got.Error)
		}
		if got.FinishedAt.IsZero() {
			t.Fatalf("run %d 回收后必须有终态时间，否则前端仍显示进行中", id)
		}
	}
	if repo.runs[4].Status != RunSucceeded || repo.runs[5].Status != RunCanceled {
		t.Fatal("终态行不应被巡检改写")
	}
}

// 未启动巡检时 StopRecovery 必须安全（defer 里无条件调用）
func TestStopRecoveryWithoutStart(t *testing.T) {
	u := New(newFakeRepo(), nil, nil, nil)
	u.StopRecovery()
}
