// Package harnessrunner 评测被测对象执行器（harness.Runner 的 data 侧适配器）。
//
// 按 target 前缀分派黑盒执行：
//
//	agent:<agent_key> → agentfactory.UseCase.RunOnce（同步执行一次，复用执行图与
//	                    能力白名单，trace 事件 / LLM 调用记录自动落库，链路完整）
//	model:<model>     → llm.Manager 指定模型直连对话（模型级横评）
package harnessrunner

import (
	"context"
	"fmt"
	"strings"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/harness"
	"go_agent/internal/biz/llm"
)

// Runner 被测对象执行器
type Runner struct {
	af  *agentfactory.UseCase
	llm *llm.Manager
}

var _ harness.Runner = (*Runner)(nil)

// New 构造执行器
func New(af *agentfactory.UseCase, m *llm.Manager) *Runner {
	return &Runner{af: af, llm: m}
}

// Run 执行被测对象一次
func (r *Runner) Run(ctx context.Context, target, input string) (string, error) {
	target = strings.TrimSpace(target)
	switch {
	case strings.HasPrefix(target, harness.TargetAgentPrefix):
		if r.af == nil {
			return "", harness.ErrNoRunner
		}
		return r.af.RunOnce(ctx, strings.TrimPrefix(target, harness.TargetAgentPrefix), input)
	case strings.HasPrefix(target, harness.TargetModelPrefix):
		if r.llm == nil {
			return "", harness.ErrNoRunner
		}
		// 打上用途标记：trace 里能直接区分「评测直连模型」与「业务任务里的 LLM 调用」，
		// 否则一排 llm.chat 看不出是谁发起的
		ctx = llm.WithMeta(ctx, llm.CallMeta{Purpose: "harness.model"})
		return r.llm.ChatTemp(ctx, "", input, 0.3)
	default:
		return "", fmt.Errorf("%w: %s", harness.ErrInvalidTarget, target)
	}
}

// Resolve 解析被测对象的版本信息：评测报告里「这次测的是哪个版本」由它提供。
//
// 解析失败（Agent 不存在 / 尚无快照）返回零值并继续跑——版本绑定是记录项，
// 不该让一次评测因为「还没发布过快照」直接失败。
func (r *Runner) Resolve(ctx context.Context, target string) harness.TargetInfo {
	target = strings.TrimSpace(target)
	switch {
	case strings.HasPrefix(target, harness.TargetAgentPrefix):
		if r.af == nil {
			return harness.TargetInfo{}
		}
		key := strings.TrimPrefix(target, harness.TargetAgentPrefix)
		id, version := r.af.LatestSnapshotForKey(ctx, key)
		return harness.TargetInfo{SnapshotID: id, SnapshotVersion: version}
	case strings.HasPrefix(target, harness.TargetModelPrefix):
		return harness.TargetInfo{Model: strings.TrimPrefix(target, harness.TargetModelPrefix)}
	default:
		return harness.TargetInfo{}
	}
}
