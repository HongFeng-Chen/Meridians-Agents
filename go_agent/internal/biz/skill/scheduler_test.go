package skill

// 调度器单测：策略必须是确定性的（同分同延迟按 ID 排），否则线上会出现
// 「同样两个实例，有时打到 A 有时打到 B」的不可复现问题。

import (
	"errors"
	"testing"
	"time"
)

func schedAt(now time.Time) *Scheduler {
	return &Scheduler{TTL: 30 * time.Second, Breaker: 3, Now: func() time.Time { return now }}
}

func freshInst(id string, weight, inflight, latency int) Instance {
	return Instance{
		ID: id, Endpoint: "http://10.0.0.1:9000", Weight: weight,
		MaxConcurrency: 10, Inflight: inflight, LatencyMS: int64(latency),
		Status: InstanceOnline, LastHeartbeat: time.Now(),
	}
}

func TestSchedulerPrefersLessLoaded(t *testing.T) {
	now := time.Now()
	s := schedAt(now)
	busy := freshInst("busy", 1, 9, 50)
	idle := freshInst("idle", 1, 1, 50)
	picked, err := s.Pick([]Instance{busy, idle}, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if picked.ID != "idle" {
		t.Fatalf("应选空闲实例，实际 %s", picked.ID)
	}
}

func TestSchedulerWeightBeatsLoad(t *testing.T) {
	now := time.Now()
	s := schedAt(now)
	// 权重 5 但半载（score=5*0.5=2.5） vs 权重 1 空载（1*1=1）
	heavy := freshInst("heavy", 5, 5, 0)
	light := freshInst("light", 1, 0, 0)
	picked, err := s.Pick([]Instance{heavy, light}, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if picked.ID != "heavy" {
		t.Fatalf("权重应占优，实际 %s", picked.ID)
	}
}

func TestSchedulerSkipsStaleAndDraining(t *testing.T) {
	now := time.Now()
	s := schedAt(now)
	stale := freshInst("stale", 9, 0, 0)
	stale.LastHeartbeat = now.Add(-2 * time.Minute)
	draining := freshInst("draining", 9, 0, 0)
	draining.Status = InstanceDraining
	ok := freshInst("ok", 1, 0, 0)

	picked, err := s.Pick([]Instance{stale, draining, ok}, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if picked.ID != "ok" {
		t.Fatalf("心跳过期/排空中的实例不该被选中，实际 %s", picked.ID)
	}
}

func TestSchedulerBreakerThenFallback(t *testing.T) {
	now := time.Now()
	s := schedAt(now)
	broken := freshInst("broken", 9, 0, 0)
	broken.Failures = 3 // 达到熔断阈值
	ok := freshInst("ok", 1, 0, 0)

	picked, err := s.Pick([]Instance{broken, ok}, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if picked.ID != "ok" {
		t.Fatalf("熔断实例不该被优先选中，实际 %s", picked.ID)
	}

	// 只剩熔断实例时退化为它：熔断是概率判断，不是硬故障，
	// 直接失败会把「一次抖动」放大成「整个技能不可用」。
	picked, err = s.Pick([]Instance{broken}, nil)
	if err != nil {
		t.Fatalf("只剩熔断实例时应退化可用: %v", err)
	}
	if picked.ID != "broken" {
		t.Fatalf("应退化为 broken，实际 %s", picked.ID)
	}
}

func TestSchedulerExcludeOnRetry(t *testing.T) {
	now := time.Now()
	s := schedAt(now)
	a := freshInst("a", 2, 0, 0)
	b := freshInst("b", 1, 0, 0)
	picked, err := s.Pick([]Instance{a, b}, map[string]struct{}{"a": {}})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if picked.ID != "b" {
		t.Fatalf("重试应换实例，实际 %s", picked.ID)
	}
}

func TestSchedulerNoCandidate(t *testing.T) {
	now := time.Now()
	s := schedAt(now)
	if _, err := s.Pick(nil, nil); !errors.Is(err, ErrNoWorker) {
		t.Fatalf("空列表应返回 ErrNoWorker: %v", err)
	}
	stale := freshInst("stale", 1, 0, 0)
	stale.LastHeartbeat = now.Add(-time.Hour)
	if _, err := s.Pick([]Instance{stale}, nil); !errors.Is(err, ErrNoWorker) {
		t.Fatalf("全部离线应返回 ErrNoWorker: %v", err)
	}
}

func TestSchedulerFullInstanceStillCandidate(t *testing.T) {
	now := time.Now()
	s := schedAt(now)
	// 在途 = 上限：Healthy 为 false，但仍在心跳 → 作为兜底候选（背压而不是硬失败）
	full := freshInst("full", 1, 10, 0)
	list := s.Candidates([]Instance{full}, nil)
	if len(list) != 1 || list[0].ID != "full" {
		t.Fatalf("满载实例应保留为兜底候选: %+v", list)
	}
}
