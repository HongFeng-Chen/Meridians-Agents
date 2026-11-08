package repo

// MemoryConvSource：把会话消息表适配为记忆模块的回源来源。
// 用途：短期记忆（Redis）为空时，从 MySQL 会话消息回源并反写 Redis，
// 让记忆控制台的「会话历史」能直接查看会话聊天的内容。

import (
	"context"

	"go_agent/internal/biz/memory"
)

type MemoryConvSource struct {
	conv *ConversationRepo
}

func NewMemoryConvSource(conv *ConversationRepo) *MemoryConvSource {
	return &MemoryConvSource{conv: conv}
}

// ListRecent 取会话最近 limit 条消息（时间正序），投影为记忆模块的最小结构。
//
// userID 非空时校验会话归属：不匹配直接返回空，而不是「先取再筛」——
// 后者一旦有人把校验删掉或改坏，泄漏是静默的。这里的语义是
// 「不属于你的会话，等于不存在」。
func (s *MemoryConvSource) ListRecent(ctx context.Context, conversationID, userID string, limit int) ([]memory.ShortMessage, error) {
	if s == nil || s.conv == nil || conversationID == "" {
		return nil, nil
	}
	if userID != "" {
		c, err := s.conv.Get(ctx, conversationID)
		if err != nil || c == nil {
			return nil, nil // 会话不存在：按空历史处理
		}
		if c.UserID != "" && c.UserID != userID {
			return nil, nil
		}
	}
	msgs, err := s.conv.ListMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	out := make([]memory.ShortMessage, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		// 辩论消息带 speaker：以「角色名」作为身份更可读
		if m.Speaker != "" {
			role = m.Speaker
		}
		out = append(out, memory.ShortMessage{Role: role, Content: m.Content, At: m.CreatedAt})
	}
	return out, nil
}
