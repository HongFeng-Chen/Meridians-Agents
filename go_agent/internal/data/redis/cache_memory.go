// cache_memory.go 用 Redis 存储短期会话记忆：
// 每个会话一个 List（mem:short:{session_id}），消息按时间追加，
// TTL 随每次写入自动续期——会话静默即自动过期，符合"短期"语义。
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go_agent/internal/biz/memory"
	"go_agent/internal/data/redis/models"

	goredis "github.com/redis/go-redis/v9"
)

// maxHistory 单会话最多保留的消息数，保留策略统一定义在 models 里
const maxHistory = models.MaxSessionMessages

// ShortTermStore 短期记忆存储，实现 memory.ShortTermStore
type ShortTermStore struct {
	rdb *goredis.Client
}

var _ memory.ShortTermStore = (*ShortTermStore)(nil)

// NewShortTermStore 创建短期记忆存储
func NewShortTermStore(d *RDBData) *ShortTermStore {
	return &ShortTermStore{rdb: d.Client()}
}

// Append 追加消息并续期；超出 maxHistory 时裁剪最旧消息
func (s *ShortTermStore) Append(ctx context.Context, sessionID string, msg memory.Message, ttl time.Duration) error {
	if sessionID == "" {
		return fmt.Errorf("短期记忆: session_id 为空")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("短期记忆: 序列化消息: %w", err)
	}
	key := models.ShortSessionKey(sessionID)
	pipe := s.rdb.TxPipeline()
	pipe.RPush(ctx, key, raw)
	pipe.LTrim(ctx, key, -maxHistory, -1)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("短期记忆: 追加消息: %w", err)
	}
	return nil
}

// History 最近 limit 条消息（按时间正序返回）
func (s *ShortTermStore) History(ctx context.Context, sessionID string, limit int) ([]memory.Message, error) {
	if sessionID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	key := models.ShortSessionKey(sessionID)
	n, err := s.rdb.LLen(ctx, key).Result()
	if err != nil {
		return nil, nil // 会话不存在或 Redis 异常：按空历史处理，不阻断问答
	}
	start := n - int64(limit)
	if start < 0 {
		start = 0
	}
	raws, err := s.rdb.LRange(ctx, key, start, -1).Result()
	if err != nil {
		return nil, nil
	}
	out := make([]memory.Message, 0, len(raws))
	for _, raw := range raws {
		var m memory.Message
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// Len 会话消息数
func (s *ShortTermStore) Len(ctx context.Context, sessionID string) (int64, error) {
	n, err := s.rdb.LLen(ctx, models.ShortSessionKey(sessionID)).Result()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// Clear 清空会话
func (s *ShortTermStore) Clear(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	if err := s.rdb.Del(ctx, models.ShortSessionKey(sessionID)).Err(); err != nil {
		return fmt.Errorf("短期记忆: 清空会话: %w", err)
	}
	return nil
}

// TTL 查询会话剩余存活时间（管理端展示用；-2 表示不存在）
func (s *ShortTermStore) TTL(ctx context.Context, sessionID string) (time.Duration, error) {
	return s.rdb.TTL(ctx, models.ShortSessionKey(sessionID)).Result()
}

// ScanSessions 枚举会话键（管理端统计用，SCAN 避免 KEYS 阻塞）
func (s *ShortTermStore) ScanSessions(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []string
	var cursor uint64
	for len(out) < limit {
		keys, next, err := s.rdb.Scan(ctx, cursor, models.ShortSessionScanPattern(), 100).Result()
		if err != nil {
			return out, err
		}
		out = append(out, keys...)
		if next == 0 || len(out) >= limit {
			break
		}
		cursor = next
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
