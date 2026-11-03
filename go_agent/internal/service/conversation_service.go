package service

// 会话服务：实现 api/conversation/v1 的 ConversationService。
// 会话 id 优先复用任务 ID（会话 id = task id）；消息 meta 记录能力调用链路。

import (
	"bytes"
	"context"

	v1 "go_agent/api/conversation/v1"
	"go_agent/internal/biz/conversation"
	"go_agent/internal/pkg/httpx"
)

// ConversationService 会话服务。
type ConversationService struct {
	v1.UnimplementedConversationServiceServer

	uc *conversation.UseCase
}

var (
	_ v1.ConversationServiceHTTPServer = (*ConversationService)(nil)
	_ v1.ConversationServiceServer     = (*ConversationService)(nil)
)

// NewConversationService 创建会话服务
func NewConversationService(uc *conversation.UseCase) *ConversationService {
	return &ConversationService{uc: uc}
}

func toConversation(c conversation.Conversation) *v1.Conversation {
	return &v1.Conversation{
		Id:           c.ID,
		Title:        c.Title,
		AgentKey:     c.AgentKey,
		MessageCount: c.MessageCount,
		LastMessage:  c.LastMessage,
		CreatedAt:    formatTS(c.CreatedAt),
		UpdatedAt:    formatTS(c.UpdatedAt),
		HasDebate:    c.HasDebate,
	}
}

func toMessage(m conversation.Message) *v1.ConversationMessage {
	out := &v1.ConversationMessage{
		Id:             m.ID,
		ConversationId: m.ConversationID,
		Role:           m.Role,
		Content:        m.Content,
		Meta:           m.Meta,
		TaskId:         m.TaskID,
		CreatedAt:      formatTS(m.CreatedAt),
		Speaker:        m.Speaker,
	}
	for _, a := range m.Attachments {
		out.Attachments = append(out.Attachments, toAttachment(a))
	}
	return out
}

func toAttachment(a conversation.Attachment) *v1.Attachment {
	return &v1.Attachment{Name: a.Name, Url: a.URL, Mime: a.MIME, Size: a.Size}
}

func fromAttachment(a *v1.Attachment) conversation.Attachment {
	return conversation.Attachment{Name: a.GetName(), URL: a.GetUrl(), MIME: a.GetMime(), Size: a.GetSize()}
}

func fromAttachments(list []*v1.Attachment) []conversation.Attachment {
	if len(list) == 0 {
		return nil
	}
	out := make([]conversation.Attachment, 0, len(list))
	for _, a := range list {
		out = append(out, fromAttachment(a))
	}
	return out
}

// CreateConversation 新建会话。
func (s *ConversationService) CreateConversation(ctx context.Context, req *v1.CreateConversationRequest) (*v1.CreateConversationReply, error) {
	c, err := s.uc.Create(ctx, req.GetId(), req.GetTitle(), req.GetAgentKey())
	if err != nil {
		return nil, err
	}
	return &v1.CreateConversationReply{Conversation: toConversation(*c)}, nil
}

// ListConversations 会话目录。
func (s *ConversationService) ListConversations(ctx context.Context, req *v1.ListConversationsRequest) (*v1.ListConversationsReply, error) {
	list, total, err := s.uc.List(ctx, conversation.Filter{
		Kw:     req.GetKw(),
		Limit:  int(req.GetLimit()),
		Offset: int(req.GetOffset()),
	})
	if err != nil {
		return nil, err
	}
	out := &v1.ListConversationsReply{Conversations: make([]*v1.Conversation, 0, len(list)), Total: total}
	for _, c := range list {
		out.Conversations = append(out.Conversations, toConversation(c))
	}
	return out, nil
}

// GetConversation 会话详情 + 全部消息。
func (s *ConversationService) GetConversation(ctx context.Context, req *v1.GetConversationRequest) (*v1.GetConversationReply, error) {
	c, msgs, err := s.uc.Detail(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	out := &v1.GetConversationReply{Conversation: toConversation(*c), Messages: make([]*v1.ConversationMessage, 0, len(msgs))}
	for _, m := range msgs {
		out.Messages = append(out.Messages, toMessage(m))
	}
	return out, nil
}

// ListMessages 会话消息分页（微信式向上翻页：默认最新一页，before_id 向前翻）。
func (s *ConversationService) ListMessages(ctx context.Context, req *v1.ListMessagesRequest) (*v1.ListMessagesReply, error) {
	msgs, hasMore, err := s.uc.ListMessagesPage(ctx, req.GetId(), req.GetLimit(), req.GetBeforeId())
	if err != nil {
		return nil, err
	}
	out := &v1.ListMessagesReply{Messages: make([]*v1.ConversationMessage, 0, len(msgs)), HasMore: hasMore}
	for _, m := range msgs {
		out.Messages = append(out.Messages, toMessage(m))
	}
	return out, nil
}

// UpdateConversation 重命名会话。
func (s *ConversationService) UpdateConversation(ctx context.Context, req *v1.UpdateConversationRequest) (*v1.UpdateConversationReply, error) {
	c, err := s.uc.Rename(ctx, req.GetId(), req.GetTitle())
	if err != nil {
		return nil, err
	}
	return &v1.UpdateConversationReply{Conversation: toConversation(*c)}, nil
}

// DeleteConversation 删除会话。
func (s *ConversationService) DeleteConversation(ctx context.Context, req *v1.DeleteConversationRequest) (*v1.DeleteConversationReply, error) {
	if err := s.uc.Delete(ctx, req.GetId()); err != nil {
		return nil, err
	}
	return &v1.DeleteConversationReply{}, nil
}

// AppendMessage 追加消息。
func (s *ConversationService) AppendMessage(ctx context.Context, req *v1.AppendMessageRequest) (*v1.AppendMessageReply, error) {
	m, err := s.uc.Append(ctx, req.GetId(), req.GetRole(), req.GetContent(), req.GetMeta(), req.GetTaskId())
	if err != nil {
		return nil, err
	}
	return &v1.AppendMessageReply{Message: toMessage(*m)}, nil
}

// Chat 与 LLM 对话一轮（LLM 自主编排 Agent / MCP，自动召回 Memory / RAG / GraphRAG）。
// attachments 为已上传附件（先经 UploadMedia 拿到 url）。
func (s *ConversationService) Chat(ctx context.Context, req *v1.ChatRequest) (*v1.ChatReply, error) {
	userMsg, assistantMsg, err := s.uc.Chat(ctx, req.GetId(), req.GetContent(), fromAttachments(req.GetAttachments()))
	if err != nil {
		return nil, err
	}
	out := &v1.ChatReply{}
	if userMsg != nil {
		out.UserMessage = toMessage(*userMsg)
	}
	if assistantMsg != nil {
		out.AssistantMessage = toMessage(*assistantMsg)
	}
	return out, nil
}

// UploadMedia 上传聊天附件（图片 / 视频）。
// HTTP 走 multipart（文件名与内容来自解码器），gRPC 直接填 file/name。
func (s *ConversationService) UploadMedia(ctx context.Context, req *v1.UploadMediaRequest) (*v1.UploadMediaReply, error) {
	file, err := httpx.ResolveFile(ctx, req.GetFile(), req.GetName())
	if err != nil {
		return nil, err
	}
	att, err := s.uc.SaveMedia(file.Filename, bytes.NewReader(file.Data))
	if err != nil {
		return nil, badRequest(err.Error())
	}
	return &v1.UploadMediaReply{Attachment: toAttachment(*att)}, nil
}

// —— 辩论会话 ——

func toDebateRole(r *v1.DebateRole) conversation.DebateRole {
	return conversation.DebateRole{Name: r.GetName(), Persona: r.GetPersona(), Stance: r.GetStance(), Style: r.GetStyle()}
}

func toProtoDebate(d *conversation.Debate) *v1.Debate {
	if d == nil {
		return nil
	}
	roles := make([]*v1.DebateRole, 0, len(d.Roles))
	for _, r := range d.Roles {
		roles = append(roles, &v1.DebateRole{Name: r.Name, Persona: r.Persona, Stance: r.Stance, Style: r.Style})
	}
	out := &v1.Debate{
		Id:             d.ID,
		ConversationId: d.ConversationID,
		Topic:          d.Topic,
		Status:         d.Status,
		Round:          int32(d.Round),
		MaxRounds:      int32(d.MaxRounds),
		EndRule:        d.EndRule,
		Roles:          roles,
		CreatedAt:      formatTS(d.CreatedAt),
		UpdatedAt:      formatTS(d.UpdatedAt),
	}
	if d.FinishedAt != nil {
		out.FinishedAt = formatTS(*d.FinishedAt)
	}
	return out
}

// CreateDebate 创建辩论（后台推演启动）。
func (s *ConversationService) CreateDebate(ctx context.Context, req *v1.CreateDebateRequest) (*v1.CreateDebateReply, error) {
	roles := make([]conversation.DebateRole, 0, len(req.GetRoles()))
	for _, r := range req.GetRoles() {
		roles = append(roles, toDebateRole(r))
	}
	d, err := s.uc.CreateDebate(ctx, req.GetId(), req.GetTopic(), roles, int(req.GetMaxRounds()), req.GetEndRule())
	if err != nil {
		return nil, err
	}
	return &v1.CreateDebateReply{Debate: toProtoDebate(d)}, nil
}

// GetCurrentDebate 会话当前 / 最近一场辩论。
func (s *ConversationService) GetCurrentDebate(ctx context.Context, req *v1.GetConversationRequest) (*v1.GetDebateReply, error) {
	d, err := s.uc.CurrentDebate(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return &v1.GetDebateReply{Debate: toProtoDebate(d)}, nil
}

// StopDebate 请求停止辩论。
func (s *ConversationService) StopDebate(ctx context.Context, req *v1.GetConversationRequest) (*v1.StopDebateReply, error) {
	d, err := s.uc.StopDebate(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return &v1.StopDebateReply{Debate: toProtoDebate(d)}, nil
}

// PauseDebate 暂停辩论。
func (s *ConversationService) PauseDebate(ctx context.Context, req *v1.GetConversationRequest) (*v1.StopDebateReply, error) {
	d, err := s.uc.PauseDebate(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return &v1.StopDebateReply{Debate: toProtoDebate(d)}, nil
}

// ResumeDebate 恢复已暂停的辩论。
func (s *ConversationService) ResumeDebate(ctx context.Context, req *v1.GetConversationRequest) (*v1.StopDebateReply, error) {
	d, err := s.uc.ResumeDebate(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return &v1.StopDebateReply{Debate: toProtoDebate(d)}, nil
}

// AddDebateRole 辩论中追加角色。
func (s *ConversationService) AddDebateRole(ctx context.Context, req *v1.AddDebateRoleRequest) (*v1.AddDebateRoleReply, error) {
	d, err := s.uc.AddDebateRole(ctx, req.GetId(), toDebateRole(req.GetRole()))
	if err != nil {
		return nil, err
	}
	return &v1.AddDebateRoleReply{Debate: toProtoDebate(d)}, nil
}

// RemoveDebateRole 辩论中移除角色。
func (s *ConversationService) RemoveDebateRole(ctx context.Context, req *v1.RemoveDebateRoleRequest) (*v1.RemoveDebateRoleReply, error) {
	d, err := s.uc.RemoveDebateRole(ctx, req.GetId(), req.GetName())
	if err != nil {
		return nil, err
	}
	return &v1.RemoveDebateRoleReply{Debate: toProtoDebate(d)}, nil
}
