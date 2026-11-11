// Package mcp MCP（Model Context Protocol）服务与工具管理域。
//
// 关系：一个 MCPServer 1:N Tool。
//
// 存储分工：
//   - MySQL：配置与工具元数据（mcp_servers / tools），软删除；
//   - Milvus：只放 tool_id + 向量 + 过滤字段（server_id / namespace / is_enabled），
//     召回拿到 id 后再回 MySQL/Redis 取完整详情；
//   - Redis：工具完整对象、运行状态、命名空间工具索引，作为热点缓存；
//   - http.Client：不落库，运行时由 Manager 按实例注入（超时/代理/TLS/连接池）。
//
// 工具同步链路（SyncTools）：tools/list → MySQL upsert → embedding → Milvus → Redis。
// 召回链路（SearchTools）：query embedding → Milvus topK → Redis → MySQL 降级。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// 传输类型
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http" // Streamable HTTP MCP
	// TransportProxy API 代理：导入 Swagger/OpenAPI 文档，把接口当工具调用
	TransportProxy = "proxy"
)

// DefaultNamespace 未显式指定命名空间时的默认值
const DefaultNamespace = "default"

// 领域错误
var (
	ErrServerNotFound = errors.New("MCP 服务不存在")
	ErrToolNotFound   = errors.New("工具不存在")
	ErrServerExists   = errors.New("MCP 服务名称已存在")
	ErrServerDisabled = errors.New("MCP 服务已停用")
	ErrNotConnected   = errors.New("MCP 服务未连接")
	ErrInvalidServer  = errors.New("MCP 服务配置不合法")
	ErrToolDisabled   = errors.New("工具已停用")
	ErrEmptyQuery     = errors.New("查询内容不能为空")
)

// Server MCP 服务（DO）
type Server struct {
	ID          uint64 `json:"id"`
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Transport   string `json:"transport"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`

	// HTTP 模式
	Endpoint string            `json:"endpoint,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`

	// Stdio 模式
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// Proxy 模式：Swagger/OpenAPI 导入配置（JSON 原文，结构见 ProxySpec）
	ProxyConfig string `json:"proxy_config,omitempty"`

	HealthCheckInterval int64 `json:"health_check_interval"`

	ToolCount int       `json:"tool_count"`
	Tools     []Tool    `json:"tools,omitempty"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EffectiveNamespace 命名空间兜底
func (s Server) EffectiveNamespace() string {
	if ns := strings.TrimSpace(s.Namespace); ns != "" {
		return ns
	}
	return DefaultNamespace
}

// Tool 工具（DO）
type Tool struct {
	ID          uint64 `json:"id"`
	ServerID    uint64 `json:"mcp_server_id"`
	ServerName  string `json:"mcp_server_name"`
	Namespace   string `json:"namespace"`
	ToolName    string `json:"tool_name"`
	UniqueName  string `json:"unique_name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// InputSchema / OutputSchema 为 JSON 原文，前端按文本框展示、按需解析
	InputSchema  string   `json:"input_schema"`
	OutputSchema string   `json:"output_schema"`
	Examples     []string `json:"examples,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Enabled      bool     `json:"enabled"`
	Vectorized   bool     `json:"vectorized"`
	Version      string   `json:"version"`
	// Route Proxy 工具到 HTTP 请求的映射（JSON：method/path/params），其他传输为空
	Route     string    `json:"route,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EmbeddingText 参与向量化的文本：唯一名 + 描述 + 标签 + 示例。
//
// 只喂「检索时用户会怎么说」的信息：schema 里的字段名噪声太大，不参与。
func (t Tool) EmbeddingText() string {
	var b strings.Builder
	b.WriteString(t.UniqueName)
	if d := strings.TrimSpace(t.Description); d != "" {
		b.WriteString("\n" + d)
	}
	if len(t.Tags) > 0 {
		b.WriteString("\n标签: " + strings.Join(t.Tags, ", "))
	}
	if len(t.Examples) > 0 {
		b.WriteString("\n示例: " + strings.Join(t.Examples, " / "))
	}
	return b.String()
}

// Status 运行状态（存 Redis，不落 MySQL）
type Status struct {
	ServerID   uint64    `json:"mcp_server_id"`
	ServerName string    `json:"server_name,omitempty"`
	Running    bool      `json:"running"`
	LastPingAt time.Time `json:"last_ping_at"`
	LastSyncAt time.Time `json:"last_sync_at"`
	ToolCount  int       `json:"tool_count"`
	ErrorMsg   string    `json:"error_msg"`
	// 以下为运行时信息（来自内存实例，不落库）
	HTTPClient      string `json:"http_client,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	ServerVersion   string `json:"server_version,omitempty"`
}

// Stats 规模统计
type Stats struct {
	Servers        int64 `json:"servers"`
	EnabledServers int64 `json:"enabled_servers"`
	Running        int64 `json:"running"`
	Tools          int64 `json:"tools"`
	EnabledTools   int64 `json:"enabled_tools"`
	Vectorized     int64 `json:"vectorized"`
	Namespaces     int64 `json:"namespaces"`
}

// ServerFilter 服务列表过滤
type ServerFilter struct {
	Keyword   string
	Namespace string
	Transport string
	Enabled   *bool
	Limit     int
}

// ToolFilter 工具列表过滤
type ToolFilter struct {
	ServerID uint64
	// Namespace 为空表示不过滤
	Namespace  string
	Keyword    string
	Enabled    *bool
	Vectorized *bool
	IDs        []uint64
	Limit      int
}

// ServerInput 创建/更新服务的入参
type ServerInput struct {
	Name                string
	Namespace           string
	Transport           string
	Description         string
	Endpoint            string
	Headers             map[string]string
	Command             string
	Args                []string
	Env                 map[string]string
	HealthCheckInterval int64
	Enabled             *bool
	// ProxyConfig Proxy 模式配置（JSON 原文，见 ProxySpec）
	ProxyConfig string
}

// Normalize 归一化并校验
func (in *ServerInput) Normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Namespace = strings.TrimSpace(in.Namespace)
	in.Transport = strings.ToLower(strings.TrimSpace(in.Transport))
	in.Description = strings.TrimSpace(in.Description)
	in.Endpoint = strings.TrimSpace(in.Endpoint)
	in.Command = strings.TrimSpace(in.Command)

	if in.Namespace == "" {
		in.Namespace = DefaultNamespace
	}
	if in.HealthCheckInterval < 0 {
		in.HealthCheckInterval = 0
	}
	if in.HealthCheckInterval == 0 {
		in.HealthCheckInterval = 30
	}
	if len(in.Description) > 512 {
		in.Description = string([]rune(in.Description)[:512])
	}

	if in.Name == "" {
		return fmt.Errorf("%w: name 不能为空", ErrInvalidServer)
	}
	if utf8.RuneCountInString(in.Name) > 128 {
		return fmt.Errorf("%w: name 最长 128 字符", ErrInvalidServer)
	}
	switch in.Transport {
	case TransportHTTP:
		if in.Endpoint == "" {
			return fmt.Errorf("%w: http 模式必须填 endpoint", ErrInvalidServer)
		}
		// 出站闸门：只校验 http(s):// 前缀等于让后端当内网跳板（SSRF），
		// 这里走统一策略（scheme / userinfo / 字面 IP 段 / 主机白名单 / 端口）。
		// 解析后的 IP 校验在 Dial 阶段做，见 egress_transport.go。
		if err := ValidateServerEndpoint(in.Name, in.Endpoint); err != nil {
			return err
		}
	case TransportStdio:
		// stdio 会在后端 exec 子进程：走统一策略（默认拒绝 + 白名单 + 路径/参数/环境校验），
		// 不能只校验 command 非空，否则管理面一旦失守就是远程命令执行。
		if err := ValidateStdio(in.Command, in.Args, in.Env); err != nil {
			return err
		}
	case TransportProxy:
		cfg, err := DecodeProxySpec(in.ProxyConfig)
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		// 归一化后的配置回写，保证落库内容与校验口径一致
		if normalized, err := EncodeProxySpec(cfg); err == nil {
			in.ProxyConfig = normalized
		}
	default:
		return fmt.Errorf("%w: transport 只能是 stdio / http / proxy", ErrInvalidServer)
	}
	return nil
}

// RemoteTool MCP 服务 tools/list 返回的原始工具
type RemoteTool struct {
	Name         string
	Title        string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  json.RawMessage
}

// ServerInfo initialize 握手返回的服务信息
type ServerInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
	Instructions    string
}

// ContentPart 工具返回的内容块（text / image / resource）
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Data 其余字段原文（image 的 data/mimeType 等）
	Data string `json:"data,omitempty"`
}

// CallResult 工具调用结果
type CallResult struct {
	IsError    bool          `json:"is_error"`
	Content    []ContentPart `json:"content"`
	Structured string        `json:"structured,omitempty"`
	// ElapsedMs 调用耗时（毫秒），便于前端展示
	ElapsedMs int64 `json:"elapsed_ms"`
	// Replayed 命中幂等记录的回放结果（没有真正调用工具）。
	// 任务重试链路用它拦住重复副作用；前端可提示「该结果为上次执行的回放」。
	Replayed bool `json:"replayed,omitempty"`
}

// ToolSync 一次 upsert 的差异统计
type ToolSync struct {
	Added      int
	Updated    int
	Unchanged  int
	Removed    int
	RemovedIDs []uint64
}

// SyncResult 工具同步结果
type SyncResult struct {
	ServerID   uint64 `json:"mcp_server_id"`
	ServerName string `json:"mcp_server_name,omitempty"`
	Fetched    int    `json:"fetched"`
	Added      int    `json:"added"`
	Updated    int    `json:"updated"`
	Removed    int    `json:"removed"`
	Vectorized int    `json:"vectorized"`
	// Warning 向量化失败等非致命问题
	Warning string `json:"warning,omitempty"`
}

// SearchHit 语义召回结果
type SearchHit struct {
	Tool  Tool    `json:"tool"`
	Score float64 `json:"score"`
	// Source 命中来源：redis | mysql
	Source string `json:"source"`
}

// ClientOptions 运行时 http.Client 覆盖项（不落库）
type ClientOptions struct {
	TimeoutSec  int
	ProxyURL    string
	InsecureTLS bool
}

// Client 底层 MCP 客户端（由 data/third_service/mcp 实现）
type Client interface {
	Initialize(ctx context.Context) (ServerInfo, error)
	ListTools(ctx context.Context) ([]RemoteTool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error)
	Ping(ctx context.Context) error
	Close() error
}

// Dialer 按配置建立客户端；hc 为注入的 http.Client（stdio 模式忽略）
type Dialer interface {
	Dial(ctx context.Context, s Server, hc *http.Client) (Client, error)
}

// ClientProvider 为 http 模式提供 http.Client（可在运行时应答覆盖项）
type ClientProvider interface {
	Client(s Server, opts ClientOptions) *http.Client
}

// Embedder 文本向量化（实现见 internal/biz/embedder）
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}
