package ratelimit

import (
	"testing"
	"time"
)

func TestAllowBurstThenReject(t *testing.T) {
	l := New(1, 3) // 1 QPS，桶容量 3
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("t1"); !ok {
			t.Fatalf("第 %d 次应放行（桶容量内）", i+1)
		}
	}
	ok, wait := l.Allow("t1")
	if ok {
		t.Fatal("超出桶容量应被拒绝")
	}
	if wait <= 0 {
		t.Fatal("拒绝时应给出重试等待时间")
	}
	// 另一个 key 互不影响
	if ok, _ := l.Allow("t2"); !ok {
		t.Fatal("不同 key 应有独立令牌桶")
	}
}

func TestRefill(t *testing.T) {
	l := New(100, 1) // 100 QPS
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("首次应放行")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("令牌耗尽应拒绝")
	}
	time.Sleep(30 * time.Millisecond) // 100 QPS → 30ms 补充 3 个令牌
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("补充后应放行")
	}
}

func TestDisabled(t *testing.T) {
	l := New(0, 0)
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatal("未启用限流时应恒放行")
		}
	}
	if l.Enabled() {
		t.Fatal("qps=0 应视为未启用")
	}
}
