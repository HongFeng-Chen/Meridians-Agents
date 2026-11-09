package service

// 统一操作历史查询面：实现 api/history/v1 的 HistoryService。
// 数据由各 biz（mcp / memory / rag / skill）经 history.Recorder 异步落库（op_histories 表）。

import (
	"context"

	v1 "go_agent/api/history/v1"
	"go_agent/internal/biz/history"
)

// HistoryService 操作历史查询服务。
type HistoryService struct {
	v1.UnimplementedHistoryServiceServer

	uc *history.UseCase
}

var (
	_ v1.HistoryServiceHTTPServer = (*HistoryService)(nil)
	_ v1.HistoryServiceServer     = (*HistoryService)(nil)
)

// NewHistoryService 创建操作历史查询服务
func NewHistoryService(uc *history.UseCase) *HistoryService {
	return &HistoryService{uc: uc}
}

// ListHistories 操作历史列表。
func (s *HistoryService) ListHistories(ctx context.Context, req *v1.ListHistoriesRequest) (*v1.ListHistoriesReply, error) {
	f := history.Filter{
		TenantID: auditTenant(ctx), // 普通用户只看本租户；管理员可看全平台
		UserID:   auditScope(ctx),  // 再收窄到本人记录
		Kind:     req.GetKind(),
		TaskID:   req.GetTaskId(),
		ConvID:   req.GetConvId(),
		Ref:      req.GetRef(),
		Limit:    int(req.GetLimit()),
		Offset:   int(req.GetOffset()),
	}
	switch req.GetStatus() {
	case "ok":
		t := true
		f.OK = &t
	case "fail":
		t := false
		f.OK = &t
	}
	list, total, err := s.uc.List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := &v1.ListHistoriesReply{Histories: make([]*v1.History, 0, len(list)), Total: total}
	for _, h := range list {
		out.Histories = append(out.Histories, toHistory(h))
	}
	return out, nil
}

// GetHistoryStats 操作历史统计。
func (s *HistoryService) GetHistoryStats(ctx context.Context, _ *v1.GetHistoryStatsRequest) (*v1.HistoryStats, error) {
	st, err := s.uc.Stats(ctx, auditTenant(ctx), auditScope(ctx))
	if err != nil {
		return nil, err
	}
	out := &v1.HistoryStats{Total: st.Total, ByKind: make([]*v1.HistoryKindStat, 0, len(st.ByKind))}
	for _, k := range st.ByKind {
		out.ByKind = append(out.ByKind, &v1.HistoryKindStat{
			Kind:  k.Kind,
			Label: history.KindLabels[k.Kind],
			Count: k.Count,
		})
	}
	return out, nil
}

func toHistory(h history.Record) *v1.History {
	return &v1.History{
		Id:        h.ID,
		Kind:      h.Kind,
		TaskId:    h.TaskID,
		AgentKey:  h.AgentKey,
		ConvId:    h.ConvID,
		Ref:       h.Ref,
		Query:     h.Query,
		Summary:   h.Summary,
		Detail:    h.Detail,
		Ok:        h.OK,
		LatencyMs: h.LatencyMS,
		CreatedAt: formatTS(h.CreatedAt),
	}
}
