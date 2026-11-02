// Package aiplatform 外部 AI 平台（千问 / 百炼 / 自建 MCP 广场）接入域。
//
// 解决的问题：本平台的 MCP 服务只能一条条手工填 endpoint，技能只能从 skillhub 拉。
// 千问（阿里云百炼）这类平台把 MCP 服务做成了「广场 + 统一网关」：
//
//	Streamable HTTP  https://dashscope.aliyuncs.com/api/v1/mcps/{mcpCode}/mcp
//	SSE              https://dashscope.aliyuncs.com/api/v1/mcps/{mcpCode}/sse
//	鉴权             Authorization: Bearer $DASHSCOPE_API_KEY
//
// 因此这里把「平台」做成一份可配置的数据源（ai_platforms），提供三件事：
//  1. 平台 CRUD / 设为默认 / 连通性测试；
//  2. 从平台目录（MCP 广场接口）搜索服务 → 一键拉取成本地 MCP 服务（复用 biz/mcp 全套管线：
//     落库、连接、tools/list、向量化、能力目录）；
//  3. 目录接口不可用时按 mcpCode 直接接入（只依赖路径模板，不依赖目录 API）。
//
// 平台还可以顺带登记一个技能市场（skill_hubs）：填了 skill_base_url 就在保存时
// 幂等 upsert 一个同名 hub（token 复用平台 API Key），这样「千问的技能」走的是
// 已有的技能市场通道，不新增第二套技能体系。
package aiplatform

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 平台类型
const (
	KindQwen    = "qwen"    // 千问 / 阿里云百炼 MCP 广场（同一套网关）
	KindBailian = "bailian" // 兼容旧命名，行为与 qwen 一致
	KindGeneric = "generic" // 自建或第三方 MCP 广场
)

// 鉴权方式
const (
	AuthBearer = "bearer" // Authorization: Bearer <api_key>
	AuthHeader = "header" // 自定义头，值即 api_key
	AuthNone   = "none"   // 不需要鉴权
)

// 传输方式（与本地 MCP 服务对齐；SSE 仅用于展示/提示，导入统一走 Streamable HTTP）
const (
	TransportHTTP = "http"
	TransportSSE  = "sse"
)

// 千问/百炼的默认模板。真实接口见阿里云文档：
// https://help.aliyun.com/zh/model-studio/rich-code-application-mcp
const (
	QwenBaseURL        = "https://dashscope.aliyuncs.com"
	QwenCatalogPath    = "/api/v1/mcps"
	QwenStreamablePath = "/api/v1/mcps/{code}/mcp"
	QwenSSEPath        = "/api/v1/mcps/{code}/sse"
)

// 领域错误
var (
	ErrPlatformNotFound   = errors.New("AI 平台不存在")
	ErrPlatformExists     = errors.New("AI 平台名称已存在")
	ErrInvalidPlatform    = errors.New("AI 平台配置不合法")
	ErrNoPlatform         = errors.New("没有可用的 AI 平台，请先添加（如千问 / 百炼）")
	ErrCatalogUnsupported = errors.New("该平台没有可用的 MCP 目录接口，请用「按编码接入」直接拉取")
	// ErrAuthFailed 平台拒绝了当前凭证（401/403）：不是用户请求写错，而是配置的 Key / 权限不对
	ErrAuthFailed   = errors.New("平台鉴权失败")
	ErrImportFailed = errors.New("从平台接入 MCP 服务失败")
)

// Platform 外部 AI 平台（DO）
type Platform struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"` // 唯一标识，小写，如 qwen
	Kind string `json:"kind"` // qwen | bailian | generic

	BaseURL string `json:"base_url"`
	// APIKey 只进不出：读取接口只返回 has_key
	APIKey     string `json:"-"`
	HasKey     bool   `json:"has_key"`
	AuthScheme string `json:"auth_scheme"`           // bearer | header | none
	AuthHeader string `json:"auth_header,omitempty"` // auth_scheme=header 时的头名
	// Headers 附加请求头（除了鉴权头），如 {"X-Tenant":"x"}
	Headers map[string]string `json:"headers,omitempty"`

	// CatalogPath MCP 目录接口路径；为空表示平台不提供目录（只能按编码接入）
	CatalogPath string `json:"catalog_path,omitempty"`
	// StreamablePath / SSEPath 接入地址模板，{code} 会被替换成 mcpCode
	StreamablePath string `json:"streamable_path,omitempty"`
	SSEPath        string `json:"sse_path,omitempty"`

	// SkillBaseURL 非空时，保存平台会幂等 upsert 同名技能市场（token 复用 APIKey）
	SkillBaseURL string `json:"skill_base_url,omitempty"`
	SkillHub     string `json:"skill_hub,omitempty"`

	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	IsDefault   bool      `json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Masked 脱敏副本（API Key 只暴露「有没有」）
func (p Platform) Masked() Platform {
	p.HasKey = strings.TrimSpace(p.APIKey) != ""
	p.APIKey = ""
	return p
}

// RemoteMCP 平台目录里的一个 MCP 服务条目
type RemoteMCP struct {
	Code        string   `json:"code"` // mcpCode，接入地址模板的 {code}
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Provider    string   `json:"provider"`  // 平台名
	Transport   string   `json:"transport"` // 建议传输：http / sse
	Endpoint    string   `json:"endpoint"`  // 解析后的接入地址
	ToolCount   int      `json:"tool_count"`
	Tags        []string `json:"tags,omitempty"`
	Installed   bool     `json:"installed"`
	ServerName  string   `json:"server_name,omitempty"` // 接入后的本地服务名
}

// Filter 平台列表过滤
type Filter struct {
	Keyword string
	Enabled *bool
	Limit   int
}

// Input 新建/修改平台入参（指针字段为空表示不改）
type Input struct {
	Name           string            `json:"name"`
	Kind           string            `json:"kind"`
	BaseURL        *string           `json:"base_url"`
	APIKey         *string           `json:"api_key"`
	AuthScheme     *string           `json:"auth_scheme"`
	AuthHeader     *string           `json:"auth_header"`
	Headers        map[string]string `json:"headers"`
	CatalogPath    *string           `json:"catalog_path"`
	StreamablePath *string           `json:"streamable_path"`
	SSEPath        *string           `json:"sse_path"`
	SkillBaseURL   *string           `json:"skill_base_url"`
	Description    *string           `json:"description"`
	Enabled        *bool             `json:"enabled"`
	IsDefault      *bool             `json:"is_default"`
}

// ImportInput 从平台拉取一个 MCP 服务
type ImportInput struct {
	Platform    string `json:"platform"`
	Code        string `json:"code"`
	Name        string `json:"name"`      // 本地服务名，默认 <platform>-<code>
	Namespace   string `json:"namespace"` // 默认取平台名，工具唯一名即 <namespace>__<tool>
	Transport   string `json:"transport"` // http（默认）/ sse
	Description string `json:"description"`
	Enabled     *bool  `json:"enabled"`
	SyncTools   *bool  `json:"sync_tools"` // 默认 true
}

// ImportResult 接入结果
type ImportResult struct {
	ServerID   uint64 `json:"mcp_server_id"`
	ServerName string `json:"server_name"`
	Namespace  string `json:"namespace"`
	Endpoint   string `json:"endpoint"`
	ToolCount  int    `json:"tool_count"`
	Reused     bool   `json:"reused"`  // 本地已有同名服务：直接复用并重新同步工具
	Message    string `json:"message"` // 部分成功时的提示（如工具同步失败）
}

// TestResult 连通性测试结果
type TestResult struct {
	OK             bool   `json:"ok"`
	Message        string `json:"message"`
	LatencyMS      int64  `json:"latency_ms"`
	CatalogOK      bool   `json:"catalog_ok"`
	SampleCount    int    `json:"sample_count"`
	AuthConfigured bool   `json:"auth_configured"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// validName 平台名：小写字母或数字开头，可含 . _ -，最长 64
func validName(name string) bool { return nameRe.MatchString(name) }

// Normalize 归一化平台配置：填默认值、校验必填项。isCreate 为 true 时要求 name。
func (in *Input) Normalize(isCreate bool) error {
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	if in.Kind == "" {
		// kind 省略时按名称推断：name=qwen / bailian 就走对应预设，
		// 避免「name=qwen + api_key 即可」这条捷径在服务端失效。
		in.Kind = inferKind(in.Name)
	}
	switch in.Kind {
	case KindQwen, KindBailian, KindGeneric:
	default:
		return fmt.Errorf("%w: kind 只能是 qwen / bailian / generic", ErrInvalidPlatform)
	}
	if isCreate && !validName(in.Name) {
		return fmt.Errorf("%w: 名称需小写字母或数字开头，可含 . _ -，最长 64 字符", ErrInvalidPlatform)
	}

	applyDefaults(in)

	if in.BaseURL == nil {
		return fmt.Errorf("%w: base_url 不能为空（通用平台没有默认地址）", ErrInvalidPlatform)
	}
	if in.BaseURL != nil {
		base := strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
		if base == "" {
			return fmt.Errorf("%w: base_url 不能为空", ErrInvalidPlatform)
		}
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: base_url 需是 http(s) 地址", ErrInvalidPlatform)
		}
		in.BaseURL = &base
	}
	if in.AuthScheme != nil {
		scheme := strings.ToLower(strings.TrimSpace(*in.AuthScheme))
		if scheme == "" {
			scheme = AuthBearer
		}
		switch scheme {
		case AuthBearer, AuthHeader, AuthNone:
		default:
			return fmt.Errorf("%w: auth_scheme 只能是 bearer / header / none", ErrInvalidPlatform)
		}
		in.AuthScheme = &scheme
	}
	if in.StreamablePath != nil && strings.TrimSpace(*in.StreamablePath) != "" &&
		!strings.Contains(*in.StreamablePath, "{code}") {
		return fmt.Errorf("%w: streamable_path 必须包含 {code} 占位符", ErrInvalidPlatform)
	}
	if in.SSEPath != nil && strings.TrimSpace(*in.SSEPath) != "" &&
		!strings.Contains(*in.SSEPath, "{code}") {
		return fmt.Errorf("%w: sse_path 必须包含 {code} 占位符", ErrInvalidPlatform)
	}
	return nil
}

// inferKind 从平台名推断 kind（只在调用方没传 kind 时使用）。
// name 含 "qwen"/"千问" → qwen，含 "bailian"/"dashscope"/"百炼" → bailian，其余 generic。
func inferKind(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case n == "", n == "generic":
		return KindGeneric
	case strings.Contains(n, "qwen"), strings.Contains(n, "千问"):
		return KindQwen
	case strings.Contains(n, "bailian"), strings.Contains(n, "dashscope"), strings.Contains(n, "百炼"):
		return KindBailian
	default:
		return KindGeneric
	}
}

// applyDefaults 按 kind 补默认值（只补「没传」的字段，不改用户显式传的空值以外内容）
func applyDefaults(in *Input) {
	qwen := in.Kind == KindQwen || in.Kind == KindBailian
	if in.BaseURL == nil && qwen {
		v := QwenBaseURL
		in.BaseURL = &v
	}
	if in.CatalogPath == nil && qwen {
		v := QwenCatalogPath
		in.CatalogPath = &v
	}
	if in.StreamablePath == nil && qwen {
		v := QwenStreamablePath
		in.StreamablePath = &v
	}
	if in.SSEPath == nil && qwen {
		v := QwenSSEPath
		in.SSEPath = &v
	}
	if in.AuthScheme == nil && qwen {
		v := AuthBearer
		in.AuthScheme = &v
	}
	if in.Enabled == nil {
		v := true
		in.Enabled = &v
	}
}

// ResolveEndpoint 按模板解析某个 mcpCode 的接入地址。
func (p Platform) ResolveEndpoint(code, transport string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", fmt.Errorf("%w: mcpCode 不能为空", ErrInvalidPlatform)
	}
	tpl := strings.TrimSpace(p.StreamablePath)
	if strings.EqualFold(strings.TrimSpace(transport), TransportSSE) {
		tpl = strings.TrimSpace(p.SSEPath)
	}
	if tpl == "" {
		return "", fmt.Errorf("%w: 平台未配置 %s 接入路径模板", ErrInvalidPlatform, transport)
	}
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	pathPart := strings.ReplaceAll(tpl, "{code}", url.PathEscape(code))
	if strings.HasPrefix(pathPart, "http://") || strings.HasPrefix(pathPart, "https://") {
		return pathPart, nil
	}
	if !strings.HasPrefix(pathPart, "/") {
		pathPart = "/" + pathPart
	}
	return base + pathPart, nil
}

// AuthHeaders 生成鉴权头（额外 headers 会被合并，鉴权头优先）
func (p Platform) AuthHeaders() map[string]string {
	out := make(map[string]string, len(p.Headers)+1)
	for k, v := range p.Headers {
		if strings.TrimSpace(k) != "" {
			out[k] = v
		}
	}
	key := strings.TrimSpace(p.APIKey)
	switch strings.ToLower(strings.TrimSpace(p.AuthScheme)) {
	case AuthBearer:
		if key != "" {
			out["Authorization"] = "Bearer " + key
		}
	case AuthHeader:
		name := strings.TrimSpace(p.AuthHeader)
		if name == "" {
			name = "X-Api-Key"
		}
		if key != "" {
			out[name] = key
		}
	}
	return out
}

// LocalServerName 本地 MCP 服务名：<platform>-<code>，只保留安全字符。
func (p Platform) LocalServerName(code string) string {
	safe := make([]rune, 0, len(code))
	for _, r := range strings.TrimSpace(code) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			safe = append(safe, r)
		case r == ' ':
			safe = append(safe, '-')
		}
	}
	name := strings.Trim(string(safe), "-.")
	if name == "" {
		name = "mcp"
	}
	// 统一小写：服务名会出现在能力名（mcp:<ns>__<tool>）与 URL 里，大小写混排容易踩坑
	return strings.ToLower(strings.TrimSpace(p.Name) + "-" + name)
}

// EffectiveNamespace 默认命名空间 = 平台名（工具唯一名 <platform>__<tool>）
func (p Platform) EffectiveNamespace(ns string) string {
	if v := strings.TrimSpace(ns); v != "" {
		return v
	}
	return strings.TrimSpace(p.Name)
}
