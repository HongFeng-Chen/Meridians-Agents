// Package aiplatform 外部 AI 平台 MCP 目录客户端。
//
// 千问 / 阿里云百炼把 MCP 服务收在「MCP 广场」，服务本身是标准 MCP Server：
//
//	Streamable HTTP  {base}/api/v1/mcps/{mcpCode}/mcp
//	SSE              {base}/api/v1/mcps/{mcpCode}/sse
//	鉴权             Authorization: Bearer $DASHSCOPE_API_KEY
//
// 目录（广场）接口各家形态不一、且不保证公开，因此这里做两件事：
//  1. 按 ai_platforms 里配置的 catalog_path 请求，**容错解码**多种 JSON 形态
//     （数组 / {data:[…]} / {data:{mcps|servers|items|list:[…]}} / 字段名 camelCase 或 snake_case）；
//  2. 目录不可用（404 / HTML / 非 JSON）时返回包裹 biz.ErrCatalogUnsupported 的错误，
//     上层据此提示「按 mcpCode 直接接入」——这条路只依赖路径模板，跟目录接口无关。
package aiplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	biz "go_agent/internal/biz/aiplatform"
	"go_agent/internal/conf"
)

const maxBodySize = 4 << 20 // 目录响应上限 4MB

// Client 实现 biz.Catalog
type Client struct {
	http *http.Client
}

var _ biz.Catalog = (*Client)(nil)

// New 创建客户端；超时复用 data.skill.httpTimeout（默认 30s）
func New(c *conf.Data) *Client {
	timeout := 30 * time.Second
	if t := c.GetSkill().GetHttpTimeout().AsDuration(); t > 0 {
		timeout = t
	}
	return &Client{http: &http.Client{Timeout: timeout}}
}

// Search 拉取平台目录里的 MCP 服务列表
func (c *Client) Search(ctx context.Context, p biz.Platform, keyword string, limit int) ([]biz.RemoteMCP, error) {
	path := strings.TrimSpace(p.CatalogPath)
	if path == "" {
		return nil, fmt.Errorf("%w: 平台 %s 未配置目录接口（catalog_path）", biz.ErrCatalogUnsupported, p.Name)
	}
	if limit <= 0 {
		limit = 20
	}
	endpoint, err := catalogURL(p, path, keyword, limit)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "go-agent-aiplatform/1.0")
	for k, v := range p.AuthHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 %s 目录失败: %w", p.Name, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w：平台 %s 返回 HTTP %d，检查 API Key 是否有效、是否有 MCP 广场权限",
			biz.ErrAuthFailed, p.Name, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, fmt.Errorf("%w: %s 返回 HTTP %d（目录地址可能不对，当前 %s）",
			biz.ErrCatalogUnsupported, p.Name, resp.StatusCode, endpoint)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("平台 %s 目录返回 HTTP %d：%s", p.Name, resp.StatusCode, snippet(raw))
	}

	items, ok := decodeList(raw)
	if !ok {
		if looksHTML(raw) {
			return nil, fmt.Errorf("%w: %s 返回的是 HTML 页面（该地址可能是控制台页面而不是 OpenAPI）",
				biz.ErrCatalogUnsupported, p.Name)
		}
		return nil, fmt.Errorf("%w: 目录响应无法解析（%s）", biz.ErrCatalogUnsupported, snippet(raw))
	}

	out := make([]biz.RemoteMCP, 0, len(items))
	for _, item := range items {
		code := pickString(item, "code", "mcp_code", "mcpCode", "server_code", "serverCode", "slug", "id", "name")
		if strings.TrimSpace(code) == "" {
			continue
		}
		name := pickString(item, "display_name", "displayName", "title", "cn_name", "name")
		if strings.TrimSpace(name) == "" {
			name = code
		}
		out = append(out, biz.RemoteMCP{
			Code:        strings.TrimSpace(code),
			Name:        strings.TrimSpace(name),
			Description: pickString(item, "description", "desc", "summary", "description_zh"),
			Transport:   normalizeTransport(pickString(item, "transport", "type")),
			Endpoint:    pickString(item, "endpoint", "url", "base_url", "baseUrl", "mcp_url"),
			ToolCount:   pickInt(item, "tool_count", "tools_count", "toolsCount", "tool_num"),
			Tags:        pickStrings(item, "tags", "categories", "labels"),
		})
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// catalogURL 拼目录地址：catalog_path 支持 {q} / {limit} 占位，否则用查询参数。
func catalogURL(p biz.Platform, path, keyword string, limit int) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		return "", fmt.Errorf("平台 %s 未配置 base_url", p.Name)
	}
	pathed := path
	hasQ := strings.Contains(pathed, "{q}")
	hasLimit := strings.Contains(pathed, "{limit}")
	pathed = strings.ReplaceAll(pathed, "{q}", url.QueryEscape(strings.TrimSpace(keyword)))
	pathed = strings.ReplaceAll(pathed, "{limit}", strconv.Itoa(limit))
	if !strings.HasPrefix(pathed, "/") && !strings.HasPrefix(pathed, "http") {
		pathed = "/" + pathed
	}
	full := pathed
	if !strings.HasPrefix(pathed, "http") {
		full = base + pathed
	}
	if hasQ && hasLimit {
		return full, nil
	}
	u, err := url.Parse(full)
	if err != nil {
		return "", fmt.Errorf("目录地址不合法: %w", err)
	}
	q := u.Query()
	if !hasQ {
		// 同时给两套参数名：不同平台认 q / keyword，多传不影响
		q.Set("q", strings.TrimSpace(keyword))
		q.Set("keyword", strings.TrimSpace(keyword))
	}
	if !hasLimit {
		q.Set("limit", strconv.Itoa(limit))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// normalizeTransport 把各家写法收敛到 http / sse
func normalizeTransport(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "sse":
		return biz.TransportSSE
	case "streamablehttp", "streamable_http", "streamable", "http", "remote":
		return biz.TransportHTTP
	default:
		return biz.TransportHTTP
	}
}

// decodeList 多形态解码：数组、{data:[…]}、{data:{items|list|mcps|servers:[…]}}、
// {items|list|mcps|servers:[…]}、{result:{…}}。
func decodeList(raw []byte) ([]map[string]any, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	if trimmed[0] == '[' {
		var arr []map[string]any
		if json.Unmarshal(trimmed, &arr) == nil {
			return arr, true
		}
		return nil, false
	}
	var root map[string]any
	if json.Unmarshal(trimmed, &root) != nil {
		return nil, false
	}
	return findList(root, 0)
}

// findList 递归找第一个「看起来是服务列表」的数组（最多 3 层，避免异常结构里乱钻）
func findList(obj map[string]any, depth int) ([]map[string]any, bool) {
	if depth > 3 {
		return nil, false
	}
	for _, key := range []string{"mcps", "servers", "items", "list", "data", "result", "results"} {
		v, ok := obj[key]
		if !ok || v == nil {
			continue
		}
		switch val := v.(type) {
		case []any:
			out := make([]map[string]any, 0, len(val))
			for _, it := range val {
				if m, ok := it.(map[string]any); ok {
					out = append(out, m)
				}
			}
			if len(out) > 0 {
				return out, true
			}
		case map[string]any:
			if out, ok := findList(val, depth+1); ok {
				return out, true
			}
		}
	}
	return nil, false
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				if strings.TrimSpace(s) != "" {
					return s
				}
			case json.Number:
				return s.String()
			case float64:
				return strconv.FormatFloat(s, 'f', -1, 64)
			}
		}
	}
	return ""
}

func pickInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i)
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				return i
			}
		}
	}
	return 0
}

func pickStrings(m map[string]any, keys ...string) []string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch arr := v.(type) {
		case []any:
			out := make([]string, 0, len(arr))
			for _, it := range arr {
				switch s := it.(type) {
				case string:
					if strings.TrimSpace(s) != "" {
						out = append(out, strings.TrimSpace(s))
					}
				case map[string]any:
					if n := pickString(s, "name", "label", "key"); n != "" {
						out = append(out, n)
					}
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func looksHTML(raw []byte) bool {
	trimmed := bytes.TrimSpace(bytes.ToLower(raw))
	return bytes.HasPrefix(trimmed, []byte("<!doctype")) || bytes.HasPrefix(trimmed, []byte("<html"))
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len([]rune(s)) <= 200 {
		return s
	}
	return string([]rune(s)[:200]) + "..."
}
