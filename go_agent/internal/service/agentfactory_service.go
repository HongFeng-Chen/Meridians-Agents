package service

// AgentFactoryService：Agent 低代码工厂域的对外实现（HTTP/gRPC 双协议共用）。

import (
	"context"
	"fmt"
	"strings"

	v1 "go_agent/api/agentfactory/v1"
	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/pkg/identity"
)

type AgentFactoryService struct {
	v1.UnimplementedAgentFactoryServiceServer

	uc *agentfactory.UseCase
}

var (
	_ v1.AgentFactoryServiceHTTPServer = (*AgentFactoryService)(nil)
	_ v1.AgentFactoryServiceServer     = (*AgentFactoryService)(nil)
)

func NewAgentFactoryService(uc *agentfactory.UseCase) *AgentFactoryService {
	return &AgentFactoryService{uc: uc}
}

// ———— 统计与能力 ————

func (s *AgentFactoryService) GetAgentFactoryStats(ctx context.Context, _ *v1.GetAgentFactoryStatsRequest) (*v1.AgentFactoryStats, error) {
	st, err := s.uc.Stats(ctx)
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return &v1.AgentFactoryStats{
		Agents:        st.Agents,
		EnabledAgents: st.EnabledAgents,
		LlmBorn:       st.LLMBorn,
		Sandboxed:     st.Sandboxed,
		Vectorized:    st.Vectorized,
		Tasks:         st.Tasks,
		Pending:       st.Pending,
		Running:       st.Running,
		Succeeded:     st.Succeeded,
		Failed:        st.Failed,
		Canceled:      st.Canceled,
		Capabilities:  st.Capabilities,
	}, nil
}

func (s *AgentFactoryService) ListCapabilities(ctx context.Context, _ *v1.ListCapabilitiesRequest) (*v1.ListCapabilitiesReply, error) {
	caps, err := s.uc.Capabilities(ctx)
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	out := make([]*v1.CapabilitySchema, 0, len(caps))
	for _, c := range caps {
		out = append(out, &v1.CapabilitySchema{
			Name:        c.Name,
			Type:        c.Type,
			Description: c.Description,
			ArgsExample: c.ArgsExample,
			Disabled:    c.Disabled,
		})
	}
	return &v1.ListCapabilitiesReply{Capabilities: out}, nil
}

// ———— Agent 定义 ————

func (s *AgentFactoryService) ListAgentDefs(ctx context.Context, req *v1.ListAgentDefsRequest) (*v1.ListAgentDefsReply, error) {
	f := agentfactory.DefFilter{
		Keyword: req.GetKeyword(),
		Source:  req.GetSource(),
		Limit:   int(req.GetLimit()),
	}
	f.Enabled = parseBoolFilter(req.GetEnabled())
	defs, err := s.uc.ListDefs(ctx, f)
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	out := make([]*v1.AgentDef, 0, len(defs))
	for i := range defs {
		out = append(out, toAgentDef(&defs[i]))
	}
	return &v1.ListAgentDefsReply{Agents: out, Total: int64(len(out))}, nil
}

func (s *AgentFactoryService) CreateAgentDef(ctx context.Context, req *v1.AgentInput) (*v1.AgentDef, error) {
	def, err := s.uc.CreateDef(ctx, agentInputFromProto(req))
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return toAgentDef(def), nil
}

func (s *AgentFactoryService) CreateAgentByLLM(ctx context.Context, req *v1.CreateAgentByLLMRequest) (*v1.CreateAgentByLLMReply, error) {
	def, raw, err := s.uc.Factory().CreateByLLM(ctx, req.GetRequirement())
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return &v1.CreateAgentByLLMReply{Agent: toAgentDef(def), RawOutput: raw}, nil
}

func (s *AgentFactoryService) SearchAgentDefs(ctx context.Context, req *v1.SearchAgentDefsRequest) (*v1.SearchAgentDefsReply, error) {
	hits, err := s.uc.SearchDefs(ctx, req.GetQuery(), int(req.GetLimit()))
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	out := make([]*v1.AgentDefHit, 0, len(hits))
	for i := range hits {
		out = append(out, &v1.AgentDefHit{
			Agent:  toAgentDef(&hits[i].Agent),
			Score:  hits[i].Score,
			Source: hits[i].Source,
		})
	}
	return &v1.SearchAgentDefsReply{Hits: out}, nil
}

func (s *AgentFactoryService) SyncAgentVectors(ctx context.Context, _ *v1.SyncAgentVectorsRequest) (*v1.SyncAgentVectorsReply, error) {
	// 全量重建向量索引：一次调用会扫全库并写 Milvus，属运维动作
	if err := requireAdmin(ctx, "agentfactory.vectors.sync"); err != nil {
		return nil, err
	}
	n, err := s.uc.SyncAllVectors(ctx)
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return &v1.SyncAgentVectorsReply{Synced: int32(n)}, nil
}

func (s *AgentFactoryService) GetAgentDef(ctx context.Context, req *v1.GetAgentDefRequest) (*v1.AgentDef, error) {
	def, err := s.uc.GetDef(ctx, req.GetId())
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return toAgentDef(def), nil
}

func (s *AgentFactoryService) UpdateAgentDef(ctx context.Context, req *v1.UpdateAgentDefRequest) (*v1.AgentDef, error) {
	if req.GetInput() == nil {
		return nil, badRequest("input 必填")
	}
	def, err := s.uc.UpdateDef(ctx, req.GetId(), agentInputFromProto(req.GetInput()))
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return toAgentDef(def), nil
}

func (s *AgentFactoryService) DeleteAgentDef(ctx context.Context, req *v1.DeleteAgentDefRequest) (*v1.DeleteAgentDefReply, error) {
	// Agent 定义是共享资产（没有 owner 字段，列表对所有调用方可见），
	// 删除会把别人正在用的 Agent 一起摘掉，属于平台级操作。
	if err := requireAdmin(ctx, "agentfactory.def.delete"); err != nil {
		return nil, err
	}
	if err := s.uc.DeleteDef(ctx, req.GetId()); err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return &v1.DeleteAgentDefReply{Id: req.GetId()}, nil
}

func (s *AgentFactoryService) ToggleAgentDef(ctx context.Context, req *v1.ToggleAgentDefRequest) (*v1.AgentDef, error) {
	// 启停是全局生效的（停用后它作为子 Agent 也调不起来），同样是平台级操作
	if err := requireAdmin(ctx, "agentfactory.def.toggle"); err != nil {
		return nil, err
	}
	def, err := s.uc.ToggleDef(ctx, req.GetId(), req.GetEnabled())
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return toAgentDef(def), nil
}

// ———— 任务 ————

func (s *AgentFactoryService) SubmitTask(ctx context.Context, req *v1.SubmitTaskRequest) (*v1.AgentTask, error) {
	task, err := s.uc.SubmitTask(ctx, agentfactory.TaskInput{
		AgentDefID: req.GetAgentDefId(),
		AgentKey:   req.GetAgentKey(),
		Input:      req.GetInput(),
	})
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return toAgentTask(task), nil
}

func (s *AgentFactoryService) GetTaskStats(ctx context.Context, _ *v1.GetTaskStatsRequest) (*v1.TaskStats, error) {
	st, err := s.uc.Stats(ctx)
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return &v1.TaskStats{
		Tasks:     st.Tasks,
		Pending:   st.Pending,
		Running:   st.Running,
		Succeeded: st.Succeeded,
		Failed:    st.Failed,
		Canceled:  st.Canceled,
	}, nil
}

// ensureTaskOwned 任务归属校验：不属于当前用户的返回 403。
// 详情 / 取消 / 重试 / 事件流都走这里，避免知道任务 ID 就能看他人链路或终止他人任务。
func (s *AgentFactoryService) ensureTaskOwned(ctx context.Context, id uint64) (*agentfactory.Task, error) {
	t, err := s.uc.GetTask(ctx, id)
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	if err := ensureOwned(ctx, t.UserID, "任务"); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *AgentFactoryService) ListTasks(ctx context.Context, req *v1.ListTasksRequest) (*v1.ListTasksReply, error) {
	tasks, err := s.uc.ListTasks(ctx, agentfactory.TaskFilter{
		AgentDefID:   req.GetAgentDefId(),
		AgentKey:     req.GetAgentKey(),
		Status:       req.GetStatus(),
		Keyword:      req.GetKeyword(),
		ParentTaskID: req.GetParentTaskId(),
		RootOnly:     req.GetRootOnly(),
		ConvID:       req.GetConvId(),
		TenantID:     identity.TenantID(ctx), // 租户内可见（跨租户同 user_id 也不可见）
		UserID:       scopeUser(ctx),         // 任务中心只列当前用户的任务
		Limit:        int(req.GetLimit()),
	})
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	out := make([]*v1.AgentTask, 0, len(tasks))
	for i := range tasks {
		out = append(out, toAgentTask(&tasks[i]))
	}
	return &v1.ListTasksReply{Tasks: out, Total: int64(len(out))}, nil
}

func (s *AgentFactoryService) GetTask(ctx context.Context, req *v1.GetTaskRequest) (*v1.AgentTask, error) {
	t, err := s.ensureTaskOwned(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return toAgentTask(t), nil
}

func (s *AgentFactoryService) CancelTask(ctx context.Context, req *v1.CancelTaskRequest) (*v1.AgentTask, error) {
	if _, err := s.ensureTaskOwned(ctx, req.GetId()); err != nil {
		return nil, err
	}
	t, err := s.uc.CancelTask(ctx, req.GetId())
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return toAgentTask(t), nil
}

func (s *AgentFactoryService) RetryTask(ctx context.Context, req *v1.RetryTaskRequest) (*v1.AgentTask, error) {
	if _, err := s.ensureTaskOwned(ctx, req.GetId()); err != nil {
		return nil, err
	}
	t, err := s.uc.RetryTask(ctx, req.GetId())
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return toAgentTask(t), nil
}

func (s *AgentFactoryService) ApproveTask(ctx context.Context, req *v1.ApproveTaskRequest) (*v1.AgentTask, error) {
	// 审批是「独立审批人」的职责：管理员可跨用户审批；非管理员只能看到自己的任务，
	// 且高风险操作不能自批（四眼原则在 biz 层强制，这里只做可见范围）。
	if identity.AdminAllowed(ctx) {
		if _, err := s.uc.GetTask(ctx, req.GetId()); err != nil {
			return nil, mapAgentFactoryErr(err)
		}
	} else if _, err := s.ensureTaskOwned(ctx, req.GetId()); err != nil {
		return nil, err
	}
	decision := strings.ToLower(strings.TrimSpace(req.GetDecision()))
	if decision != "approve" && decision != "reject" {
		return nil, fmt.Errorf("decision 仅支持 approve / reject")
	}
	t, err := s.uc.ApproveTask(ctx, req.GetId(), decision, req.GetNote())
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	return toAgentTask(t), nil
}

func (s *AgentFactoryService) ListTaskEvents(ctx context.Context, req *v1.ListTaskEventsRequest) (*v1.ListTaskEventsReply, error) {
	if _, err := s.ensureTaskOwned(ctx, req.GetId()); err != nil {
		return nil, err
	}
	events, err := s.uc.TaskEvents(ctx, req.GetId(), int(req.GetLimit()))
	if err != nil {
		return nil, mapAgentFactoryErr(err)
	}
	out := make([]*v1.TaskEvent, 0, len(events))
	for _, e := range events {
		out = append(out, &v1.TaskEvent{
			Id:        e.ID,
			TaskId:    e.TaskID,
			Turn:      int32(e.Turn),
			Type:      e.Type,
			Payload:   e.Payload,
			CreatedAt: formatTS(e.CreatedAt),
		})
	}
	return &v1.ListTaskEventsReply{Events: out}, nil
}

// ———— DO ↔ 消息转换 ————

func toAgentDef(d *agentfactory.AgentDef) *v1.AgentDef {
	if d == nil {
		return nil
	}
	return &v1.AgentDef{
		Id:                   d.ID,
		AgentKey:             d.AgentKey,
		Name:                 d.Name,
		Description:          d.Description,
		System:               d.System,
		LlmModel:             d.LLMModel,
		LlmSource:            d.LLMSource,
		GraphType:            d.GraphType,
		AutoCompose:          d.AutoCompose,
		RequireApproval:      d.RequireApproval,
		ApprovalTimeoutHours: int32(d.ApprovalTimeoutHours),
		RiskPauseLevel:       d.RiskPauseLevel,
		Capabilities:         d.Capabilities,
		Middlewares:          d.Middlewares,
		MaxTurns:             int32(d.MaxTurns),
		Sandbox:              d.Sandbox,
		Source:               d.Source,
		Version:              int32(d.Version),
		Enabled:              d.Enabled,
		Vectorized:           d.Vectorized,
		CreatedAt:            formatTS(d.CreatedAt),
		UpdatedAt:            formatTS(d.UpdatedAt),
		Role:                 d.Role,
		Orchestration:        d.Orchestration,
		Members:              d.Members,
		SupervisorKey:        d.SupervisorKey,
		SupervisionScope:     d.SupervisionScope,
		RiskAgentKey:         d.RiskAgentKey,
		AutoRun:              d.AutoRun,
		CronExpr:             d.CronExpr,
		CronInput:            d.CronInput,
		NextRunAt:            formatTS(d.NextRunAt),
		LastRunAt:            formatTS(d.LastRunAt),
		Flow:                 d.Flow,
	}
}

func agentInputFromProto(in *v1.AgentInput) agentfactory.AgentInput {
	if in == nil {
		return agentfactory.AgentInput{}
	}
	return agentfactory.AgentInput{
		AgentKey:             in.GetAgentKey(),
		Name:                 in.GetName(),
		Description:          in.GetDescription(),
		System:               in.GetSystem(),
		LLMModel:             in.GetLlmModel(),
		LLMSource:            in.GetLlmSource(),
		GraphType:            in.GetGraphType(),
		AutoCompose:          in.GetAutoCompose(),
		RequireApproval:      in.GetRequireApproval(),
		ApprovalTimeoutHours: int(in.GetApprovalTimeoutHours()),
		RiskPauseLevel:       in.GetRiskPauseLevel(),
		Capabilities:         in.GetCapabilities(),
		Middlewares:          in.GetMiddlewares(),
		MaxTurns:             int(in.GetMaxTurns()),
		Sandbox:              in.GetSandbox(),
		Role:                 in.GetRole(),
		Orchestration:        in.GetOrchestration(),
		Members:              in.GetMembers(),
		SupervisorKey:        in.GetSupervisorKey(),
		SupervisionScope:     in.GetSupervisionScope(),
		RiskAgentKey:         in.GetRiskAgentKey(),
		AutoRun:              in.GetAutoRun(),
		CronExpr:             in.GetCronExpr(),
		CronInput:            in.GetCronInput(),
		Flow:                 in.GetFlow(),
	}
}

func toAgentTask(t *agentfactory.Task) *v1.AgentTask {
	if t == nil {
		return nil
	}
	return &v1.AgentTask{
		Id:           t.ID,
		AgentDefId:   t.AgentDefID,
		AgentKey:     t.AgentKey,
		Title:        t.Title,
		Input:        t.Input,
		Status:       t.Status,
		Progress:     int32(t.Progress),
		Result:       t.Result,
		ErrorMsg:     t.ErrorMsg,
		ParentTaskId: t.ParentTaskID,
		Depth:        int32(t.Depth),
		CreatedAt:    formatTS(t.CreatedAt),
		StartedAt:    formatTS(t.StartedAt),
		FinishedAt:   formatTS(t.FinishedAt),
		Archived:     t.Archived,
		ConvId:       t.ConvID,
		UserId:       t.UserID,
	}
}
