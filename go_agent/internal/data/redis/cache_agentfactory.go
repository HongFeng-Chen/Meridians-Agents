package redis

// agentfactory Redis 缓存：定义热缓存 + 任务实时快照 + 任务队列。
// 缓存失败一律降级（返回 miss / 忽略写失败），不阻塞主流程。

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/data/redis/models"
)

var _ agentfactory.Cache = (*AgentFactoryCache)(nil)

type AgentFactoryCache struct {
	rdb *goredis.Client
}

func NewAgentFactoryCache(d *RDBData) *AgentFactoryCache {
	return &AgentFactoryCache{rdb: d.Client()}
}

// ———— Agent 定义缓存 ————

func (c *AgentFactoryCache) GetDef(ctx context.Context, id uint64) (*agentfactory.AgentDef, bool) {
	raw, err := c.rdb.Get(ctx, models.AgentKey(id)).Result()
	if err != nil || raw == "" {
		return nil, false
	}
	var def agentfactory.AgentDef
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		return nil, false
	}
	return &def, true
}

func (c *AgentFactoryCache) SetDef(ctx context.Context, def *agentfactory.AgentDef) {
	raw, err := json.Marshal(def)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, models.AgentKey(def.ID), raw, models.AgentTTL*time.Second).Err()
}

func (c *AgentFactoryCache) DeleteDefs(ctx context.Context, ids []uint64) {
	if len(ids) == 0 {
		return
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, models.AgentKey(id))
	}
	_ = c.rdb.Del(ctx, keys...).Err()
}

// ———— 任务实时快照 ————

func (c *AgentFactoryCache) GetTask(ctx context.Context, id uint64) (*agentfactory.Task, bool) {
	raw, err := c.rdb.Get(ctx, models.TaskKey(id)).Result()
	if err != nil || raw == "" {
		return nil, false
	}
	var t agentfactory.Task
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, false
	}
	return &t, true
}

// SetTask 刷新任务快照；终态任务立即删除（终态读库，快照只服务运行中轮询）
func (c *AgentFactoryCache) SetTask(ctx context.Context, t *agentfactory.Task) {
	if t == nil {
		return
	}
	if t.Status == agentfactory.TaskSucceeded || t.Status == agentfactory.TaskFailed || t.Status == agentfactory.TaskCanceled {
		_ = c.rdb.Del(ctx, models.TaskKey(t.ID)).Err()
		return
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, models.TaskKey(t.ID), raw, models.ActiveTaskTTL*time.Second).Err()
}

// ———— 任务队列 ————

func (c *AgentFactoryCache) EnqueueTask(ctx context.Context, id uint64) error {
	return c.rdb.LPush(ctx, models.TaskQueueKey, strconv.FormatUint(id, 10)).Err()
}

// DequeueTask BRPOP 出队；timeout ≤ 0 时立即返回
func (c *AgentFactoryCache) DequeueTask(ctx context.Context, timeout time.Duration) (uint64, bool) {
	res, err := c.rdb.BRPop(ctx, timeout, models.TaskQueueKey).Result()
	if err != nil {
		// Nil = 超时无任务；其他错误降级为 miss，worker 下一轮继续
		return 0, false
	}
	// BRPop 返回 [key, value]
	if len(res) < 2 {
		return 0, false
	}
	id, err := strconv.ParseUint(res[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
