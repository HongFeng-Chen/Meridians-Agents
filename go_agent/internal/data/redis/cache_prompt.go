// cache_prompt.go 缓存提示词的「当前生效版本」。
//
// 缓存的意义：RAG 每次问答都要读系统提示词，回源 MySQL 会给数据库带来
// 与问题量成正比的读压力。发布/激活/删除时由用例层显式失效，所以
// 缓存 TTL 只是兜底（防止异常写入导致的长期脏读），不需要太长。
package redis

import (
	"context"
	"encoding/json"
	"time"

	"go_agent/internal/biz/prompt"
	"go_agent/internal/data/redis/models"

	goredis "github.com/redis/go-redis/v9"
)

// PromptCache 实现 prompt.Cache；键格式见 models.PromptVersionKey
type PromptCache struct {
	rdb *goredis.Client
}

var _ prompt.Cache = (*PromptCache)(nil)

// NewPromptCache 创建提示词生效版本缓存
func NewPromptCache(d *RDBData) *PromptCache {
	return &PromptCache{rdb: d.Client()}
}

// GetVersion 读取生效版本
func (c *PromptCache) GetVersion(ctx context.Context, key string) (*prompt.Version, bool) {
	raw, err := c.rdb.Get(ctx, models.PromptVersionKey(key)).Bytes()
	if err != nil {
		return nil, false
	}
	var v prompt.Version
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	v.Active = true
	return &v, true
}

// PutVersion 写入生效版本（ttl<=0 不缓存）
func (c *PromptCache) PutVersion(ctx context.Context, key string, v *prompt.Version, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, models.PromptVersionKey(key), raw, ttl).Err()
}

// Invalidate 删除指定模板缓存
func (c *PromptCache) Invalidate(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, models.PromptVersionKey(key)).Err()
}
