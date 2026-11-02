// cache_answer.go 用 Redis 承担 RAG 两件事：
//  1. 问答结果缓存（命中即跳过向量检索与 LLM 调用）
//  2. 入库去重锁（同一文档并发导入时只放行一个）
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go_agent/internal/biz/rag"
	"go_agent/internal/data/redis/models"

	goredis "github.com/redis/go-redis/v9"
)

// AnswerCache 实现 rag.AnswerCache 与 rag.Locker
//
// 键的构造（问题 + 过滤条件哈希）在 biz 侧，本实现只负责存取，
// 批量失效用 models.AnswerScanPattern() 生成 SCAN 模式。
type AnswerCache struct {
	rdb *goredis.Client
}

var (
	_ rag.AnswerCache = (*AnswerCache)(nil)
	_ rag.Locker      = (*AnswerCache)(nil)
)

// NewAnswerCache 创建 RAG 问答缓存与分布式锁实现
func NewAnswerCache(d *RDBData) *AnswerCache {
	return &AnswerCache{rdb: d.Client()}
}

// GetAnswer 读取问答缓存
func (c *AnswerCache) GetAnswer(ctx context.Context, key string) (*rag.Answer, bool) {
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	var ans rag.Answer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return nil, false
	}
	return &ans, true
}

// PutAnswer 写入问答缓存（ttl<=0 时用 5 分钟兜底）
func (c *AnswerCache) PutAnswer(ctx context.Context, key string, ans *rag.Answer, ttl time.Duration) {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	raw, err := json.Marshal(ans)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, key, raw, ttl).Err()
}

// Invalidate 清理全部问答缓存（文档增删后调用，用 SCAN 避免 KEYS 阻塞）
func (c *AnswerCache) Invalidate(ctx context.Context) error {
	var cursor uint64
	prefix := models.AnswerScanPattern()
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, prefix, 200).Result()
		if err != nil {
			return fmt.Errorf("scan 问答缓存: %w", err)
		}
		if len(keys) > 0 {
			if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("删除问答缓存: %w", err)
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// TryLock 尝试获取锁（SET NX EX）
func (c *AnswerCache) TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	ok, err := c.rdb.SetNX(ctx, key, time.Now().UnixMilli(), ttl).Result()
	if err != nil {
		// Redis 不可用时降级：不阻塞入库，只记录错误
		return true, fmt.Errorf("redis 不可用，跳过分布式锁: %w", err)
	}
	return ok, nil
}

// Unlock 释放锁
func (c *AnswerCache) Unlock(ctx context.Context, key string) error {
	err := c.rdb.Del(ctx, key).Err()
	if errors.Is(err, goredis.Nil) {
		return nil
	}
	return err
}
