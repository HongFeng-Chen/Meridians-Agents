package principal

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go_agent/internal/pkg/auth"
	"go_agent/internal/pkg/identity"
)

// 认证错误：网关统一转 401，不区分「令牌缺失 / 过期 / 签名错误」，
// 避免给攻击者提供枚举信息。
var (
	ErrUnauthenticated = errors.New("身份认证失败")
	// ErrNotHandled 该 provider 不认识这种凭证形态，Resolver 继续尝试下一个。
	ErrNotHandled = errors.New("凭证不由该 provider 处理")
)

// Provider 身份提供方适配器：JWT / JWKS / OIDC / Token Introspection / API Key。
//
// Authenticate 只做「凭证 → Principal」，不查库、不判权限。
type Provider interface {
	Name() string
	Authenticate(ctx context.Context, credential string) (*Principal, error)
}

// Resolver 依次尝试多个 provider，第一个成功的结果生效。
//
// 顺序有意义：本地 HS256 最便宜，API Key 次之，远程 introspection 最贵；
// 把便宜的放前面，避免每个请求都打一次 IdP。
type Resolver struct {
	providers []Provider
}

// NewResolver 构建解析器（nil provider 会被忽略）。
func NewResolver(providers ...Provider) *Resolver {
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p != nil {
			out = append(out, p)
		}
	}
	return &Resolver{providers: out}
}

// Providers 已启用的 provider 名称（启动日志用）。
func (r *Resolver) Providers() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p.Name())
	}
	return out
}

// Resolve 解析凭证。
func (r *Resolver) Resolve(ctx context.Context, credential string) (*Principal, error) {
	if r == nil || len(r.providers) == 0 {
		return nil, ErrNotHandled
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return nil, fmt.Errorf("%w: 缺少凭证", ErrUnauthenticated)
	}
	var lastErr error
	for _, p := range r.providers {
		pr, err := p.Authenticate(ctx, credential)
		if err == nil && pr != nil {
			if pr.AuthType == "" {
				pr.AuthType = p.Name()
			}
			return pr, nil
		}
		if err != nil && !errors.Is(err, ErrNotHandled) {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthenticated, lastErr)
	}
	return nil, fmt.Errorf("%w: 没有 provider 能识别该凭证", ErrUnauthenticated)
}

// ---------------------------------------------------------------- API Key

// APIKeyProvider 受控服务接入：配置里 token → 固定 Principal。
//
// 只适合内网脚本、CI、巡检；不适合浏览器（令牌会出现在前端）。
type APIKeyProvider struct {
	keys map[string]*Principal
}

// NewAPIKeyProvider 从 map 构建（key 长度 < 16 的直接忽略，防止弱口令）。
func NewAPIKeyProvider(keys map[string]*Principal) *APIKeyProvider {
	clean := make(map[string]*Principal, len(keys))
	for k, v := range keys {
		if len(strings.TrimSpace(k)) < 16 || v == nil {
			continue
		}
		cp := *v
		cp.AuthType = "apikey"
		if cp.Issuer == "" {
			cp.Issuer = "internal"
		}
		clean[k] = &cp
	}
	if len(clean) == 0 {
		return nil
	}
	return &APIKeyProvider{keys: clean}
}

func (p *APIKeyProvider) Name() string { return "apikey" }

func (p *APIKeyProvider) Authenticate(_ context.Context, credential string) (*Principal, error) {
	// 遍历全部 key 做常量时间比较：不能因为「前缀不匹配」提前返回，
	// 否则响应时间会泄露 key 的前缀。
	var hit *Principal
	h := sha256.Sum256([]byte(credential))
	for k, v := range p.keys {
		kh := sha256.Sum256([]byte(k))
		if subtle.ConstantTimeCompare(h[:], kh[:]) == 1 {
			hit = v
		}
	}
	if hit == nil {
		return nil, ErrNotHandled
	}
	return hit, nil
}

// ---------------------------------------------------------------- JWT (HS256)

// JWTProvider 本地签发的 HS256 令牌：与 V1 网关验签逻辑完全复用同一把密钥，
// 不引入第二种令牌格式（登录接口签发的令牌可以走这里）。
type JWTProvider struct {
	verifier *auth.Verifier
}

// NewJWTProvider 复用 auth.Verifier。
func NewJWTProvider(v *auth.Verifier) *JWTProvider {
	if v == nil {
		return nil
	}
	return &JWTProvider{verifier: v}
}

func (p *JWTProvider) Name() string { return "jwt" }

func (p *JWTProvider) Authenticate(_ context.Context, credential string) (*Principal, error) {
	info, err := p.verifier.Verify(credential)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidToken) && !looksLikeJWT(credential) {
			return nil, ErrNotHandled
		}
		return nil, err
	}
	// 静态令牌也会被 verifier 接受，这里只为 JWT 建 Principal
	if info.Auth != "jwt" {
		return nil, ErrNotHandled
	}
	return FromIdentity(info), nil
}

func looksLikeJWT(token string) bool { return strings.Count(token, ".") == 2 }

// ---------------------------------------------------------------- JWKS

// JWKSProvider 校验外部 IdP 签发的 RS256 令牌（JWT + JWKS 模式）。
//
// 只依赖标准库：拉 JWKS、按 kid 找公钥、验 RS256 签名、校验 iss/aud/exp/nbf。
// 公钥缓存 10 分钟；遇到未知 kid 立即刷新一次（IdP 轮换密钥的常见场景）。
type JWKSProvider struct {
	jwksURL  string
	issuer   string
	audience string
	client   *http.Client

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	// now 可注入时钟（单测）
	now func() time.Time
}

// JWKSOption 可选参数
type JWKSOption func(*JWKSProvider)

// WithJWKSIssuer 校验 iss。
func WithJWKSIssuer(iss string) JWKSOption {
	return func(p *JWKSProvider) { p.issuer = strings.TrimSpace(iss) }
}

// WithJWKSAudience 校验 aud。
func WithJWKSAudience(aud string) JWKSOption {
	return func(p *JWKSProvider) { p.audience = strings.TrimSpace(aud) }
}

// WithJWKSHTTPClient 自定义 HTTP 客户端（超时 / 代理 / 测试用）。
func WithJWKSHTTPClient(c *http.Client) JWKSOption {
	return func(p *JWKSProvider) {
		if c != nil {
			p.client = c
		}
	}
}

// NewJWKSProvider 创建基准地址为 jwksURL 的校验器。
func NewJWKSProvider(jwksURL string, opts ...JWKSOption) *JWKSProvider {
	jwksURL = strings.TrimSpace(jwksURL)
	if jwksURL == "" {
		return nil
	}
	p := &JWKSProvider{
		jwksURL: jwksURL,
		client:  &http.Client{Timeout: 5 * time.Second},
		keys:    map[string]*rsa.PublicKey{},
		now:     time.Now,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *JWKSProvider) Name() string { return "jwks" }

func (p *JWKSProvider) Authenticate(ctx context.Context, credential string) (*Principal, error) {
	if !looksLikeJWT(credential) {
		return nil, ErrNotHandled
	}
	parts := strings.Split(credential, ".")
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrNotHandled
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, ErrNotHandled
	}
	if !strings.HasPrefix(strings.ToUpper(header.Alg), "RS") {
		// HS256 交给 JWTProvider，避免算法混淆
		return nil, ErrNotHandled
	}
	key, err := p.key(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	payload, err := verifyRS256(parts, header.Alg, key)
	if err != nil {
		return nil, err
	}
	return p.claimsToPrincipal(payload)
}

func (p *JWKSProvider) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.RLock()
	k, ok := p.keys[kid]
	fresh := p.now().Sub(p.fetchedAt) < 10*time.Minute
	p.mu.RUnlock()
	if ok && fresh {
		return k, nil
	}
	if err := p.refresh(ctx); err != nil {
		if ok {
			return k, nil // 刷新失败但缓存里有旧公钥：继续用，避免 IdP 抖动导致全站 401
		}
		return nil, fmt.Errorf("%w: 拉取 JWKS 失败: %v", ErrUnauthenticated, err)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if k, ok := p.keys[kid]; ok {
		return k, nil
	}
	// kid 为空且只有一个公钥时直接用它（部分 IdP 不设 kid）
	if kid == "" && len(p.keys) == 1 {
		for _, only := range p.keys {
			return only, nil
		}
	}
	return nil, fmt.Errorf("%w: JWKS 中没有 kid=%q 的公钥", ErrUnauthenticated, kid)
}

func (p *JWKSProvider) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if !strings.EqualFold(k.Kty, "RSA") || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 {
			continue
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	if len(keys) == 0 {
		return errors.New("JWKS 中没有可用的 RSA 公钥")
	}
	p.mu.Lock()
	p.keys = keys
	p.fetchedAt = p.now()
	p.mu.Unlock()
	return nil
}

func verifyRS256(parts []string, alg string, key *rsa.PublicKey) (map[string]any, error) {
	signed := []byte(parts[0] + "." + parts[1])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrUnauthenticated
	}
	var hash crypto.Hash
	switch strings.ToUpper(alg) {
	case "RS256":
		hash = crypto.SHA256
	case "RS384":
		hash = crypto.SHA384
	case "RS512":
		hash = crypto.SHA512
	default:
		return nil, fmt.Errorf("%w: 不支持的算法 %q", ErrUnauthenticated, alg)
	}
	h := hash.New()
	h.Write(signed)
	if err := rsa.VerifyPKCS1v15(key, hash, h.Sum(nil), sig); err != nil {
		return nil, fmt.Errorf("%w: 签名校验失败", ErrUnauthenticated)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrUnauthenticated
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return nil, ErrUnauthenticated
	}
	return claims, nil
}

func (p *JWKSProvider) claimsToPrincipal(claims map[string]any) (*Principal, error) {
	now := p.now()
	if exp, ok := numberClaim(claims, "exp"); ok {
		if now.After(time.Unix(exp, 0).Add(60 * time.Second)) {
			return nil, fmt.Errorf("%w: 令牌已过期", ErrUnauthenticated)
		}
	}
	if nbf, ok := numberClaim(claims, "nbf"); ok && now.Add(60*time.Second).Before(time.Unix(nbf, 0)) {
		return nil, fmt.Errorf("%w: 令牌尚未生效", ErrUnauthenticated)
	}
	if p.issuer != "" && !strings.EqualFold(stringClaim(claims, "iss"), p.issuer) {
		return nil, fmt.Errorf("%w: iss 不匹配", ErrUnauthenticated)
	}
	if p.audience != "" && !audienceMatch(claims["aud"], p.audience) {
		return nil, fmt.Errorf("%w: aud 不匹配", ErrUnauthenticated)
	}
	sub := firstNonEmpty(stringClaim(claims, "sub"), stringClaim(claims, "user_id"))
	if sub == "" {
		return nil, fmt.Errorf("%w: 缺少 sub", ErrUnauthenticated)
	}
	pr := &Principal{
		Issuer:        stringClaim(claims, "iss"),
		ClientID:      firstNonEmpty(stringClaim(claims, "client_id"), stringClaim(claims, "azp")),
		SubjectID:     sub,
		TenantID:      firstNonEmpty(stringClaim(claims, "tenant_id"), stringClaim(claims, "tid"), identity.DefaultTenant),
		Roles:         append(stringListClaim(claims, "role"), stringListClaim(claims, "roles")...),
		Scopes:        append(scopeClaim(claims), stringListClaim(claims, "scp")...),
		DepartmentIDs: append(stringListClaim(claims, "department_ids"), stringListClaim(claims, "departments")...),
		Audience:      stringListClaim(claims, "aud"),
		AuthType:      "jwks",
	}
	if pr.Issuer == "" {
		pr.Issuer = "external"
	}
	for _, r := range pr.Roles {
		if strings.EqualFold(strings.TrimSpace(r), "platform_admin") {
			pr.Admin = true
		}
	}
	return pr, nil
}

// ---------------------------------------------------------------- Introspection

// IntrospectionProvider RFC 7662：把不透明令牌交给 IdP 校验。
//
// 适合第三方只发 opaque token、又不愿意共享公钥的场景；代价是每个请求
// （缓存未命中时）多一次网络往返，生产环境建议在网关前置缓存。
type IntrospectionProvider struct {
	endpoint string
	clientID string
	secret   string
	client   *http.Client
}

// NewIntrospectionProvider 创建 introspection 适配器。
func NewIntrospectionProvider(endpoint, clientID, secret string) *IntrospectionProvider {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil
	}
	return &IntrospectionProvider{
		endpoint: endpoint,
		clientID: clientID,
		secret:   secret,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

func (p *IntrospectionProvider) Name() string { return "introspection" }

func (p *IntrospectionProvider) Authenticate(ctx context.Context, credential string) (*Principal, error) {
	// JWT 形态优先交给本地/ JWKS 校验，避免每个请求都打 IdP
	if looksLikeJWT(credential) {
		return nil, ErrNotHandled
	}
	form := url.Values{"token": {credential}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.clientID != "" {
		req.SetBasicAuth(p.clientID, p.secret)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: introspection 请求失败: %v", ErrUnauthenticated, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: introspection HTTP %d", ErrUnauthenticated, resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: introspection 响应不是 JSON", ErrUnauthenticated)
	}
	if active, _ := body["active"].(bool); !active {
		return nil, fmt.Errorf("%w: 令牌未激活", ErrUnauthenticated)
	}
	sub := firstNonEmpty(stringClaim(body, "sub"), stringClaim(body, "username"))
	if sub == "" {
		return nil, fmt.Errorf("%w: introspection 响应缺少 sub", ErrUnauthenticated)
	}
	return &Principal{
		Issuer:        firstNonEmpty(stringClaim(body, "iss"), "external"),
		ClientID:      stringClaim(body, "client_id"),
		SubjectID:     sub,
		TenantID:      firstNonEmpty(stringClaim(body, "tenant_id"), stringClaim(body, "tid"), identity.DefaultTenant),
		Roles:         append(stringListClaim(body, "role"), stringListClaim(body, "roles")...),
		Scopes:        append(scopeClaim(body), stringListClaim(body, "scp")...),
		DepartmentIDs: append(stringListClaim(body, "department_ids"), stringListClaim(body, "departments")...),
		Audience:      stringListClaim(body, "aud"),
		AuthType:      "introspection",
	}, nil
}

// ---------------------------------------------------------------- claim helpers

func stringClaim(claims map[string]any, key string) string {
	if key == "" {
		return ""
	}
	switch v := claims[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprintf("%.0f", v)
	}
	return ""
}

func numberClaim(claims map[string]any, key string) (int64, bool) {
	switch v := claims[key].(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

func stringListClaim(claims map[string]any, key string) []string {
	raw, ok := claims[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		// role/roles 这类 claim 有的 IdP 用逗号或空格分隔
		return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	return nil
}

func scopeClaim(claims map[string]any) []string {
	if v, ok := claims["scope"].(string); ok {
		return strings.Fields(v)
	}
	return nil
}

func audienceMatch(claim any, want string) bool {
	switch v := claim.(type) {
	case string:
		return strings.TrimSpace(v) == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) == want {
				return true
			}
		}
	}
	return false
}
