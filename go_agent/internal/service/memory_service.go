package service

// 记忆系统管理面。
//
// 实现 api/memory/v1/memory.proto：短期会话（Redis）、长期记忆（MySQL + Milvus +
// 图谱）、召回与问答。业务规则全部在 internal/biz/memory。

import (
	"context"
	"strings"
	"time"

	v1 "go_agent/api/memory/v1"
	ragv1 "go_agent/api/rag/v1"
	"go_agent/internal/biz/memory"
	"go_agent/internal/conf"
)

// MemoryService 记忆系统管理面：短期会话、长期记忆、向量召回、图谱、统计。
type MemoryService struct {
	v1.UnimplementedMemoryServiceServer

	uc  *memory.UseCase
	cfg *conf.Data
}

var (
	_ v1.MemoryServiceHTTPServer = (*MemoryService)(nil)
	_ v1.MemoryServiceServer     = (*MemoryService)(nil)
)

// NewMemoryService 创建记忆服务
func NewMemoryService(uc *memory.UseCase, c *conf.Data) *MemoryService {
	return &MemoryService{uc: uc, cfg: c}
}

// ---------- 短期记忆 ----------

// AppendShort 追加一条会话消息。
func (s *MemoryService) AppendShort(ctx context.Context, req *v1.AppendShortRequest) (*v1.AppendShortReply, error) {
	if strings.TrimSpace(req.GetSessionId()) == "" || strings.TrimSpace(req.GetContent()) == "" {
		return nil, badRequest("请求体无效，需要 session_id 与 content 字段")
	}
	if err := s.uc.AppendMessage(ctx, req.GetSessionId(), req.GetRole(), req.GetContent()); err != nil {
		return nil, mapMemoryErr(err)
	}
	n, err := s.uc.SessionLen(ctx, req.GetSessionId())
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	return &v1.AppendShortReply{SessionId: req.GetSessionId(), Messages: int32(n)}, nil
}

// ListShort 读取会话历史。
func (s *MemoryService) ListShort(ctx context.Context, req *v1.ListShortRequest) (*v1.ListShortReply, error) {
	if strings.TrimSpace(req.GetSessionId()) == "" {
		return nil, badRequest("缺少 session_id")
	}
	msgs, err := s.uc.History(ctx, req.GetSessionId(), int(req.GetLimit()))
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	n, err := s.uc.SessionLen(ctx, req.GetSessionId())
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	out := &v1.ListShortReply{
		SessionId: req.GetSessionId(),
		Messages:  make([]*v1.ShortMessage, 0, len(msgs)),
		Total:     int32(n),
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, &v1.ShortMessage{
			Role:    m.Role,
			Content: m.Content,
			At:      m.At.Format(time.RFC3339),
		})
	}
	return out, nil
}

// ClearShort 清空会话（删除 Redis key）。
func (s *MemoryService) ClearShort(ctx context.Context, req *v1.ClearShortRequest) (*v1.ClearShortReply, error) {
	if strings.TrimSpace(req.GetSessionId()) == "" {
		return nil, badRequest("缺少 session_id")
	}
	if err := s.uc.ClearSession(ctx, req.GetSessionId()); err != nil {
		return nil, mapMemoryErr(err)
	}
	return &v1.ClearShortReply{SessionId: req.GetSessionId(), Cleared: true}, nil
}

// ---------- 长期记忆 ----------

// Remember 写入长期记忆（向量化 + 可选图谱抽取）。
func (s *MemoryService) Remember(ctx context.Context, req *v1.RememberRequest) (*v1.RememberResult, error) {
	if strings.TrimSpace(req.GetContent()) == "" {
		return nil, badRequest("请求体无效，需要 content 字段")
	}
	in := memory.RememberInput{
		ID:         req.GetId(),
		UserID:     scopedUserID(ctx, req.GetUserId()),
		Kind:       req.GetKind(),
		Content:    req.GetContent(),
		Importance: int(req.GetImportance()),
		Tags:       req.GetTags(),
		Source:     req.GetSource(),
		Graph:      req.Graph,
	}
	// 过期时间：显式 expire_at 优先，其次 ttl_days
	if req.GetExpireAt() != "" {
		t, err := time.Parse(time.RFC3339, req.GetExpireAt())
		if err != nil {
			return nil, badRequest("expire_at 需为 RFC3339 格式，如 2027-01-01T00:00:00Z")
		}
		in.ExpiresAt = &t
	} else if req.GetTtlDays() > 0 {
		in.TTL = time.Duration(req.GetTtlDays()) * 24 * time.Hour
	}
	res, err := s.uc.Remember(ctx, in)
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	return toRememberResult(res), nil
}

// ListLong 长期记忆列表。
func (s *MemoryService) ListLong(ctx context.Context, req *v1.ListLongRequest) (*v1.ListLongReply, error) {
	mems, err := s.uc.List(ctx, scopedUserID(ctx, req.GetUserId()), req.GetIncludeExpired(), int(req.GetLimit()))
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	out := &v1.ListLongReply{Memories: make([]*v1.LongTermMemory, 0, len(mems)), Total: int32(len(mems))}
	for _, m := range mems {
		out.Memories = append(out.Memories, toLongTerm(m))
	}
	return out, nil
}

// GetLong 单条长期记忆。
//
// 记忆 ID 是可枚举的（mem_xxx 带时间前缀），只按 ID 读取等于「知道 ID 就能看」。
// 先取回再校验归属：不匹配返回 403，且不透露内容。
func (s *MemoryService) GetLong(ctx context.Context, req *v1.GetLongRequest) (*v1.LongTermMemory, error) {
	m, err := s.uc.Get(ctx, req.GetId())
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	if err := ensureOwned(ctx, m.UserID, "长期记忆"); err != nil {
		return nil, err
	}
	return toLongTerm(m), nil
}

// DeleteLong 删除单条长期记忆（含向量与关联图谱）。
//
// 删除比读取更危险：先查归属，再执行级联删除（MySQL + Milvus + 图谱）。
// 顺序不能反——删完再报 403，数据已经没了。
func (s *MemoryService) DeleteLong(ctx context.Context, req *v1.DeleteLongRequest) (*v1.RememberResult, error) {
	m, err := s.uc.Get(ctx, req.GetId())
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	if err := ensureOwned(ctx, m.UserID, "长期记忆"); err != nil {
		return nil, err
	}
	res, err := s.uc.Delete(ctx, req.GetId())
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	return toRememberResult(res), nil
}

// PurgeExpired 清理已过期记忆。
func (s *MemoryService) PurgeExpired(ctx context.Context, req *v1.PurgeExpiredRequest) (*v1.PurgeExpiredReply, error) {
	n, err := s.uc.PurgeExpired(ctx, int(req.GetLimit()))
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	return &v1.PurgeExpiredReply{Purged: int32(n)}, nil
}

// ---------- 召回与问答 ----------

// Recall 向量 + 图谱召回。
func (s *MemoryService) Recall(ctx context.Context, req *v1.RecallRequest) (*v1.RecallResult, error) {
	if strings.TrimSpace(req.GetQuery()) == "" {
		return nil, badRequest("请求体无效，需要 query 字段")
	}
	withGraph := true
	if req.WithGraph != nil {
		withGraph = req.GetWithGraph()
	}
	res, err := s.uc.Recall(ctx, req.GetQuery(), scopedUserID(ctx, req.GetUserId()), int(req.GetTopK()), withGraph, memory.RecallSourceAPI)
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	return toRecallResult(res), nil
}

// Answer 基于记忆的多轮问答。
func (s *MemoryService) Answer(ctx context.Context, req *v1.AnswerRequest) (*v1.MemoryAnswer, error) {
	if strings.TrimSpace(req.GetQuestion()) == "" {
		return nil, badRequest("请求体无效，需要 question 字段")
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		sessionID = "default"
	}
	// 归属键必须走 scopedUserID：认证态忽略请求体 user_id，
	// 否则「登录用户 + 别人的 user_id」就能读到对方的长期记忆
	ans, err := s.uc.Answer(ctx, sessionID, scopedUserID(ctx, req.GetUserId()), req.GetQuestion())
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	return &v1.MemoryAnswer{
		SessionId: ans.SessionID,
		Question:  ans.Question,
		Answer:    ans.Answer,
		Recall:    toRecallResult(ans.Recall),
	}, nil
}

// GetMemoryStats 规模统计。
func (s *MemoryService) GetMemoryStats(ctx context.Context, _ *v1.GetMemoryStatsRequest) (*v1.MemoryStats, error) {
	st, err := s.uc.Stats(ctx)
	if err != nil {
		return nil, mapMemoryErr(err)
	}
	return &v1.MemoryStats{
		MemoriesTotal:   anyInt(st["memories_total"]),
		MemoriesExpired: anyInt(st["memories_expired"]),
		VectorEntities:  anyInt(st["vector_entities"]),
		GraphEntities:   anyInt(st["graph_entities"]),
		GraphRelations:  anyInt(st["graph_relations"]),
	}, nil
}

// GetMemoryInfo 运行参数。
func (s *MemoryService) GetMemoryInfo(_ context.Context, _ *v1.GetMemoryInfoRequest) (*v1.MemoryInfo, error) {
	m := s.uc.Info()
	return &v1.MemoryInfo{
		EmbedModel: anyString(m["embed_model"]),
		EmbedDim:   int32(anyInt(m["embed_dim"])),
		ShortTtl:   anyString(m["short_ttl"]),
		RecallTopK: int32(anyInt(m["recall_top_k"])),
		HistoryLim: int32(anyInt(m["history_lim"])),
		GraphOn:    anyBool(m["graph_on"]),
	}, nil
}

// ---------- 领域模型 → 协议 ----------

func toLongTerm(m *memory.LongTermMemory) *v1.LongTermMemory {
	if m == nil {
		return nil
	}
	out := &v1.LongTermMemory{
		Id:          m.ID,
		UserId:      m.UserID,
		Kind:        m.Kind,
		Content:     m.Content,
		Importance:  int32(m.Importance),
		Tags:        m.Tags,
		Source:      m.Source,
		RuleName:    m.RuleName,
		Entities:    int32(m.Entities),
		Relations:   int32(m.Relations),
		RecallCount: int32(m.RecallCount),
		CreatedAt:   m.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   m.UpdatedAt.Format(time.RFC3339),
		Deduped:     m.Deduped,
		Expired:     m.Expired,
	}
	if m.ExpiresAt != nil {
		s := m.ExpiresAt.Format(time.RFC3339)
		out.ExpiresAt = &s
	}
	if m.LastRecalledAt != nil {
		s := m.LastRecalledAt.Format(time.RFC3339)
		out.LastRecalledAt = &s
	}
	return out
}

func toRecalled(r memory.RecalledMemory) *v1.RecalledMemory {
	base := toLongTerm(r.LongTermMemory)
	return &v1.RecalledMemory{
		Id:             base.GetId(),
		UserId:         base.GetUserId(),
		Kind:           base.GetKind(),
		Content:        base.GetContent(),
		Importance:     base.GetImportance(),
		Tags:           base.GetTags(),
		Source:         base.GetSource(),
		RuleName:       base.GetRuleName(),
		Entities:       base.GetEntities(),
		Relations:      base.GetRelations(),
		RecallCount:    base.GetRecallCount(),
		LastRecalledAt: base.LastRecalledAt,
		ExpiresAt:      base.ExpiresAt,
		CreatedAt:      base.GetCreatedAt(),
		UpdatedAt:      base.GetUpdatedAt(),
		Deduped:        base.GetDeduped(),
		Expired:        base.GetExpired(),
		Score:          r.Score,
	}
}

func toRecallResult(res *memory.RecallResult) *v1.RecallResult {
	if res == nil {
		return nil
	}
	out := &v1.RecallResult{
		Query:    res.Query,
		UserId:   res.UserID,
		Memories: make([]*v1.RecalledMemory, 0, len(res.Memories)),
	}
	for _, m := range res.Memories {
		out.Memories = append(out.Memories, toRecalled(m))
	}
	if res.Graph != nil {
		out.Graph = &v1.GraphContext{
			Entities:  make([]*ragv1.Entity, 0, len(res.Graph.Entities)),
			Relations: make([]*ragv1.Relation, 0, len(res.Graph.Relations)),
		}
		for _, e := range res.Graph.Entities {
			out.Graph.Entities = append(out.Graph.Entities, ToProtoEntity(e))
		}
		for _, r := range res.Graph.Relations {
			out.Graph.Relations = append(out.Graph.Relations, ToProtoRelation(r))
		}
	}
	return out
}

func toRememberResult(res *memory.RememberResult) *v1.RememberResult {
	if res == nil {
		return &v1.RememberResult{}
	}
	return &v1.RememberResult{
		Memory:       toLongTerm(res.Memory),
		Deduped:      res.Deduped,
		Entities:     int32(res.Entities),
		Relations:    int32(res.Relations),
		GraphWarning: res.GraphWarning,
	}
}

// ---------- 记忆规则 ----------

func toRule(r *memory.MemoryRule) *v1.MemoryRule {
	if r == nil {
		return nil
	}
	return &v1.MemoryRule{
		Id:          r.ID,
		Name:        r.Name,
		Type:        r.Type,
		Kind:        r.Kind,
		Instruction: r.Instruction,
		Builtin:     r.Builtin,
		Enabled:     r.Enabled,
		CreatedAt:   r.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   r.UpdatedAt.Format(time.RFC3339),
	}
}

// ListMemoryRules 记忆规则列表
func (s *MemoryService) ListMemoryRules(ctx context.Context, _ *v1.ListMemoryRulesRequest) (*v1.ListMemoryRulesReply, error) {
	rules, err := s.uc.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	out := &v1.ListMemoryRulesReply{Rules: make([]*v1.MemoryRule, 0, len(rules))}
	for _, r := range rules {
		out.Rules = append(out.Rules, toRule(r))
	}
	return out, nil
}

// CreateMemoryRule 新建规则
func (s *MemoryService) CreateMemoryRule(ctx context.Context, req *v1.SaveMemoryRuleRequest) (*v1.SaveMemoryRuleReply, error) {
	r, err := s.uc.CreateRule(ctx, req.GetName(), req.GetType(), req.GetKind(), req.GetInstruction(), req.GetEnabled())
	if err != nil {
		return nil, err
	}
	return &v1.SaveMemoryRuleReply{Rule: toRule(r)}, nil
}

// UpdateMemoryRule 更新规则
func (s *MemoryService) UpdateMemoryRule(ctx context.Context, req *v1.UpdateMemoryRuleRequest) (*v1.SaveMemoryRuleReply, error) {
	r, err := s.uc.UpdateRule(ctx, req.GetId(), req.GetName(), req.GetKind(), req.GetInstruction(), req.GetEnabled())
	if err != nil {
		return nil, err
	}
	return &v1.SaveMemoryRuleReply{Rule: toRule(r)}, nil
}

// DeleteMemoryRule 删除自定义规则
func (s *MemoryService) DeleteMemoryRule(ctx context.Context, req *v1.DeleteMemoryRuleRequest) (*v1.DeleteMemoryRuleReply, error) {
	if err := s.uc.DeleteRule(ctx, req.GetId()); err != nil {
		return nil, err
	}
	return &v1.DeleteMemoryRuleReply{Deleted: true}, nil
}

// ---------- 记忆提取 ----------

func toExtracted(m memory.ExtractedMemory) *v1.ExtractedMemory {
	return &v1.ExtractedMemory{
		Content:    m.Content,
		Kind:       m.Kind,
		Importance: int32(m.Importance),
		Tags:       m.Tags,
		Deduped:    m.Deduped,
		MemoryId:   m.MemoryID,
		RuleId:     m.RuleID,
		RuleName:   m.RuleName,
		Error:      m.Error,
	}
}

// ExtractMemories 按规则从会话提取长期记忆
func (s *MemoryService) ExtractMemories(ctx context.Context, req *v1.ExtractMemoriesRequest) (*v1.ExtractMemoriesReply, error) {
	// 提取会把长期记忆写到 user_id 名下：同样不能信请求体
	res, err := s.uc.ExtractMemories(ctx, req.GetSessionId(), scopedUserID(ctx, req.GetUserId()), req.GetRuleIds(), int(req.GetLastN()))
	if err != nil {
		return nil, err
	}
	out := &v1.ExtractMemoriesReply{
		SessionId: res.SessionID,
		Results:   make([]*v1.RuleExtractResult, 0, len(res.Results)),
		Written:   int32(res.Written),
		Deduped:   int32(res.Deduped),
	}
	for _, r := range res.Results {
		rr := &v1.RuleExtractResult{
			RuleId:   r.RuleID,
			RuleName: r.RuleName,
			Error:    r.Error,
			Memories: make([]*v1.ExtractedMemory, 0, len(r.Memories)),
		}
		for _, m := range r.Memories {
			rr.Memories = append(rr.Memories, toExtracted(m))
		}
		out.Results = append(out.Results, rr)
	}
	return out, nil
}

// ---------- 检索历史 ----------

// ListRecallLogs 记忆检索历史
func (s *MemoryService) ListRecallLogs(ctx context.Context, req *v1.ListRecallLogsRequest) (*v1.ListRecallLogsReply, error) {
	logs, err := s.uc.ListRecallLogs(ctx, scopedUserID(ctx, req.GetUserId()), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := &v1.ListRecallLogsReply{Logs: make([]*v1.MemoryRecallLog, 0, len(logs))}
	for _, l := range logs {
		out.Logs = append(out.Logs, &v1.MemoryRecallLog{
			Id:        l.ID,
			UserId:    l.UserID,
			Query:     l.Query,
			Source:    l.Source,
			TopK:      int32(l.TopK),
			Hits:      int32(l.Hits),
			MemoryIds: l.MemoryIDs,
			LatencyMs: l.LatencyMS,
			CreatedAt: l.CreatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}
