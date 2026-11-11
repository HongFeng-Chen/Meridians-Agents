package repo

// harness_recovery_test.go 僵尸运行回收（真 SQL）：COALESCE 兜底与状态过滤。

import (
	"context"
	"testing"
	"time"

	"go_agent/internal/data/db/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newHarnessRunDB(t *testing.T) *HarnessRepo {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&models.HarnessRunPO{}); err != nil {
		t.Fatal(err)
	}
	return &HarnessRepo{db: gdb}
}

func TestReapRuns(t *testing.T) {
	r := newHarnessRunDB(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-5 * time.Minute)

	rows := []models.HarnessRunPO{
		{ID: 1, Status: "running", CreatedAt: old, HeartbeatAt: &now},   // 活着
		{ID: 2, Status: "running", CreatedAt: old, HeartbeatAt: &old},   // 失联
		{ID: 3, Status: "pending", CreatedAt: old},                      // 从未心跳，靠创建时间
		{ID: 4, Status: "succeeded", CreatedAt: old, HeartbeatAt: &old}, // 终态
	}
	if err := r.db.Create(&rows).Error; err != nil {
		t.Fatalf("造数据: %v", err)
	}

	n, err := r.ReapRuns(ctx, now.Add(-90*time.Second), "执行体失联：心跳超时")
	if err != nil {
		t.Fatalf("ReapRuns: %v", err)
	}
	if n != 2 {
		t.Fatalf("应回收 2 行，实际 %d", n)
	}

	load := func(id uint64) *models.HarnessRunPO {
		var po models.HarnessRunPO
		if err := r.db.First(&po, id).Error; err != nil {
			t.Fatalf("读取 run %d: %v", id, err)
		}
		return &po
	}
	if po := load(1); po.Status != "running" {
		t.Fatalf("心跳新鲜的运行被误收: %s", po.Status)
	}
	for _, id := range []uint64{2, 3} {
		po := load(id)
		if po.Status != "failed" {
			t.Fatalf("run %d 应回收为 failed，实际 %s", id, po.Status)
		}
		if po.Error == "" || po.FinishedAt == nil {
			t.Fatalf("run %d 缺回收原因或终态时间: %+v", id, po)
		}
	}
	if po := load(4); po.Status != "succeeded" {
		t.Fatalf("终态行不应被改写: %s", po.Status)
	}
}

// 心跳只续 running 的行：终态行不能被「续」回活跃状态
func TestTouchRunOnlyWhileRunning(t *testing.T) {
	r := newHarnessRunDB(t)
	ctx := context.Background()
	now := time.Now()

	if err := r.db.Create(&models.HarnessRunPO{
		ID: 1, Status: "succeeded", CreatedAt: now.Add(-time.Hour), HeartbeatAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Minute)
	if err := r.TouchRun(ctx, 1, later); err != nil {
		t.Fatalf("TouchRun: %v", err)
	}
	var po models.HarnessRunPO
	if err := r.db.First(&po, 1).Error; err != nil {
		t.Fatal(err)
	}
	if po.HeartbeatAt != nil && po.HeartbeatAt.After(now) {
		t.Fatalf("终态行的心跳被改写: %v", po.HeartbeatAt)
	}
}
