package service

// LLM 调用历史查询面：实现 api/llm/v1 的 LLMCallQueryService。
// 数据由 llm.Manager 在每次 LLM 调用后异步落库（llm_calls 表）。

import (
	"context"
	"strings"
	"time"

	v1 "go_agent/api/llm/v1"
	"go_agent/internal/biz/llm"
)

// parseTimeArg 解析查询时间参数：RFC3339 全格式，或日期形式（当天起止）。
// 解析失败返回 ok=false（按未传处理，不让脏参数 500）。
func parseTimeArg(s string, endOfDay bool) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		if endOfDay {
			return t.Add(24*time.Hour - time.Second), true
		}
		return t, true
	}
	return time.Time{}, false
}

// LLMCallQueryService 调用历史查询服务。
type LLMCallQueryService struct {
	v1.UnimplementedLLMCallQueryServiceServer

	uc *llm.Manager
}

var (
	_ v1.LLMCallQueryServiceHTTPServer = (*LLMCallQueryService)(nil)
	_ v1.LLMCallQueryServiceServer     = (*LLMCallQueryService)(nil)
)

// NewLLMCallQueryService 创建调用历史查询服务
func NewLLMCallQueryService(uc *llm.Manager) *LLMCallQueryService {
	return &LLMCallQueryService{uc: uc}
}

// ListLLMCalls 调用历史。
func (s *LLMCallQueryService) ListLLMCalls(ctx context.Context, req *v1.ListLLMCallsRequest) (*v1.ListLLMCallsReply, error) {
	f := llm.CallFilter{
		TaskID:   req.GetTaskId(),
		Model:    req.GetModel(),
		Models:   req.GetModels(),
		Provider: req.GetProvider(),
		Purpose:  req.GetPurpose(),
		Limit:    int(req.GetLimit()),
		Offset:   int(req.GetOffset()),
	}
	// 时间范围：RFC3339；日期形式（2026-10-30）按当天 00:00 ~ 23:59:59 处理
	if t, ok := parseTimeArg(req.GetSince(), false); ok {
		f.Since = t
	}
	if t, ok := parseTimeArg(req.GetUntil(), true); ok {
		f.Until = t
	}
	switch req.GetStatus() {
	case "ok":
		t := true
		f.OK = &t
	case "fail":
		t := false
		f.OK = &t
	}
	list, total, err := s.uc.ListCalls(ctx, f)
	if err != nil {
		return nil, mapLLMErr(err)
	}
	out := &v1.ListLLMCallsReply{Calls: make([]*v1.LLMCall, 0, len(list)), Total: total}
	for _, c := range list {
		out.Calls = append(out.Calls, toLLMCall(c))
	}
	return out, nil
}

// GetLLMCallStats 调用统计。
func (s *LLMCallQueryService) GetLLMCallStats(ctx context.Context, _ *v1.GetLLMCallStatsRequest) (*v1.LLMCallStats, error) {
	st, err := s.uc.CallStats(ctx)
	if err != nil {
		return nil, mapLLMErr(err)
	}
	out := &v1.LLMCallStats{
		Calls:            st.Calls,
		PromptTokens:     st.PromptTokens,
		CompletionTokens: st.CompletionTokens,
		TotalTokens:      st.TotalTokens,
		Failures:         st.Failures,
		ByModel:          make([]*v1.LLMModelUsage, 0, len(st.ByModel)),
	}
	for _, m := range st.ByModel {
		out.ByModel = append(out.ByModel, &v1.LLMModelUsage{
			Model:            m.Model,
			Calls:            m.Calls,
			PromptTokens:     m.PromptTokens,
			CompletionTokens: m.CompletionTokens,
		})
	}
	return out, nil
}

// BreakdownTokens token 用量下钻：按 agent/scene/purpose/model 维度聚合排行。
func (s *LLMCallQueryService) BreakdownTokens(ctx context.Context, req *v1.BreakdownRequest) (*v1.BreakdownReply, error) {
	dim := strings.TrimSpace(req.GetDim())
	if !llm.BreakdownDims[dim] {
		dim = "agent_key"
	}
	rows, err := s.uc.UsageBreakdown(ctx, dim, int(req.GetDays()))
	if err != nil {
		return nil, mapLLMErr(err)
	}
	out := &v1.BreakdownReply{Dim: dim, Rows: make([]*v1.BreakdownRow, 0, len(rows))}
	for _, r := range rows {
		out.Rows = append(out.Rows, &v1.BreakdownRow{
			Key:              r.Key,
			Calls:            r.Calls,
			PromptTokens:     r.PromptTokens,
			CompletionTokens: r.CompletionTokens,
			TotalTokens:      r.TotalTokens,
			Failures:         r.Failures,
		})
	}
	return out, nil
}

func toLLMCall(c llm.CallRecord) *v1.LLMCall {
	return &v1.LLMCall{
		Id:               c.ID,
		Provider:         c.Provider,
		Model:            c.Model,
		Scene:            c.Scene,
		Purpose:          c.Purpose,
		TaskId:           c.TaskID,
		AgentKey:         c.AgentKey,
		Temperature:      c.Temperature,
		DurationMs:       c.DurationMS,
		Ok:               c.OK,
		ErrorMsg:         c.ErrorMsg,
		PromptChars:      int32(c.PromptChars),
		ReplyChars:       int32(c.ReplyChars),
		PromptTokens:     int32(c.PromptTokens),
		CompletionTokens: int32(c.CompletionTokens),
		TotalTokens:      int32(c.TotalTokens),
		PromptText:       c.PromptText,
		ReplyText:        c.ReplyText,
		CreatedAt:        formatTS(c.CreatedAt),
	}
}
