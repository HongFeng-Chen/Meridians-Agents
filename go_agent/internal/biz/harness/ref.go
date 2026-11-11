// ref.go 评测运行的成本归属键。
//
// 问题：一次评测的 LLM 成本有三路来源——被测 Agent 内部的调用、model: 目标的
// 直连调用、LLM-as-Judge 的判分调用。前两者在 agent 目标下才有任务行（task_id），
// judge 和 model: 目标根本没有任务，成本就变成了无主的 tokens：
// 能查到「今天花了多少」，查不到「这一次评测花了多少」。
//
// 解法：在评测运行的 context 上挂一个 (harness_run, runID) 归属键，
// 顺着 ctx 一路传到 llm.Manager，由它写进 usage_records.ref_kind/ref_id。
// 不引任务表新列，也不要求每条调用自带归属——只在评测这一个入口挂一次。
//
// 注意这一键是**同时**写进 llm 的调用元数据（llm.WithMeta）：只挂在 harness
// 自己的 context key 上是不够的——agent: 目标会进入 agentfactory.runGraph，
// 那里重新设置 CallMeta 时只能看到 llm 元数据，看不到私有 key，ref 就断了。
package harness

import (
	"context"
	"strconv"

	"go_agent/internal/biz/llm"
)

// RefKindRun 评测运行的成本归属类型（与 usage.RefHarnessRun 取值一致）。
const RefKindRun = "harness_run"

type runRefKey struct{}

// WithRunRef 把评测运行挂到 context 上（runID 为 0 时原样返回）。
// 同时写入 llm 调用元数据，agent 内部与直连模型的调用都能继承。
func WithRunRef(ctx context.Context, runID uint64) context.Context {
	if runID == 0 {
		return ctx
	}
	ctx = context.WithValue(ctx, runRefKey{}, runID)
	return llm.WithMeta(ctx, llm.CallMeta{
		RefKind: RefKindRun,
		RefID:   strconv.FormatUint(runID, 10),
	})
}

// RunRefFrom 取评测运行 ID（0 = 不在评测上下文里）。
func RunRefFrom(ctx context.Context) uint64 {
	id, _ := ctx.Value(runRefKey{}).(uint64)
	return id
}
