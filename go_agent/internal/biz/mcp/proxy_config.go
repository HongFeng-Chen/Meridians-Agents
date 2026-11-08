package mcp

// API 代理（transport=proxy）：把一份 Swagger / OpenAPI 文档里的接口转成 MCP 工具。
//
// 与 stdio/http 两种 MCP 传输的差别只在「工具从哪来、调用怎么发」：
//   - 工具来源：解析 spec 文档（paths → tools），不是 tools/list；
//   - 调用：拼 HTTP 请求（路径参数 / query / body）+ 注入鉴权，不是 JSON-RPC。
//
// 这样不同系统（自研后台、钉钉、内部中台…）的差异化鉴权与请求约定，
// 收敛成每份 spec 一份 ProxySpec 配置，对上层 Agent 完全一致：
// 工具仍然落在 tools 表，能力名仍是 mcp:<namespace>__<tool>。

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// 鉴权方案
const (
	// ProxyAuthNone 不鉴权（内网只读接口）
	ProxyAuthNone = "none"
	// ProxyAuthStatic 固定头/固定值（长期 token、内部密钥）
	ProxyAuthStatic = "static"
	// ProxyAuthBasic HTTP Basic（用户名 + 密码）
	ProxyAuthBasic = "basic"
	// ProxyAuthAPIKey 头或 query 里的固定 key
	ProxyAuthAPIKey = "api_key"
	// ProxyAuthLogin 调登录接口换 token（支持从响应 JSON 取值 + 过期时间）
	ProxyAuthLogin = "login"
	// ProxyAuthOAuth2 client_credentials 换 token
	ProxyAuthOAuth2 = "oauth2"
)

// ProxyAuth Token 获取方案。不同系统的差异都在这里。
type ProxyAuth struct {
	Type string `json:"type"`
	// Header 注入头名，默认 Authorization（api_key 且 in=header 时也用这个）
	Header string `json:"header,omitempty"`
	// Prefix 值前缀，如 "Bearer "
	Prefix string `json:"prefix,omitempty"`
	// Token static / api_key 的固定值
	Token string `json:"token,omitempty"`
	// In api_key 的注入位置：header | query
	In string `json:"in,omitempty"`
	// QueryName api_key 注入 query 时的参数名
	QueryName string `json:"query_name,omitempty"`

	// basic
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`

	// login：POST 登录接口换 token
	LoginURL     string            `json:"login_url,omitempty"`
	LoginMethod  string            `json:"login_method,omitempty"`
	LoginBody    string            `json:"login_body,omitempty"` // JSON 模板，支持 ${username} ${password}
	LoginHeaders map[string]string `json:"login_headers,omitempty"`
	// TokenPath 从登录响应里取 token 的 JSON 路径，如 data.access_token（支持数组下标 [0]）
	TokenPath string `json:"token_path,omitempty"`
	// ExpirePath 过期秒数的 JSON 路径，如 data.expires_in；取不到按 10 分钟缓存
	ExpirePath string `json:"expire_path,omitempty"`

	// oauth2 client_credentials
	TokenURL     string `json:"token_url,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// ProxyRequest 统一请求方案：所有接口共享的请求约定。
type ProxyRequest struct {
	// TimeoutMS 单次请求超时（默认 30000）
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// Headers 默认请求头（如 X-Tenant）
	Headers map[string]string `json:"headers,omitempty"`
	// Query 固定 query 参数（如 appKey、lang）
	Query map[string]string `json:"query,omitempty"`
	// BodyType 请求体编码：json（默认）| form
	BodyType string `json:"body_type,omitempty"`
}

// ProxySpec 一份 Swagger/OpenAPI 文档的代理配置（落 mcp_servers.proxy_config）。
type ProxySpec struct {
	// SpecURL 文档地址（http/https，支持 /v3/api-docs、swagger.json 等）
	SpecURL string `json:"spec_url,omitempty"`
	// SpecText 直接粘贴的文档内容（JSON 或 YAML），优先级高于 SpecURL
	SpecText string `json:"spec_text,omitempty"`
	// BaseURL 覆盖文档里的 servers[0].url / host+basePath（内网地址、网关前缀）
	BaseURL string `json:"base_url,omitempty"`

	Auth    ProxyAuth    `json:"auth"`
	Request ProxyRequest `json:"request"`

	// Ops 最近一次解析结果（同步时刷新，供前端展示与调用时定位接口）
	Ops []ProxyOp `json:"ops,omitempty"`
	// SpecTitle / SpecVersion 文档元信息（展示用）
	SpecTitle   string `json:"spec_title,omitempty"`
	SpecVersion string `json:"spec_version,omitempty"`
	// FetchedAt 最近一次解析时间
	FetchedAt string `json:"fetched_at,omitempty"`
}

// ProxyOp 一个接口（= 一个工具）。
type ProxyOp struct {
	// Name 工具名（operationId 归一化，缺失时由 method+path 生成）
	Name        string       `json:"name"`
	Summary     string       `json:"summary,omitempty"`
	Description string       `json:"description,omitempty"`
	Method      string       `json:"method"`
	Path        string       `json:"path"`
	Tags        []string     `json:"tags,omitempty"`
	Deprecated  bool         `json:"deprecated,omitempty"`
	Params      []ProxyParam `json:"params,omitempty"`
	// BodySchema 请求体 JSON Schema（可能是 $ref 解析后的对象）
	BodySchema string `json:"body_schema,omitempty"`
}

// ProxyParam 接口参数。
type ProxyParam struct {
	Name        string `json:"name"`
	In          string `json:"in"` // path | query | header | cookie | formData
	Type        string `json:"type,omitempty"`
	Format      string `json:"format,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
	Default     any    `json:"default,omitempty"`
	Enum        []any  `json:"enum,omitempty"`
}

// Validate 校验代理配置：至少要有文档来源，鉴权方案必须可落地。
func (p *ProxySpec) Validate() error {
	p.SpecURL = strings.TrimSpace(p.SpecURL)
	p.SpecText = strings.TrimSpace(p.SpecText)
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	if p.SpecURL == "" && p.SpecText == "" {
		return fmt.Errorf("%w: proxy 模式必须填 spec_url 或 spec_text", ErrInvalidServer)
	}
	// spec_url 与 base_url 都是服务端会主动去访问的地址，同样走
	// 出站策略（SSRF）：否则「导入一份 Swagger」就能让后端去探测内网。
	if p.SpecURL != "" {
		if err := ValidateEgressURL(p.SpecURL); err != nil {
			return err
		}
	}
	if p.BaseURL != "" {
		if err := ValidateEgressURL(p.BaseURL); err != nil {
			return err
		}
	}
	if err := p.Auth.normalize(); err != nil {
		return err
	}
	if p.Request.TimeoutMS <= 0 {
		p.Request.TimeoutMS = 30000
	}
	if p.Request.TimeoutMS > 300000 {
		p.Request.TimeoutMS = 300000
	}
	if bt := strings.ToLower(strings.TrimSpace(p.Request.BodyType)); bt == "" {
		p.Request.BodyType = "json"
	} else if bt != "json" && bt != "form" {
		return fmt.Errorf("%w: request.body_type 只能是 json 或 form", ErrInvalidServer)
	} else {
		p.Request.BodyType = bt
	}
	p.Request.Headers = cleanStrMap(p.Request.Headers)
	p.Request.Query = cleanStrMap(p.Request.Query)
	return nil
}

func (a *ProxyAuth) normalize() error {
	a.Type = strings.ToLower(strings.TrimSpace(a.Type))
	if a.Type == "" {
		a.Type = ProxyAuthNone
	}
	a.Header = strings.TrimSpace(a.Header)
	a.In = strings.ToLower(strings.TrimSpace(a.In))
	a.LoginMethod = strings.ToUpper(strings.TrimSpace(a.LoginMethod))
	if a.LoginMethod == "" {
		a.LoginMethod = "POST"
	}
	switch a.Type {
	case ProxyAuthNone:
		return nil
	case ProxyAuthStatic:
		if strings.TrimSpace(a.Token) == "" {
			return fmt.Errorf("%w: static 鉴权必须填 token", ErrInvalidServer)
		}
	case ProxyAuthBasic:
		if a.Username == "" {
			return fmt.Errorf("%w: basic 鉴权必须填 username", ErrInvalidServer)
		}
	case ProxyAuthAPIKey:
		if strings.TrimSpace(a.Token) == "" {
			return fmt.Errorf("%w: api_key 鉴权必须填 token", ErrInvalidServer)
		}
		if a.In == "" {
			a.In = "header"
		}
		if a.In != "header" && a.In != "query" {
			return fmt.Errorf("%w: api_key 的 in 只能是 header 或 query", ErrInvalidServer)
		}
		if a.In == "query" && strings.TrimSpace(a.QueryName) == "" {
			return fmt.Errorf("%w: api_key 注入 query 时必须填 query_name", ErrInvalidServer)
		}
	case ProxyAuthLogin:
		if !isHTTPURL(a.LoginURL) {
			return fmt.Errorf("%w: login 鉴权必须填合法的 login_url", ErrInvalidServer)
		}
		if strings.TrimSpace(a.TokenPath) == "" {
			return fmt.Errorf("%w: login 鉴权必须填 token_path（如 data.access_token）", ErrInvalidServer)
		}
	case ProxyAuthOAuth2:
		if !isHTTPURL(a.TokenURL) {
			return fmt.Errorf("%w: oauth2 鉴权必须填合法的 token_url", ErrInvalidServer)
		}
		if a.ClientID == "" {
			return fmt.Errorf("%w: oauth2 鉴权必须填 client_id", ErrInvalidServer)
		}
	default:
		return fmt.Errorf("%w: auth.type 只能是 none/static/basic/api_key/login/oauth2", ErrInvalidServer)
	}
	if a.Header == "" {
		a.Header = "Authorization"
	}
	if a.Prefix == "" && (a.Type == ProxyAuthLogin || a.Type == ProxyAuthOAuth2 || a.Type == ProxyAuthStatic) {
		a.Prefix = "Bearer "
	}
	if utf8.RuneCountInString(a.Token) > 4096 {
		a.Token = string([]rune(a.Token)[:4096])
	}
	return nil
}

// ProxyPreview 文档解析预览（保存前确认接口清单）。
type ProxyPreview struct {
	BaseURL  string    `json:"base_url"`
	Title    string    `json:"title"`
	Version  string    `json:"version"`
	Ops      []ProxyOp `json:"operations"`
	Warnings []string  `json:"warnings,omitempty"`
}

// DecodeProxySpec 解析 proxy_config JSON 原文。
func DecodeProxySpec(raw string) (*ProxySpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &ProxySpec{}, nil
	}
	var p ProxySpec
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("%w: proxy_config 不是合法 JSON: %v", ErrInvalidServer, err)
	}
	return &p, nil
}

// EncodeProxySpec 序列化代理配置（落库前调用，同时做归一化）。
func EncodeProxySpec(p *ProxySpec) (string, error) {
	if p == nil {
		return "", nil
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// AuthCacheKey 鉴权缓存键：同一服务 + 同一鉴权配置共用一个 token。
func (a ProxyAuth) AuthCacheKey(serverID uint64) string {
	b, _ := json.Marshal(a)
	// 密码类字段参与哈希，避免改了密码还在用旧 token
	return fmt.Sprintf("mcp:proxy:token:%d:%x", serverID, fnv32(b))
}

func isHTTPURL(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func cleanStrMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func fnv32(b []byte) uint32 {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	h := uint32(offset)
	for _, c := range b {
		h ^= uint32(c)
		h *= prime
	}
	return h
}
