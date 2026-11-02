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
		return r.llm.ChatTemp(ctx, "", input, 0.3)
	default:
		return "", fmt.Errorf("%w: %s", harness.ErrInvalidTarget, target)
	}
}
