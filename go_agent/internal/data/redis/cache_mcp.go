// cache_mcp.go MCP 域缓存：
//
//   - 工具完整对象（mcp:tool:{id}）：召回命中后用 id 批量取详情，避免打 MySQL；
//   - 运行状态（mcp:status:{id}）：连接/健康检查结果，重启后可恢复展示；
//   - 命名空间索引（ns:tools:{ns}）：按命名空间取工具 id 集合。
//
// 缓存是「读加速」而非数据源：未命中一律回 MySQL，写入失败只降级不报错。
package redis

import (
	"context"
	"encoding/json"
	"strconv"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/data/redis/models"

	goredis "github.com/redis/go-redis/v9"
)

// MCPCache 实现 mcp.Cache
type MCPCache struct {
	rdb *goredis.Client
}

var _ mcp.Cache = (*MCPCache)(nil)

// NewMCPCache 创建 MCP 缓存
func NewMCPCache(d *RDBData) *MCPCache { return &MCPCache{rdb: d.Client()} }

// GetTools 批量读工具；未命中的 id 不出现在返回 map 中
func (c *MCPCache) GetTools(ctx context.Context, ids []uint64) (map[uint64]mcp.Tool, error) {
	out := make(map[uint64]mcp.Tool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, models.ToolKey(id))
	}
	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		// 缓存不可用不影响主流程：调用方会回源 MySQL
		return out, nil
	}
	for _, v := range vals {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		var t mcp.Tool
		if err := json.Unmarshal([]byte(s), &t); err != nil || t.ID == 0 {
			continue
		}
		out[t.ID] = t
	}
	return out, nil
}

// PutTools 批量写工具（pipeline，一次往返）
func (c *MCPCache) PutTools(ctx context.Context, tools []mcp.Tool) error {
	if len(tools) == 0 {
		return nil
	}
	pipe := c.rdb.Pipeline()
	for i := range tools {
		raw, err := json.Marshal(tools[i])
		if err != nil {
			continue
		}
		pipe.Set(ctx, models.ToolKey(tools[i].ID), raw, models.ToolCacheTTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// DeleteTools 删除工具缓存
func (c *MCPCache) DeleteTools(ctx context.Context, ids []uint64) {
	if len(ids) == 0 {
		return
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, models.ToolKey(id))
	}
	_ = c.rdb.Del(ctx, keys...).Err()
}

// GetStatus 读运行状态
func (c *MCPCache) GetStatus(ctx context.Context, serverID uint64) (mcp.Status, bool) {
	raw, err := c.rdb.Get(ctx, models.StatusKey(serverID)).Bytes()
	if err != nil {
		return mcp.Status{}, false
	}
	var st mcp.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return mcp.Status{}, false
	}
	return st, true
}

// GetStatuses 批量读运行状态
func (c *MCPCache) GetStatuses(ctx context.Context, serverIDs []uint64) map[uint64]mcp.Status {
	out := make(map[uint64]mcp.Status, len(serverIDs))
	if len(serverIDs) == 0 {
		return out
	}
	keys := make([]string, 0, len(serverIDs))
	for _, id := range serverIDs {
		keys = append(keys, models.StatusKey(id))
	}
	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return out
	}
	for i, v := range vals {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		var st mcp.Status
		if err := json.Unmarshal([]byte(s), &st); err != nil {
			continue
		}
		out[serverIDs[i]] = st
	}
	return out
}

// PutStatus 写运行状态（不带 TTL：状态需要跨重启保留，删除服务时显式清理）
func (c *MCPCache) PutStatus(ctx context.Context, st mcp.Status) {
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, models.StatusKey(st.ServerID), raw, 0).Err()
}

// DeleteStatus 删除运行状态
func (c *MCPCache) DeleteStatus(ctx context.Context, serverID uint64) {
	_ = c.rdb.Del(ctx, models.StatusKey(serverID)).Err()
}

// AddToNamespace 维护命名空间索引
func (c *MCPCache) AddToNamespace(ctx context.Context, namespace string, toolIDs []uint64) {
	if len(toolIDs) == 0 {
		return
	}
	members := make([]any, 0, len(toolIDs))
	for _, id := range toolIDs {
		members = append(members, strconv.FormatUint(id, 10))
	}
	_ = c.rdb.SAdd(ctx, models.NamespaceToolsKey(namespace), members...).Err()
}

// RemoveFromNamespace 从命名空间索引移除
func (c *MCPCache) RemoveFromNamespace(ctx context.Context, namespace string, toolIDs []uint64) {
	if len(toolIDs) == 0 {
		return
	}
	members := make([]any, 0, len(toolIDs))
	for _, id := range toolIDs {
		members = append(members, strconv.FormatUint(id, 10))
	}
	_ = c.rdb.SRem(ctx, models.NamespaceToolsKey(namespace), members...).Err()
}

// NamespaceTools 取命名空间下的工具 id（SCAN 式遍历，单次上限 1000）
func (c *MCPCache) NamespaceTools(ctx context.Context, namespace string) []uint64 {
	key := models.NamespaceToolsKey(namespace)
	out := make([]uint64, 0, 32)
	var cursor uint64
	for {
		members, next, err := c.rdb.SScan(ctx, key, cursor, "", 200).Result()
		if err != nil {
			return out
		}
		for _, m := range members {
			if id, err := strconv.ParseUint(m, 10, 64); err == nil {
				out = append(out, id)
			}
		}
		if next == 0 || len(out) >= 1000 {
			return out
		}
		cursor = next
	}
}
