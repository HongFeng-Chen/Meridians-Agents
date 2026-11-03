// Package ratelimit 进程内令牌桶限流。
//
// 为什么是进程内而不是 Redis 分布式：单个实例的配额保护（防脚本刷接口、
// 防上游被自己打挂）用本地令牌桶就够了，且没有网络往返；多实例部署时
// 每实例独立计数，总量 = 单实例配额 × 副本数，需要全局配额时再换 Redis 实现
// （Limiter 是接口化的，替换只影响 server 中间件一行）。
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter 令牌桶限流器，按 key（租户 / 接口）独立计量。
type Limiter struct {
	qps   float64
	burst float64

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

// New 创建限流器。qps<=0 表示不限流（Allow 恒真）。
func New(qps float64, burst int) *Limiter {
	if burst <= 0 {
		burst = 1
	}
	return &Limiter{
		qps:       qps,
		burst:     float64(burst),
		buckets:   make(map[string]*bucket),
		lastSweep: time.Now(),
	}
}

// Enabled 是否真正启用限流
func (l *Limiter) Enabled() bool { return l != nil && l.qps > 0 }

// Allow 取一个令牌。返回 false 时同时给出建议的重试等待时间（Retry-After）。
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if !l.Enabled() {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	// 按经过时间补充令牌，封顶 burst
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.qps
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// 还差多少令牌 → 等多久
	wait := time.Duration((1 - b.tokens) / l.qps * float64(time.Second))
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return false, wait
}

// sweepLocked 清理长时间空闲的桶，避免 key 无限增长（租户数 × 路由数）。
func (l *Limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute && b.tokens >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// Size 当前桶数量（观测/排错用）
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
