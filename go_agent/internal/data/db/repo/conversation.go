// conversation.go 会话域 MySQL 仓储：conversations / conversation_messages 两表。
// 目录与消息读写，删除会话时级联删除消息。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/conversation"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// ConversationRepo 会话仓库
type ConversationRepo struct {
	db *gorm.DB
}

var _ conversation.Repo = (*ConversationRepo)(nil)

// NewConversationRepo 创建仓库；auto_migrate 打开时自动建表
func NewConversationRepo(c *conf.Data, d *db.Data) (*ConversationRepo, error) {
	r := &ConversationRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.ConversationPO{}, &models.ConversationMessagePO{}, &models.DebatePO{}); err != nil {
			return nil, errors.Join(errors.New("迁移会话表"), err)
		}
	}
	return r, nil
}

func toConversationPO(c *conversation.Conversation) *models.ConversationPO {
	return &models.ConversationPO{
		ID:           c.ID,
		Title:        c.Title,
		AgentKey:     c.AgentKey,
		MessageCount: c.MessageCount,
		LastMessage:  clip(c.LastMessage, 255),
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

func fromConversationPO(p *models.ConversationPO) conversation.Conversation {
	return conversation.Conversation{
		ID:           p.ID,
		Title:        p.Title,
		AgentKey:     p.AgentKey,
		MessageCount: p.MessageCount,
		LastMessage:  p.LastMessage,
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
	}
}

func (r *ConversationRepo) Create(ctx context.Context, c *conversation.Conversation) error {
	return r.db.WithContext(ctx).Create(toConversationPO(c)).Error
}

func (r *ConversationRepo) List(ctx context.Context, f conversation.Filter) ([]conversation.Conversation, int64, error) {
	q := r.db.WithContext(ctx).Model(&models.ConversationPO{})
	if kw := strings.TrimSpace(f.Kw); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("title LIKE ? OR last_message LIKE ?", like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.ConversationPO
	if err := q.Order("updated_at DESC").Limit(f.Limit).Offset(f.Offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]conversation.Conversation, 0, len(rows))
	for i := range rows {
		out = append(out, fromConversationPO(&rows[i]))
	}
	// 批量标记含辩论的会话（目录「辩论历史」分组用）
	if len(out) > 0 {
		ids := make([]string, 0, len(out))
		for i := range out {
			ids = append(ids, out[i].ID)
		}
		var debated []string
		if res := r.db.WithContext(ctx).Model(&models.DebatePO{}).
			Where("conversation_id IN ?", ids).Distinct().Pluck("conversation_id", &debated); res.Error == nil {
			mark := make(map[string]struct{}, len(debated))
			for _, id := range debated {
				mark[id] = struct{}{}
			}
			for i := range out {
				if _, ok := mark[out[i].ID]; ok {
					out[i].HasDebate = true
				}
			}
		}
	}
	return out, total, nil
}

func (r *ConversationRepo) Get(ctx context.Context, id string) (*conversation.Conversation, error) {
	var p models.ConversationPO
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := fromConversationPO(&p)
	return &c, nil
}

func (r *ConversationRepo) Update(ctx context.Context, c *conversation.Conversation) error {
	p := toConversationPO(c)
	p.UpdatedAt = time.Now()
	return r.db.WithContext(ctx).Model(&models.ConversationPO{}).Where("id = ?", c.ID).
		Updates(map[string]any{
			"title":      p.Title,
			"agent_key":  p.AgentKey,
			"updated_at": p.UpdatedAt,
		}).Error
}

func (r *ConversationRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("conversation_id = ?", id).Delete(&models.ConversationMessagePO{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", id).Delete(&models.ConversationPO{}).Error
	})
}

func (r *ConversationRepo) AppendMessage(ctx context.Context, m *conversation.Message) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p := models.ConversationMessagePO{
			ConversationID: m.ConversationID,
			Role:           m.Role,
			Speaker:        m.Speaker,
			Content:        m.Content,
			Meta:           m.Meta,
			Attachments:    marshalAttachments(m.Attachments),
			TaskID:         m.TaskID,
			CreatedAt:      m.CreatedAt,
		}
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		m.ID = p.ID
		// 刷新会话目录：计数 +1、最近消息预览、更新时间（纯附件消息预览为「[附件]」）
		preview := clip(m.Content, 255)
		if strings.TrimSpace(preview) == "" && len(m.Attachments) > 0 {
			preview = "[附件]"
		}
		return tx.Model(&models.ConversationPO{}).Where("id = ?", m.ConversationID).
			Updates(map[string]any{
				"message_count": gorm.Expr("message_count + 1"),
				"last_message":  preview,
				"updated_at":    m.CreatedAt,
			}).Error
	})
}

func (r *ConversationRepo) ListMessages(ctx context.Context, conversationID string) ([]conversation.Message, error) {
	var rows []models.ConversationMessagePO
	if err := r.db.WithContext(ctx).Where("conversation_id = ?", conversationID).
		Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]conversation.Message, 0, len(rows))
	for _, p := range rows {
		out = append(out, conversation.Message{
			ID:             p.ID,
			ConversationID: p.ConversationID,
			Role:           p.Role,
			Speaker:        p.Speaker,
			Content:        p.Content,
			Meta:           p.Meta,
			Attachments:    unmarshalAttachments(p.Attachments),
			TaskID:         p.TaskID,
			CreatedAt:      p.CreatedAt,
		})
	}
	return out, nil
}

// ListMessagesPaged 会话消息分页（微信式向上翻页）：
// 返回 id 正序的一页消息；beforeID > 0 时取 id < beforeID 的前一页；
// hasMore 表示是否还有更早的消息。
func (r *ConversationRepo) ListMessagesPaged(ctx context.Context, conversationID string, limit int, beforeID int64) ([]conversation.Message, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.WithContext(ctx).Where("conversation_id = ?", conversationID)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	var rows []models.ConversationMessagePO
	// 多取一条用于判断是否还有更早消息
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	// DESC 取出后反转为 id 正序
	out := make([]conversation.Message, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		p := rows[i]
		out = append(out, conversation.Message{
			ID:             p.ID,
			ConversationID: p.ConversationID,
			Role:           p.Role,
			Speaker:        p.Speaker,
			Content:        p.Content,
			Meta:           p.Meta,
			Attachments:    unmarshalAttachments(p.Attachments),
			TaskID:         p.TaskID,
			CreatedAt:      p.CreatedAt,
		})
	}
	return out, hasMore, nil
}

// —— 辩论 ——

func toDebatePO(d *conversation.Debate) (*models.DebatePO, error) {
	roles, err := json.Marshal(d.Roles)
	if err != nil {
		return nil, err
	}
	return &models.DebatePO{
		ID:             d.ID,
		ConversationID: d.ConversationID,
		Topic:          d.Topic,
		Status:         d.Status,
		Round:          d.Round,
		MaxRounds:      d.MaxRounds,
		EndRule:        d.EndRule,
		Roles:          string(roles),
		CreatedAt:      d.CreatedAt,
		UpdatedAt:      d.UpdatedAt,
		FinishedAt:     d.FinishedAt,
	}, nil
}

func fromDebatePO(p *models.DebatePO) (*conversation.Debate, error) {
	var roles []conversation.DebateRole
	if err := json.Unmarshal([]byte(p.Roles), &roles); err != nil {
		return nil, fmt.Errorf("解析辩论角色 JSON: %w", err)
	}
	if roles == nil {
		roles = []conversation.DebateRole{}
	}
	return &conversation.Debate{
		ID:             p.ID,
		ConversationID: p.ConversationID,
		Topic:          p.Topic,
		Status:         p.Status,
		Round:          p.Round,
		MaxRounds:      p.MaxRounds,
		EndRule:        p.EndRule,
		Roles:          roles,
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
		FinishedAt:     p.FinishedAt,
	}, nil
}

func (r *ConversationRepo) CreateDebate(ctx context.Context, d *conversation.Debate) error {
	p, err := toDebatePO(d)
	if err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).Create(p).Error; err != nil {
		return err
	}
	// 回填自增 ID 与时间戳：上层直接用同一对象启动推演 goroutine 并做后续更新
	d.ID = p.ID
	d.CreatedAt = p.CreatedAt
	d.UpdatedAt = p.UpdatedAt
	return nil
}

func (r *ConversationRepo) GetDebate(ctx context.Context, id int64) (*conversation.Debate, error) {
	var p models.DebatePO
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&p).Error; err != nil {
		return nil, err
	}
	return fromDebatePO(&p)
}

// GetActiveDebate 会话内进行中（running/stopping）的辩论，没有则返回 nil
func (r *ConversationRepo) GetActiveDebate(ctx context.Context, conversationID string) (*conversation.Debate, error) {
	var p models.DebatePO
	err := r.db.WithContext(ctx).
		Where("conversation_id = ? AND status IN ?", conversationID, []string{conversation.DebateRunning, conversation.DebatePaused, conversation.DebateStopping}).
		Order("id DESC").First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fromDebatePO(&p)
}

// GetLatestDebate 会话最近一场辩论（含已结束），没有则返回 nil
func (r *ConversationRepo) GetLatestDebate(ctx context.Context, conversationID string) (*conversation.Debate, error) {
	var p models.DebatePO
	err := r.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		Order("id DESC").First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fromDebatePO(&p)
}

// UpdateDebate 更新辩论的可变字段（status/round/roles/finished_at）
func (r *ConversationRepo) UpdateDebate(ctx context.Context, d *conversation.Debate) error {
	p, err := toDebatePO(d)
	if err != nil {
		return err
	}
	p.UpdatedAt = time.Now()
	updates := map[string]any{
		"status":     p.Status,
		"round":      p.Round,
		"roles":      p.Roles,
		"updated_at": p.UpdatedAt,
	}
	if p.FinishedAt != nil {
		updates["finished_at"] = *p.FinishedAt
	}
	return r.db.WithContext(ctx).Model(&models.DebatePO{}).Where("id = ?", p.ID).Updates(updates).Error
}

// StopOrphanDebates 启动恢复：把上一进程遗留的活跃辩论统一收尾为已叫停
func (r *ConversationRepo) StopOrphanDebates(ctx context.Context) (int64, error) {
	res := r.db.WithContext(ctx).Model(&models.DebatePO{}).
		Where("status IN ?", []string{conversation.DebateRunning, conversation.DebatePaused, conversation.DebateStopping}).
		Updates(map[string]any{"status": conversation.DebateStopped, "updated_at": time.Now()})
	return res.RowsAffected, res.Error
}

// marshalAttachments 附件列表 → JSON 列（空列表存空串）
func marshalAttachments(atts []conversation.Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	b, err := json.Marshal(atts)
	if err != nil {
		return ""
	}
	return string(b)
}

// unmarshalAttachments JSON 列 → 附件列表（空串/坏数据 = nil）
func unmarshalAttachments(s string) []conversation.Attachment {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var atts []conversation.Attachment
	if err := json.Unmarshal([]byte(s), &atts); err != nil {
		return nil
	}
	return atts
}
