package budget

import (
	"context"
	"errors"
	"testing"
)

func TestNilBudget(t *testing.T) {
	var b *Budget
	if b.Limited() {
		t.Fatal("nil 预算不应有限额")
	}
	if err := b.Check(); err != nil {
		t.Fatalf("nil check: %v", err)
	}
	if err := b.Charge(100); err != nil {
		t.Fatalf("nil charge: %v", err)
	}
	if b.Snapshot() != nil {
		t.Fatal("nil 快照应为 nil")
	}
}

func TestTokenLimit(t *testing.T) {
	b := New(100, 0)
	if err := b.Charge(60); err != nil {
		t.Fatalf("首次记账应通过: %v", err)
	}
	if err := b.Check(); err != nil {
		t.Fatalf("未超额应通过: %v", err)
	}
	if err := b.Charge(50); !errors.Is(err, ErrExceeded) {
		t.Fatalf("超额应返回 ErrExceeded，得到 %v", err)
	}
	if err := b.Check(); !errors.Is(err, ErrExceeded) {
		t.Fatalf("超限后 Check 应拒绝，得到 %v", err)
	}
	s := b.Snapshot()
	if s.UsedTokens != 110 || !s.Exceeded || s.UsedCalls != 2 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
}

func TestCallLimit(t *testing.T) {
	b := New(0, 2)
	_ = b.Charge(1)
	_ = b.Charge(1)
	if err := b.Charge(1); !errors.Is(err, ErrExceeded) {
		t.Fatalf("超过调用次数应拒绝，得到 %v", err)
	}
}

func TestEnsureSharesParent(t *testing.T) {
	// 无父预算：按参数创建
	ctx := Ensure(context.Background(), 10, 1)
	if b := From(ctx); b == nil || b.Snapshot().MaxTokens != 10 {
		t.Fatalf("无父预算时应创建：%+v", b)
	}
	// 有父预算（子任务场景）：沿用，不重复领额度
	parent := New(1000, 10)
	ctx = With(context.Background(), parent)
	ctx = Ensure(ctx, 10, 1)
	if got := From(ctx); got != parent {
		t.Fatal("已有预算时应沿用父预算（子任务共用额度）")
	}
}
