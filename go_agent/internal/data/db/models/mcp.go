package models

import (
	"time"

	"gorm.io/gorm"
)

// MCPServerPO MCP 服务配置表映射（mcp_servers）。
//
// 只存配置：http 模式的 endpoint 与鉴权 header 模板、stdio 模式的命令行与 env。
// http.Client 本身不落库——超时/代理/TLS/连接池属于运行时属性，
// 由 biz 层 MCPManager 按实例注入（见 internal/biz/mcp/manager.go）。
type MCPServerPO struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	Name        string `gorm:"column:name;size:128;not null;uniqueIndex:uk_mcp_servers_name" json:"name"`
	Namespace   string `gorm:"column:namespace;size:128;not null;default:'default';index:idx_mcp_servers_namespace" json:"namespace"`
	Transport   string `gorm:"column:transport;size:32;not null" json:"transport"` // stdio | http
	Enabled     bool   `gorm:"column:is_enabled;not null;default:true;index:idx_mcp_servers_enabled" json:"is_enabled"`
	Description string `gorm:"column:description;size:512;not null;default:''" json:"description"`

	// HTTP 模式（transport=http 生效）
	Endpoint string `gorm:"column:endpoint;size:512;not null;default:''" json:"endpoint"`
	Headers  string `gorm:"column:headers;type:json" json:"headers"` // map[string]string，支持 ${ENV} 占位

	// Stdio 模式（transport=stdio 生效）
	Command string `gorm:"column:command;size:256;not null;default:''" json:"command"`
	Args    string `gorm:"column:args;type:json" json:"args"` // []string
	Env     string `gorm:"column:env;type:json" json:"env"`   // map[string]string

	// 健康检查间隔（秒），<=0 表示不主动探活
	HealthCheckInterval int64 `gorm:"column:health_check_interval;not null;default:30" json:"health_check_interval"`

	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index:idx_mcp_servers_deleted" json:"-"`
}

// TableName 表名
func (MCPServerPO) TableName() string { return "mcp_servers" }

// ToolPO 工具表映射（tools）：MCP 服务 tools/list 的落地形态。
//
// 与 mcp_servers 一对多；UniqueName = namespace + "__" + tool_name，跨服务消歧。
// InputSchema/OutputSchema 存 JSON 原文（不解析成结构体：各家 schema 差异大，
// 原样透传给前端与调用方最安全）。
type ToolPO struct {
	ID           uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	MCPServerID  uint64 `gorm:"column:mcp_server_id;not null;index:idx_tools_server" json:"mcp_server_id"`
	ToolName     string `gorm:"column:tool_name;size:128;not null;index:idx_tools_tool_name" json:"tool_name"`
	UniqueName   string `gorm:"column:unique_name;size:256;not null;uniqueIndex:uk_tools_unique_name" json:"unique_name"`
	DisplayName  string `gorm:"column:display_name;size:191;not null;default:''" json:"display_name"`
	Description  string `gorm:"column:description;type:text" json:"description"`
	InputSchema  string `gorm:"column:input_schema;type:mediumtext" json:"input_schema"`
	OutputSchema string `gorm:"column:output_schema;type:mediumtext" json:"output_schema"`
	Examples     string `gorm:"column:examples;type:text" json:"examples"` // []string，参与 embedding
	Tags         string `gorm:"column:tags;type:text" json:"tags"`         // []string，参与 embedding
	Enabled      bool   `gorm:"column:is_enabled;not null;default:true;index:idx_tools_enabled" json:"is_enabled"`
	// Vectorized 向量是否已写入 Milvus（embedding 失败时为 false，可单独重试）
	Vectorized bool   `gorm:"column:vectorized;not null;default:false;index:idx_tools_vectorized" json:"vectorized"`
	Version    string `gorm:"column:version;size:64;not null;default:''" json:"version"`

	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index:idx_tools_deleted" json:"-"`
}

// TableName 表名
func (ToolPO) TableName() string { return "tools" }
