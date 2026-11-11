// usage_sink.go 成本记账适配器：llm.CostSink → usage_records。
//
// 为什么绕过 usage.UseCase：
// UseCase.Record 要求 principal（「谁在记账」），而这里的调用方是 worker 后台协程，
// 根本没有请求身份——它记的是「任务跑了多少 token」。租户与用户来自 LLM 调用时
// 的 context（identity.From），已经固化在 CostEvent 里，不需要再过一次鉴权。
// 换句话说：这是平台代记，不是用户自助上报。
package repo

import (
	"context"
	"strconv"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/usage"
)

// CostSink 把每次 LLM 调用折算成一条追加式用量记录。
type CostSink struct {
	repo usage.Repo
}

var _ llm.CostSink = (*CostSink)(nil)

// NewCostSink 构造记账适配器
func NewCostSink(repo usage.Repo) *CostSink { return &CostSink{repo: repo} }

// RecordCost 落一条用量记录。
//
// 失败直接返回错误（由 llm.Manager 记日志）：账单少一条是会计问题，
// 不能静默吞掉；但也绝不能因此让 LLM 调用本身失败。
func (s *CostSink) RecordCost(ctx context.Context, ev llm.CostEvent) error {
	if s == nil || s.repo == nil {
		return nil
	}
	// task_id 在 usage_records 是字符串列：与 trace 属性里的 task.id 同构，
	// 这样「按任务查成本」和「按任务查链路」用的是同一个键。
	taskID := ""
	if ev.TaskID > 0 {
		taskID = strconv.FormatUint(ev.TaskID, 10)
	}
	return s.repo.Add(ctx, &usage.Record{
		TenantID:     ev.TenantID,
		SubjectID:    ev.UserID,
		AgentID:      ev.AgentKey,
		TaskID:       taskID,
		Provider:     ev.Provider,
		Model:        ev.Model,
		Kind:         usage.KindLLM,
		InputTokens:  ev.PromptTokens,
		OutputTokens: ev.CompletionTokens,
		TotalTokens:  ev.TotalTokens,
		CostMicros:   ev.CostMicros,
		Currency:     ev.Currency,
		PriceVersion: ev.PriceVersion,
		CreatedAt:    time.Now(),
	})
}
