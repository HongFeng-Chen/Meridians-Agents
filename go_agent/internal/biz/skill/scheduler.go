package skill

// 调度器：在若干 Worker 实例里挑一个执行本次技能调用。
//
// 为什么不把选实例的逻辑写在运行时里：
//   - 选实例是独立可测的决策（给定实例列表 → 返回哪个），与网络调用、记录落库无关；
//   - 将来换策略（一致性哈希 / 租户亲和 / 成本优先）只改这一个文件；
//   - 熔断与背压是「事实判断」，必须和被调用的副作用分开，否则测试要靠 mock 网络。
//
// 策略（确定性，便于测试与复现）：
//  1. 过滤：心跳在 TTL 内、状态可接单、未熔断、在途数 < 并发上限；
//  2. 打分：score = weight × (1 - inflight/max) / (1 + latency秒)；
//  3. 排序：score 降序 → 延迟升序 → 实例 ID 升序（同分时稳定）。

import (
	"sort"
	"time"
)

// Scheduler 实例选择器
type Scheduler struct {
	// TTL 心跳有效期：超过即视为离线（进程被杀、网络分区都会走到这里）
	TTL time.Duration
	// Breaker 连续失败熔断阈值（<=0 关闭熔断）
	Breaker int
	// Now 可注入时钟（测试用；为空取 time.Now）
	Now func() time.Time
}

// NewScheduler 构造调度器（TTL 缺省 45s，熔断阈值 3）
func NewScheduler(ttl time.Duration, breaker int) *Scheduler {
	if ttl <= 0 {
		ttl = 45 * time.Second
	}
	if breaker < 0 {
		breaker = 0
	}
	if breaker == 0 {
		breaker = DefaultBreakerThreshold
	}
	return &Scheduler{TTL: ttl, Breaker: breaker, Now: time.Now}
}

func (s *Scheduler) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Online 实例是否在线（心跳未过期）
func (s *Scheduler) Online(inst Instance, now time.Time) bool {
	if inst.LastHeartbeat.IsZero() {
		return false
	}
	return now.Sub(inst.LastHeartbeat) <= s.TTL
}

// Candidates 返回可用实例（按优先级排序）。exclude 为本次调用已失败过的实例 ID。
//
// 分两层：先给「健康且在线」的；一个都没有时退化为「在线但已熔断」的（
// 宁可慢也别直接失败——熔断只是概率判断，不是硬故障）。
// exclude 优先剔除，避免重试打到同一个坏实例上。
func (s *Scheduler) Candidates(instances []Instance, exclude map[string]struct{}) []Instance {
	now := s.now()
	healthy := make([]Instance, 0, len(instances))
	degraded := make([]Instance, 0, len(instances))
	for _, inst := range instances {
		if _, skip := exclude[inst.ID]; skip {
			continue
		}
		if !s.Online(inst, now) {
			continue
		}
		if inst.Healthy(s.Breaker) {
			healthy = append(healthy, inst)
			continue
		}
		// 熔断/满载但仍在心跳：作为兜底候选
		if inst.Status != InstanceOffline && inst.Status != InstanceDraining {
			degraded = append(degraded, inst)
		}
	}
	sortInstances(healthy)
	sortInstances(degraded)
	return append(healthy, degraded...)
}

// Pick 选一个实例；没有可用实例返回 ErrNoWorker
func (s *Scheduler) Pick(instances []Instance, exclude map[string]struct{}) (*Instance, error) {
	list := s.Candidates(instances, exclude)
	if len(list) == 0 {
		return nil, ErrNoWorker
	}
	chosen := list[0]
	return &chosen, nil
}

// sortInstances 按 score 降序 / 延迟升序 / ID 升序
func sortInstances(list []Instance) {
	sort.SliceStable(list, func(i, j int) bool {
		si, sj := score(list[i]), score(list[j])
		if si != sj {
			return si > sj
		}
		if list[i].LatencyMS != list[j].LatencyMS {
			return list[i].LatencyMS < list[j].LatencyMS
		}
		return list[i].ID < list[j].ID
	})
}

// score 权重 × 空闲度 / 延迟因子
func score(inst Instance) float64 {
	w := float64(inst.Weight)
	if w <= 0 {
		w = 1
	}
	idle := 1 - inst.Capacity()
	if idle <= 0 {
		idle = 0.01 // 满载实例仍保留极小概率，避免完全饿死
	}
	lat := float64(inst.LatencyMS) / 1000
	return w * idle / (1 + lat)
}
