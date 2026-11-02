package models

import (
	"strconv"
	"time"
)

// —— MCP 域键空间 ——
//
//	mcp:tool:{toolID}        Hash/JSON：工具完整对象（热点缓存）
//	mcp:status:{serverID}    运行状态（内存态，落 Redis 便于多实例共享）
//	ns:tools:{namespace}     Set：命名空间下的 toolID，支持按空间取工具
const (
	ToolPrefix           = "mcp:tool:"
	StatusPrefix         = "mcp:status:"
	NamespaceToolsPrefix = "ns:tools:"
)

// ToolCacheTTL 工具缓存过期时间。
//
// 只是兜底：同步/启停/删除时都会显式失效；TTL 用于防止漏删导致的长期脏读。
const ToolCacheTTL = 30 * time.Minute

// ToolKey 工具缓存键
func ToolKey(id uint64) string { return ToolPrefix + strconv.FormatUint(id, 10) }

// ToolScanPattern 清理全部工具缓存时的 SCAN 模式
func ToolScanPattern() string { return scanPattern(ToolPrefix) }

// StatusKey 运行状态键
func StatusKey(serverID uint64) string { return StatusPrefix + strconv.FormatUint(serverID, 10) }

// NamespaceToolsKey 命名空间工具索引键
func NamespaceToolsKey(namespace string) string { return NamespaceToolsPrefix + namespace }
