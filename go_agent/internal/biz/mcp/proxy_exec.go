package mcp

// API 代理执行引擎：Token 获取与缓存、请求构造、HTTP 调用。
//
// 设计要点：
//   - Token 按「服务 ID + 鉴权配置哈希」缓存，过期前复用，401 时失效重取一次；
//   - 请求构造由 tools.route 驱动：路径参数替换、查询参数、请求头、JSON/form 请求体；
//   - 未在文档里声明的参数：GET/DELETE 落 query，其余落 body —— 兼容「文档写得糙」的系统。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v3/log"
)

const (
	// defaultProxyTimeout 未配置超时时的默认值
	defaultProxyTimeout = 30 * time.Second
	// maxProxyRespBytes 响应体读取上限（超出截断，避免撑爆上下文）
	maxProxyRespBytes = 256 << 10
	// defaultTokenTTL 拿不到过期时间时的 token 缓存时长
	defaultTokenTTL = 10 * time.Minute
	// tokenRefreshSkew 提前刷新窗口
	tokenRefreshSkew = 30 * time.Second
)

// proxyEngine Swagger 代理引擎（无状态，缓存放内存）。
type proxyEngine struct {
	tokens sync.Map // key: auth cache key → tokenEntry
	locks  sync.Map // key: auth cache key → *sync.Mutex（防并发重复换 token）
}

type tokenEntry struct {
	token  string
	expire time.Time
}

func newProxyEngine() *proxyEngine { return &proxyEngine{} }

// client 按配置构造 http.Client（超时走配置，连接复用 default transport）。
func (e *proxyEngine) client(cfg *ProxySpec) *http.Client {
	timeout := defaultProxyTimeout
	if cfg != nil && cfg.Request.TimeoutMS > 0 {
		timeout = time.Duration(cfg.Request.TimeoutMS) * time.Millisecond
	}
	return &http.Client{Timeout: timeout}
}

// BuildTools 解析文档并生成工具清单（每次同步都重新解析）。
//
// 返回的 cfg 已回填文档元信息与 ops 快照，base 为生效的服务地址；
// 调用方负责把 cfg 落库（见 UseCase.SyncTools）。
func (e *proxyEngine) BuildTools(ctx context.Context, s Server) ([]Tool, *ProxySpec, string, error) {
	cfg, err := DecodeProxySpec(s.ProxyConfig)
	if err != nil {
		return nil, nil, "", err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, "", err
	}
	doc, err := e.loadSpec(ctx, s, cfg)
	if err != nil {
		return nil, nil, "", err
	}
	base := resolveBaseURL(cfg, s, doc)
	if base == "" {
		return nil, nil, "", fmt.Errorf("%w: 文档没有声明服务地址，请填写 base_url", ErrInvalidServer)
	}

	tools := make([]Tool, 0, len(doc.ops))
	for _, op := range doc.ops {
		desc := op.Description
		if desc == "" {
			desc = op.Summary
		}
		if desc == "" {
			desc = fmt.Sprintf("%s %s", op.Method, op.Path)
		}
		desc = fmt.Sprintf("%s（%s %s）", desc, op.Method, op.Path)
		if op.Deprecated {
			desc = "[已废弃] " + desc
		}
		tools = append(tools, Tool{
			ServerID:    s.ID,
			ToolName:    op.Name,
			UniqueName:  uniqueToolName(s.EffectiveNamespace(), op.Name),
			DisplayName: firstNonEmpty(op.Summary, op.Method+" "+op.Path),
			Description: desc,
			InputSchema: op.InputSchema(),
			Tags:        op.Tags,
			Enabled:     true,
			Version:     doc.version,
			Route:       op.RouteJSON(cfg.Request.BodyType),
		})
	}

	cfg.Ops = doc.ops
	cfg.SpecTitle = doc.title
	cfg.SpecVersion = doc.version
	cfg.FetchedAt = time.Now().Format(time.RFC3339)
	if len(doc.warnings) > 0 {
		log.Warn("代理文档解析告警", "server", s.Name, "warnings", strings.Join(doc.warnings, "; "))
	}
	return tools, cfg, base, nil
}

// Preview 仅预览解析结果（不落库）：返回接口清单与文档信息。
func (e *proxyEngine) Preview(ctx context.Context, s Server, cfg *ProxySpec) (*specDoc, string, error) {
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	doc, err := e.loadSpec(ctx, s, cfg)
	if err != nil {
		return nil, "", err
	}
	base := resolveBaseURL(cfg, s, doc)
	if base == "" {
		return nil, "", fmt.Errorf("%w: 文档没有声明服务地址，请填写 base_url", ErrInvalidServer)
	}
	doc.baseURL = base
	return doc, base, nil
}

// Call 执行一次代理调用。
func (e *proxyEngine) Call(ctx context.Context, s Server, t Tool, args map[string]any) (*CallResult, error) {
	start := time.Now()
	cfg, err := DecodeProxySpec(s.ProxyConfig)
	if err != nil {
		return nil, err
	}
	route, err := decodeProxyRoute(t.Route)
	if err != nil {
		return nil, err
	}
	base := resolveBaseURL(cfg, s, &specDoc{baseURL: cfg.BaseURL})
	if base == "" {
		return nil, fmt.Errorf("代理服务 %s 未配置 base_url，请先同步工具", s.Name)
	}

	path := route.Path
	query := url.Values{}
	headers := map[string]string{}
	for k, v := range cfg.Request.Headers {
		headers[k] = v
	}
	var bodyArg any
	hasBody := false
	extra := map[string]any{}
	for k, v := range args {
		if k == "body" {
			bodyArg, hasBody = v, true
			continue
		}
		matched := false
		for _, p := range route.Params {
			if p.Name != k {
				continue
			}
			matched = true
			switch p.In {
			case "path":
				path = replacePathParam(path, k, fmt.Sprint(v))
			case "query":
				query.Set(k, queryString(v))
			case "header":
				headers[k] = fmt.Sprint(v)
			}
			break
		}
		if !matched {
			extra[k] = v
		}
	}
	if miss := missingPathParams(path); len(miss) > 0 {
		return nil, fmt.Errorf("缺少路径参数: %s", strings.Join(miss, ", "))
	}
	if allowBody(route.Method) && len(extra) > 0 {
		if m, ok := bodyArg.(map[string]any); ok {
			merged := make(map[string]any, len(m)+len(extra))
			for k, v := range m {
				merged[k] = v
			}
			for k, v := range extra {
				if _, exists := merged[k]; !exists {
					merged[k] = v
				}
			}
			bodyArg = merged
			hasBody = true
		} else if bodyArg == nil || bodyArg == "" {
			bodyArg = extra
			hasBody = true
		}
	} else if !allowBody(route.Method) && len(extra) > 0 {
		for k, v := range extra {
			query.Set(k, queryString(v))
		}
	}
	// 配置层固定 query 不覆盖调用方显式传值
	for k, v := range cfg.Request.Query {
		if query.Get(k) == "" {
			query.Set(k, v)
		}
	}

	full := base + ensureLeadingSlash(path)
	if len(query) > 0 {
		full += "?" + query.Encode()
	}

	// 请求构造闭包：401 重试时整体重建（body 在内存里，重建无成本）
	build := func() (*http.Request, error) {
		req, err := http.NewRequest(route.Method, full, nil)
		if err != nil {
			return nil, fmt.Errorf("请求构造失败: %w", err)
		}
		if hasBody {
			if err := attachBody(req, bodyArg, defaultStr(cfg.Request.BodyType, "json")); err != nil {
				return nil, err
			}
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/json")
		}
		if err := e.applyAuth(ctx, s, cfg, req); err != nil {
			return nil, err
		}
		return req, nil
	}

	req, err := build()
	if err != nil {
		return nil, err
	}
	resp, body, err := e.do(req, cfg)
	if err != nil {
		return nil, err
	}
	// 换来的 token 过期/被踢：失效后重取一次
	if resp.StatusCode == http.StatusUnauthorized && needsToken(cfg.Auth.Type) {
		e.invalidateToken(s.ID, cfg.Auth)
		if retry, rerr := build(); rerr == nil {
			if resp2, body2, err2 := e.do(retry, cfg); err2 == nil {
				resp, body = resp2, body2
			}
		}
	}

	out := &CallResult{ElapsedMs: time.Since(start).Milliseconds()}
	out.IsError = resp.StatusCode >= 400
	text := strings.TrimSpace(string(body))
	if text == "" {
		text = "（空响应体）"
	}
	out.Content = []ContentPart{{
		Type: "text",
		Text: fmt.Sprintf("HTTP %d %s\n%s", resp.StatusCode, http.StatusText(resp.StatusCode), text),
	}}
	if json.Valid(body) {
		out.Structured = text
	}
	return out, nil
}

// Ping 探活：抓文档或 GET base_url。成功条件是拿得到 2xx/3xx/4xx（4xx 说明服务活着）。
func (e *proxyEngine) Ping(ctx context.Context, s Server) error {
	cfg, err := DecodeProxySpec(s.ProxyConfig)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	target := firstNonEmpty(cfg.BaseURL, s.Endpoint)
	if strings.TrimSpace(cfg.SpecText) == "" && cfg.SpecURL != "" {
		target = cfg.SpecURL
	}
	if target == "" {
		return fmt.Errorf("未配置 base_url 或 spec_url，无法探活")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "*/*")
	_ = e.applyAuth(ctx, s, cfg, req)
	resp, err := e.client(cfg).Do(req)
	if err != nil {
		return fmt.Errorf("代理地址不可达: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("代理地址返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// —— Token ——

// applyAuth 按鉴权方案把凭证注入请求。
func (e *proxyEngine) applyAuth(ctx context.Context, s Server, cfg *ProxySpec, req *http.Request) error {
	a := cfg.Auth
	switch a.Type {
	case ProxyAuthNone, "":
		return nil
	case ProxyAuthStatic:
		req.Header.Set(defaultStr(a.Header, "Authorization"), a.Prefix+a.Token)
		return nil
	case ProxyAuthBasic:
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.Username+":"+a.Password)))
		return nil
	case ProxyAuthAPIKey:
		if a.In == "query" {
			q := req.URL.Query()
			q.Set(a.QueryName, a.Token)
			req.URL.RawQuery = q.Encode()
			return nil
		}
		req.Header.Set(defaultStr(a.Header, "X-API-Key"), a.Token)
		return nil
	case ProxyAuthLogin, ProxyAuthOAuth2:
		tk, err := e.token(ctx, s, cfg)
		if err != nil {
			return err
		}
		// 解码出来的配置可能没经过 Normalize（历史数据 / 外部写入），这里兜底前缀
		req.Header.Set(defaultStr(a.Header, "Authorization"), defaultStr(a.Prefix, "Bearer ")+tk)
		return nil
	default:
		return fmt.Errorf("%w: 未知鉴权类型 %s", ErrInvalidServer, a.Type)
	}
}

func needsToken(t string) bool { return t == ProxyAuthLogin || t == ProxyAuthOAuth2 }

// token 取缓存或用配置里的方案换新 token。
func (e *proxyEngine) token(ctx context.Context, s Server, cfg *ProxySpec) (string, error) {
	key := cfg.Auth.AuthCacheKey(s.ID)
	if v, ok := e.tokens.Load(key); ok {
		if te := v.(tokenEntry); te.token != "" && (te.expire.IsZero() || time.Now().Before(te.expire)) {
			return te.token, nil
		}
		e.tokens.Delete(key)
	}
	// 单飞：同一服务并发调用只换一次 token
	lv, _ := e.locks.LoadOrStore(key, &sync.Mutex{})
	mu := lv.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if v, ok := e.tokens.Load(key); ok {
		if te := v.(tokenEntry); te.token != "" && (te.expire.IsZero() || time.Now().Before(te.expire)) {
			return te.token, nil
		}
	}
	var (
		token  string
		expire time.Time
		err    error
	)
	switch cfg.Auth.Type {
	case ProxyAuthLogin:
		token, expire, err = e.loginToken(ctx, cfg)
	case ProxyAuthOAuth2:
		token, expire, err = e.oauthToken(ctx, cfg)
	default:
		return "", fmt.Errorf("%w: 该鉴权类型不需要 token", ErrInvalidServer)
	}
	if err != nil {
		return "", err
	}
	e.tokens.Store(key, tokenEntry{token: token, expire: expire})
	return token, nil
}

func (e *proxyEngine) invalidateToken(serverID uint64, a ProxyAuth) {
	e.tokens.Delete(a.AuthCacheKey(serverID))
}

// loginToken 调登录接口换 token：响应 JSON 里按 token_path 取值。
func (e *proxyEngine) loginToken(ctx context.Context, cfg *ProxySpec) (string, time.Time, error) {
	a := cfg.Auth
	body := a.LoginBody
	if strings.TrimSpace(body) == "" {
		body = `{"username":"${username}","password":"${password}"}`
	}
	body = strings.ReplaceAll(body, "${username}", a.Username)
	body = strings.ReplaceAll(body, "${password}", a.Password)

	req, err := http.NewRequestWithContext(ctx, defaultStr(a.LoginMethod, http.MethodPost), a.LoginURL, strings.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("登录请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range a.LoginHeaders {
		req.Header.Set(k, v)
	}
	resp, raw, err := e.do(req, cfg)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("登录请求失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", time.Time{}, fmt.Errorf("登录失败：HTTP %d %s", resp.StatusCode, clipText(string(raw), 200))
	}
	token, expire, err := extractToken(raw, a.TokenPath, a.ExpirePath)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expire, nil
}

// oauthToken client_credentials 换 token。
func (e *proxyEngine) oauthToken(ctx context.Context, cfg *ProxySpec) (string, time.Time, error) {
	a := cfg.Auth
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", a.ClientID)
	form.Set("client_secret", a.ClientSecret)
	if a.Scope != "" {
		form.Set("scope", a.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token 请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, raw, err := e.do(req, cfg)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token 请求失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", time.Time{}, fmt.Errorf("token 请求失败：HTTP %d %s", resp.StatusCode, clipText(string(raw), 200))
	}
	return extractToken(raw, firstNonEmpty(a.TokenPath, "access_token"), firstNonEmpty(a.ExpirePath, "expires_in"))
}

// extractToken 从响应取 token 与过期时间。
func extractToken(raw []byte, tokenPath, expirePath string) (string, time.Time, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", time.Time{}, fmt.Errorf("鉴权响应不是 JSON: %s", clipText(string(raw), 120))
	}
	val := jsonPathGet(doc, tokenPath)
	token := str(val)
	if strings.TrimSpace(token) == "" {
		return "", time.Time{}, fmt.Errorf("鉴权响应里取不到 token（路径 %s）：%s", tokenPath, clipText(string(raw), 200))
	}
	expire := time.Now().Add(defaultTokenTTL)
	if expirePath != "" {
		if v := jsonPathGet(doc, expirePath); v != nil {
			if sec, ok := toFloat(v); ok && sec > 0 {
				switch {
				case sec > 1e12: // 毫秒时间戳
					expire = time.UnixMilli(int64(sec))
				case sec > 1e9: // 秒时间戳
					expire = time.Unix(int64(sec), 0)
				default: // 相对秒数
					expire = time.Now().Add(time.Duration(sec) * time.Second)
				}
			}
		}
	}
	if time.Until(expire) > tokenRefreshSkew {
		expire = expire.Add(-tokenRefreshSkew)
	}
	return token, expire, nil
}

// jsonPathGet 支持 a.b.c 与 a.b[0].c 的取值。
func jsonPathGet(doc any, path string) any {
	cur := doc
	for _, seg := range strings.Split(strings.TrimSpace(path), ".") {
		if seg == "" {
			continue
		}
		name := seg
		idx := -1
		if i := strings.Index(seg, "["); i > 0 && strings.HasSuffix(seg, "]") {
			name = seg[:i]
			idx, _ = strconv.Atoi(strings.TrimSuffix(seg[i+1:], "]"))
		}
		if name != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[name]
		}
		if idx >= 0 {
			arr, ok := cur.([]any)
			if !ok || idx >= len(arr) {
				return nil
			}
			cur = arr[idx]
		}
	}
	return cur
}

// —— 请求辅助 ——

// do 发请求并读响应体（限流读取）。
func (e *proxyEngine) do(req *http.Request, cfg *ProxySpec) (*http.Response, []byte, error) {
	if req.Body != nil && req.GetBody == nil {
		// 保留重放能力（401 重试要用）
		buf, _ := io.ReadAll(io.LimitReader(req.Body, 10<<20))
		req.Body = io.NopCloser(bytes.NewReader(buf))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
	}
	resp, err := e.client(cfg).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("请求 %s %s 失败: %w", req.Method, req.URL.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyRespBytes))
	if err != nil {
		return resp, nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if len(body) >= maxProxyRespBytes {
		body = append(body, []byte("\n…（响应超过 256KB 已截断）")...)
	}
	return resp, body, nil
}

func attachBody(req *http.Request, payload any, bodyType string) error {
	if bodyType == "form" {
		m, ok := payload.(map[string]any)
		if !ok {
			return fmt.Errorf("form 请求体必须是对象")
		}
		form := url.Values{}
		for k, v := range m {
			form.Set(k, queryString(v))
		}
		encoded := form.Encode()
		req.Body = io.NopCloser(strings.NewReader(encoded))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(encoded)), nil }
		req.ContentLength = int64(len(encoded))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("请求体序列化失败: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	// 显式声明长度：否则部分服务端（含 Python BaseHTTPRequestHandler）读不到 body
	req.ContentLength = int64(len(b))
	req.Header.Set("Content-Type", "application/json")
	return nil
}

// replacePathParam 替换 {name} 与 {name:regex} 两种占位。
func replacePathParam(path, name, value string) string {
	path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(value))
	// Swagger2 支持带正则的占位：{id:\\d+}
	for {
		i := strings.Index(path, "{"+name+":")
		if i < 0 {
			break
		}
		j := strings.Index(path[i:], "}")
		if j < 0 {
			break
		}
		path = path[:i] + url.PathEscape(value) + path[i+j+1:]
	}
	return path
}

// missingPathParams 找出未替换的占位符。
func missingPathParams(path string) []string {
	var out []string
	for {
		i := strings.Index(path, "{")
		if i < 0 {
			break
		}
		j := strings.Index(path[i:], "}")
		if j < 0 {
			break
		}
		seg := path[i+1 : i+j]
		if k := strings.Index(seg, ":"); k >= 0 {
			seg = seg[:k]
		}
		out = append(out, seg)
		path = path[i+j+1:]
	}
	return out
}

func allowBody(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func queryString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return fmt.Sprint(v)
	}
}

func ensureLeadingSlash(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

func resolveBaseURL(cfg *ProxySpec, s Server, doc *specDoc) string {
	for _, cand := range []string{cfg.BaseURL, s.Endpoint, doc.baseURL} {
		if v := strings.TrimSpace(cand); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func clipText(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
