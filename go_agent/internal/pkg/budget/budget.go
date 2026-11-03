// Package budget 运行预算：给一次 Agent 执行（含其派生的子任务）设 token / 调用次数上限。
//
// 背景：平台已有「事后账单」（llm_calls 表 + 按 task/agent 下钻），但没有「事前闸门」——
// 一个计划 6 步、每步多轮工具调用的任务，可以无声地把额度烧穿。这里提供最小闭环：
//
//	ctx = budget.With(ctx, budget.New(maxTokens, maxCalls))
//	... 每次 LLM 调用前 budget.From(ctx).Check()，调用后 Charge(usage.TotalTokens)
//	... 超限返回 ErrExceeded，执行图按「预算耗尽」收敛而不是硬失败
//
// 预算通过 context 传递：父任务创建的子任务自然共用同一份额度（而不是各自领一份）。
package budget

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrExceeded 预算耗尽（可被 errors.Is 判定，执行图据此走收敛分支）
var ErrExceeded = errors.New("运行预算已耗尽")

// Snapshot 预算快照（写入任务事件 / 接口返回，便于前端展示与排错）
type Snapshot struct {
	MaxTokens  int64 `json:"max_tokens"`
	UsedTokens int64 `json:"used_tokens"`
	MaxCalls   int32 `json:"max_calls"`
	UsedCalls  int32 `json:"used_calls"`
	Exceeded   bool  `json:"exceeded"`
}

// Budget 并发安全的预算计数器（一个执行树共享一个实例）
type Budget struct {
	maxTokens int64
	maxCalls  int32
	usedToken atomic.Int64
	usedCall  atomic.Int32
	exceeded  atomic.Bool
}

// New 创建预算。maxTokens / maxCalls 均 <=0 表示不限制（返回 nil，Check 恒通过）。
func New(maxTokens int64, maxCalls int32) *Budget {
	if maxTokens <= 0 && maxCalls <= 0 {
		return nil
	}
	return &Budget{maxTokens: maxTokens, maxCalls: maxCalls}
}

// Limited 是否设了上限
func (b *Budget) Limited() bool { return b != nil && (b.maxTokens > 0 || b.maxCalls > 0) }

// Check 调用前检查：已超限则直接拒绝，避免「明知超额还发一次请求」。
func (b *Budget) Check() error {
	if b == nil {
		return nil
	}
	if b.exceeded.Load() {
		return ErrExceeded
	}
	if b.maxTokens > 0 && b.usedToken.Load() >= b.maxTokens {
		b.exceeded.Store(true)
		return ErrExceeded
	}
	if b.maxCalls > 0 && b.usedCall.Load() >= b.maxCalls {
		b.exceeded.Store(true)
		return ErrExceeded
	}
	return nil
}

// Charge 调用后记账。超过上限返回 ErrExceeded（本次调用已发生，仍要记账）。
func (b *Budget) Charge(tokens int) error {
	if b == nil {
		return nil
	}
	if tokens < 0 {
		tokens = 0
	}
	b.usedToken.Add(int64(tokens))
	calls := b.usedCall.Add(1)
	if b.maxTokens > 0 && b.usedToken.Load() > b.maxTokens {
		b.exceeded.Store(true)
		return ErrExceeded
	}
	if b.maxCalls > 0 && calls > b.maxCalls {
		b.exceeded.Store(true)
		return ErrExceeded
	}
	return nil
}

// Snapshot 读取当前用量
func (b *Budget) Snapshot() *Snapshot {
	if b == nil {
		return nil
	}
	return &Snapshot{
		MaxTokens:  b.maxTokens,
		UsedTokens: b.usedToken.Load(),
		MaxCalls:   b.maxCalls,
		UsedCalls:  b.usedCall.Load(),
		Exceeded:   b.exceeded.Load(),
	}
}

type ctxKey struct{}

// With 把预算写入 context
func With(ctx context.Context, b *Budget) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, b)
}

// From 读取预算；未设置返回 nil（调用方需 nil 安全）
func From(ctx context.Context) *Budget {
	if ctx == nil {
		return nil
	}
	if b, ok := ctx.Value(ctxKey{}).(*Budget); ok {
		return b
	}
	return nil
}

// Ensure 保证 context 里有一份预算：已有则沿用（子任务共用父任务额度），
// 没有则按参数创建。
func Ensure(ctx context.Context, maxTokens int64, maxCalls int32) context.Context {
	if From(ctx) != nil {
		return ctx
	}
	return With(ctx, New(maxTokens, maxCalls))
}
