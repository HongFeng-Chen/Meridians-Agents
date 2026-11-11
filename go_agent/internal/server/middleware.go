package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go_agent/internal/conf"
	"go_agent/internal/pkg/auth"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/mediaauth"
	"go_agent/internal/pkg/metrics"
	"go_agent/internal/pkg/principal"
	"go_agent/internal/pkg/ratelimit"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/log"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/transport"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// gate 网关策略集合：认证 + 限流。HTTP 与 gRPC 共用同一份实现，
// 保证两种协议的安全边界一致（不会出现「HTTP 要 token、gRPC 不要」的缺口）。
type gate struct {
	authOn   bool
	verifier *auth.Verifier
	header   string
	dev      identity.Info
	public   []string

	// external 外部身份适配器（V2 External Identity）：本地 HS256 验签失败后，
	// 依次尝试 JWKS / OIDC / Token Introspection / API Key。未配置时为 nil，
	// 行为与 V1 完全一致。
	external *principal.Resolver

	limiter      *ratelimit.Limiter
	writeLimiter *ratelimit.Limiter
}

// newGate 从配置构建网关策略。Security 为空时退化为「开发态 + 不限流」，
// 保持既有单机行为不变（配置里显式打开才生效，避免升级即断服务）。
func newGate(sec *conf.Security) *gate {
	a := sec.GetAuth()
	g := &gate{
		authOn: a.GetEnabled(),
		header: a.GetHeader(),
		dev: identity.Info{
			UserID:   firstNonEmpty(a.GetDevUser(), identity.DefaultUser),
			TenantID: firstNonEmpty(a.GetDevTenant(), identity.DefaultTenant),
			Auth:     "dev",
		},
		public: a.GetPublicPaths(),
	}
	// 登录端点本身必须免认证（不然鸡生蛋）：显式开启 login 才放行，
	// 关闭时走 404，不给未上线功能暴露攻击面的机会
	if a.GetLogin().GetEnabled() {
		g.public = append(g.public, "/api/auth/login")
	}

	static := make(map[string]identity.Info, len(a.GetStaticTokens()))
	for tok, id := range a.GetStaticTokens() {
		user, tenant, role := parseStaticIdentity(id)
		// 静态令牌第三段是角色：历史上只认 admin，现在也认 RBAC 角色
		// （operator / auditor / developer …），Admin 标记仍按管理员等价处理
		static[tok] = identity.Info{
			UserID:   firstNonEmpty(user, identity.DefaultUser),
			TenantID: firstNonEmpty(tenant, identity.DefaultTenant),
			Auth:     "static",
			Admin:    strings.EqualFold(role, "admin"),
			Roles:    roleList(role),
		}
	}
	g.verifier = auth.New(a.GetJwtSecret(), static)
	if g.header == "" {
		g.header = "Authorization"
	}
	if a.GetUserClaim() != "" {
		g.verifier.UserClaim = a.GetUserClaim()
	}
	if a.GetTenantClaim() != "" {
		g.verifier.TenantClaim = a.GetTenantClaim()
	}
	g.external = buildExternalIdentity(g.verifier)

	rl := sec.GetRateLimit()
	if rl.GetEnabled() {
		g.limiter = ratelimit.New(rl.GetQps(), int(rl.GetBurst()))
		writeQPS := rl.GetWriteQps()
		writeBurst := int(rl.GetWriteBurst())
		if writeQPS <= 0 {
			writeQPS = rl.GetQps()
			writeBurst = int(rl.GetBurst())
		}
		g.writeLimiter = ratelimit.New(writeQPS, writeBurst)
	}
	return g
}

// isPublic 白名单前缀匹配（探活、指标、内置 MCP 不需要令牌；附件走 filesHandler 自己的鉴权）
func (g *gate) isPublic(path string) bool {
	if path == "" {
		return false
	}
	for _, p := range g.public {
		if p == "" {
			continue
		}
		if p == path || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// operation 取路由模板（如 /api/conversations/{id}/chat），
// 用于指标标签与限流 key —— 用模板而不是原始路径，避免 ID 造成标签爆炸。
func operation(ctx context.Context) string {
	if tr, ok := transport.FromServerContext(ctx); ok {
		if op := tr.Operation(); op != "" {
			return op
		}
	}
	return ""
}

// requestPath 取真实请求路径（HTTP），gRPC 回落到 operation
func requestPath(ctx context.Context) string {
	if req, ok := khttp.RequestFromServerContext(ctx); ok && req != nil && req.URL != nil {
		return req.URL.Path
	}
	return operation(ctx)
}

// token 取请求携带的令牌：HTTP 读请求头，gRPC 读 metadata。
func (g *gate) token(ctx context.Context) string {
	if req, ok := khttp.RequestFromServerContext(ctx); ok && req != nil {
		return auth.TokenFromHeader(req.Header.Get(g.header))
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vs := md.Get(strings.ToLower(g.header)); len(vs) > 0 {
			return auth.TokenFromHeader(vs[0])
		}
		if vs := md.Get("authorization"); len(vs) > 0 {
			return auth.TokenFromHeader(vs[0])
		}
	}
	return ""
}

// allowMedia 附件静态访问判定（/api/files/ 是原始 net/http 路由，不经过上面的中间件）。
//
// 顺序：
//  1. 认证关闭（开发态）→ 放行，保持「打开就能看图」；
//  2. 地址带有效签名 → 放行。签名由服务端在读消息时补上，浏览器拿到的是
//     /api/files/…?sig=…，<img src> 照用；
//  3. 带有效令牌 → 放行（请求头，或 <img> 用的 ?token=）。
//
// 三条都不满足就 401。这样「拿到别人的附件地址」不再等于「能读别人的附件」，
// 前提是地址里的签名只对那一个路径有效。
func (g *gate) allowMedia(r *nethttp.Request) bool {
	if g == nil || !g.authOn {
		return true
	}
	if mediaauth.Verify(r.URL.Path, r.URL.Query().Get("sig")) {
		return true
	}
	tok := auth.TokenFromHeader(r.Header.Get(g.header))
	if tok == "" {
		tok = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if tok == "" {
		return false
	}
	_, err := g.verifier.Verify(tok)
	return err == nil
}

// middleware 组装认证中间件。认证失败返回 401（不区分「令牌缺失」与「令牌无效」，
// 避免给攻击者提供枚举信息）。
func (g *gate) middleware() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			if !g.authOn {
				// 开发态：身份固定为 dev，但只有回环请求才被视为「本机管理员」。
				// 这样 AUTH_ENABLED=false + 0.0.0.0 监听时，公网请求拿不到管理面权限。
				info := g.dev
				info.Local = isLoopbackCtx(ctx)
				ctx = identity.With(ctx, info)
				return next(principal.With(ctx, principal.FromIdentity(info)), req)
			}
			if g.isPublic(requestPath(ctx)) {
				info := g.dev
				info.Local = isLoopbackCtx(ctx)
				ctx = identity.With(ctx, info)
				return next(principal.With(ctx, principal.FromIdentity(info)), req)
			}
			tok := g.token(ctx)
			info, err := g.verifier.Verify(tok)
			if err != nil && g.external != nil {
				// 本地验签不认识这个凭证：交给外部 IdP 适配器（JWKS / Introspection / API Key）。
				// 失败时仍按 401 统一口径，不把「哪个 provider 失败」透给调用方。
				if p, perr := g.external.Resolve(ctx, tok); perr == nil {
					info, err = p.IdentityInfo(), nil
				}
			}
			if err != nil {
				return nil, kerrors.Unauthorized("UNAUTHENTICATED", "未认证："+err.Error())
			}
			ctx = identity.With(ctx, info)
			return next(principal.With(ctx, principal.FromIdentity(info)), req)
		}
	}
}

// buildExternalIdentity 从环境变量构建外部身份适配器。
//
// 为什么用环境变量而不是配置文件：这些参数（JWKS 地址、introspection 客户端密钥）
// 属于部署期密钥，放 env 便于容器注入与轮换，也避免把 IdP 密钥写进仓库里的 yaml。
//
//	IDENTITY_JWKS_URL            外部 IdP 的 JWKS 地址（RS256/384/512）
//	IDENTITY_JWKS_ISSUER         期望的 iss（可选但强烈建议）
//	IDENTITY_JWKS_AUDIENCE       期望的 aud（可选）
//	IDENTITY_OIDC_ISSUER         OIDC issuer；设置后自动发现 jwks_uri（与上者二选一）
//	IDENTITY_INTROSPECTION_URL   RFC 7662 introspection 端点
//	IDENTITY_CLIENT_ID / IDENTITY_CLIENT_SECRET  introspection 客户端凭证
//	IDENTITY_API_KEYS            服务令牌，形如 "token1=svc1:public,token2=svc2:acme"
//
// 全部为空时返回 nil：网关行为与 V1 完全一致（只认本地 HS256 与静态令牌）。
func buildExternalIdentity(local *auth.Verifier) *principal.Resolver {
	var providers []principal.Provider

	if local != nil {
		providers = append(providers, principal.NewJWTProvider(local))
	}

	jwksURL := strings.TrimSpace(os.Getenv("IDENTITY_JWKS_URL"))
	oidcIssuer := strings.TrimSpace(os.Getenv("IDENTITY_OIDC_ISSUER"))
	if jwksURL == "" && oidcIssuer != "" {
		if discovered := discoverJWKS(oidcIssuer); discovered != "" {
			jwksURL = discovered
			log.Info("外部身份：OIDC discovery 成功", "jwks_uri", jwksURL)
		} else {
			log.Warn("外部身份：OIDC issuer 发现失败，已跳过 JWKS 适配器", "issuer", oidcIssuer)
		}
	}
	if jwksURL != "" {
		opts := []principal.JWKSOption{}
		if iss := firstNonEmpty(os.Getenv("IDENTITY_JWKS_ISSUER"), oidcIssuer); iss != "" {
			opts = append(opts, principal.WithJWKSIssuer(iss))
		}
		if aud := strings.TrimSpace(os.Getenv("IDENTITY_JWKS_AUDIENCE")); aud != "" {
			opts = append(opts, principal.WithJWKSAudience(aud))
		}
		if p := principal.NewJWKSProvider(jwksURL, opts...); p != nil {
			providers = append(providers, p)
			log.Info("外部身份：JWKS 适配器已启用", "jwks_url", jwksURL)
		}
	}

	if endpoint := strings.TrimSpace(os.Getenv("IDENTITY_INTROSPECTION_URL")); endpoint != "" {
		if p := principal.NewIntrospectionProvider(endpoint, os.Getenv("IDENTITY_CLIENT_ID"), os.Getenv("IDENTITY_CLIENT_SECRET")); p != nil {
			providers = append(providers, p)
			log.Info("外部身份：Token Introspection 适配器已启用", "endpoint", endpoint)
		}
	}

	if raw := strings.TrimSpace(os.Getenv("IDENTITY_API_KEYS")); raw != "" {
		keys := map[string]*principal.Principal{}
		for _, pair := range strings.Split(raw, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			tok, rest, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			parts := strings.Split(rest, ":")
			if len(parts) < 2 {
				continue
			}
			p := &principal.Principal{
				Issuer:    "internal",
				SubjectID: strings.TrimSpace(parts[0]),
				TenantID:  strings.TrimSpace(parts[1]),
				AuthType:  "apikey",
			}
			if len(parts) > 2 {
				p.Roles = []string{strings.TrimSpace(parts[2])}
				p.Admin = strings.EqualFold(parts[2], "platform_admin")
			}
			keys[strings.TrimSpace(tok)] = p
		}
		if p := principal.NewAPIKeyProvider(keys); p != nil {
			providers = append(providers, p)
			log.Info("外部身份：API Key 适配器已启用", "count", len(keys))
		}
	}

	if len(providers) <= 1 {
		return nil
	}
	return principal.NewResolver(providers...)
}

// discoverJWKS 读取 OIDC discovery 文档里的 jwks_uri（启动时一次，失败不影响启动）。
func discoverJWKS(issuer string) string {
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	client := &nethttp.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != nethttp.StatusOK {
		return ""
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return ""
	}
	return strings.TrimSpace(doc.JWKSURI)
}

// insecureExposure 判断「认证关闭 + 监听非回环地址」这一组合是否成立。
//
// 这是平台最危险的一种默认状态：任何能连上端口的人都能读全部会话/记忆/
// Agent 定义，并消耗 token。管理面虽然另有回环闸门（identity.AdminAllowed），
// 但业务数据面没有任何保护。
func insecureExposure(addrs ...string) (string, bool) {
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		host := addr
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host == "" || host == "0.0.0.0" || host == "::" {
			return addr, true
		}
		// localhost 是回环别名；它和 127.0.0.1 一样只对本机可达，
		// 不能因为 ParseIP 解不出来就当成「对外监听」误杀开发配置。
		if strings.EqualFold(host, "localhost") {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			// 绑定了主机名（非 localhost）：启动期不确定它解析到哪个网卡，
			// 按对外处理更安全——误杀的代价是加一个环境变量，漏放的代价是数据面裸奔。
			return addr, true
		}
		if !ip.IsLoopback() {
			return addr, true
		}
	}
	return "", false
}

// CheckInsecureExposure 启动闸门：认证关闭且对外监听时拒绝启动。
//
// 为什么是「拒绝」而不是「告警」：告警在容器日志里滚两屏就没人看了，
// 而默认配置（AUTH_ENABLED=false + 0.0.0.0）恰恰是最容易被直接搬上生产的一份。
// 明确要求二选一：要么打开认证，要么显式声明「我知道这是开发环境」。
//
// 开发态逃生阀：AUTH_DEV_INSECURE=true（与 AUTH_DEV_TRUST_ALL 同风格，
// 走环境变量，启动打 WARN，仅用于本机/内网演示）。
func CheckInsecureExposure(sec *conf.Security, addrs ...string) error {
	if sec.GetAuth().GetEnabled() {
		return nil
	}
	addr, exposed := insecureExposure(addrs...)
	if !exposed {
		return nil
	}
	if envFlag("AUTH_DEV_INSECURE") {
		log.Warn("AUTH_DEV_INSECURE=true：认证关闭且监听对外地址，接口数据对任何能访问端口的人可读——仅限本机开发/内网演示",
			"addr", addr)
		return nil
	}
	return fmt.Errorf("拒绝启动：认证未启用（AUTH_ENABLED=false）且监听 %s（非回环）。"+
		"三选一：① 生产/任何对外环境设置 AUTH_ENABLED=true 与 AUTH_JWT_SECRET；"+
		"② 本机开发把监听收回回环：HTTP_ADDR=127.0.0.1:8000 GRPC_ADDR=127.0.0.1:9000（仓库默认值就是回环，原样启动不会触发本检查）；"+
		"③ 确实要从别的机器访问、又暂时不开认证：AUTH_DEV_INSECURE=true（启动会打 WARN）", addr)
}

// envFlag 读取布尔环境变量（1/true/yes/on）
func envFlag(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// warnInsecureExposure 非致命的暴露告警（server 构造期调用）。
// 真正的启动闸门在 CheckInsecureExposure（main 里最先执行）；这里保留告警，
// 让走了逃生阀或只监听回环的场景也能在日志里看到当前状态。
func warnInsecureExposure(sec *conf.Security, addrs ...string) {
	if sec.GetAuth().GetEnabled() {
		if identity.DevTrustAll() {
			log.Warn("AUTH_DEV_TRUST_ALL 已开启但认证是启用的：该开关只在认证关闭时生效，可忽略")
		}
		return
	}
	if identity.DevTrustAll() {
		// 逃生阀是显式打开的，但它确实把管理面重新变成了匿名可达，
		// 每次启动都提醒一次，避免「演示环境」悄无声息地变成生产配置。
		log.Warn("AUTH_DEV_TRUST_ALL=true：认证关闭时所有请求都被当作管理员，"+
			"管理面（MCP 配置 / 技能安装 / Agent 删除）对任何能访问端口的人开放——仅限不对外暴露的演示环境",
			"advice", "生产环境请设置 AUTH_ENABLED=true 并签发 role=admin 的令牌，然后关闭该开关",
		)
	}
	if addr, exposed := insecureExposure(addrs...); exposed {
		// 能走到这里说明走了 AUTH_DEV_INSECURE 逃生阀（否则 main 里已经拒绝启动了）
		log.Warn("认证未启用且监听地址对外暴露：业务数据可匿名读写，管理面仅允许本机回环操作",
			"addr", addr,
			"hint", "对外提供服务前请设置 AUTH_ENABLED=true，并给管理员令牌签发 role=admin",
		)
	}
}

// parseStaticIdentity 解析静态令牌身份 "user" / "user:tenant" / "user:tenant:admin"。
func parseStaticIdentity(v string) (user, tenant, role string) {
	parts := strings.SplitN(strings.TrimSpace(v), ":", 3)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	switch len(parts) {
	case 1:
		return parts[0], "", ""
	case 2:
		return parts[0], parts[1], ""
	default:
		return parts[0], parts[1], parts[2]
	}
}

// isLoopbackCtx 请求是否来自回环地址（HTTP 复用 mcp.go 的 isLoopback，gRPC 看 peer）。
// gRPC 没有 RemoteAddr 为空的历史兼容问题，空地址一律视为非本机（fail-closed）。
func isLoopbackCtx(ctx context.Context) bool {
	if req, ok := khttp.RequestFromServerContext(ctx); ok && req != nil {
		return isLoopback(req)
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		host := p.Addr.String()
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// rateLimit 限流中间件：维度 = 租户 + 路由模板；超限返回 429 并带 Retry-After。
func (g *gate) rateLimit() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			if g.limiter == nil {
				return next(ctx, req)
			}
			op := operation(ctx)
			if g.isPublic(requestPath(ctx)) || op == "" {
				return next(ctx, req)
			}
			lim := g.limiter
			if isWriteMethod(ctx) {
				lim = g.writeLimiter
			}
			key := identity.TenantID(ctx) + "|" + op
			if ok, wait := lim.Allow(key); !ok {
				if w, has := khttp.ResponseWriterFromServerContext(ctx); has && w != nil {
					secs := int(wait.Seconds())
					if secs < 1 {
						secs = 1
					}
					w.Header().Set("Retry-After", strconv.Itoa(secs))
				}
				metrics.Default.Inc("gateway_ratelimit_rejected_total", metrics.Labels{"route": op})
				return nil, kerrors.New(429, "TOO_MANY_REQUESTS", "请求过于频繁，请稍后重试")
			}
			return next(ctx, req)
		}
	}
}

// isWriteMethod 判断是否写请求（gRPC 一律按读算，靠并发与预算兜底）
func isWriteMethod(ctx context.Context) bool {
	if req, ok := khttp.RequestFromServerContext(ctx); ok && req != nil {
		switch req.Method {
		case "POST", "PUT", "PATCH", "DELETE":
			return true
		}
	}
	return false
}

// metricsMiddleware 采集 HTTP/gRPC 请求量与时延。
// status 从返回的错误里取（kratos 的 error.Code 即 HTTP 状态码），
// 成功固定 200/0 —— 不劫持 ResponseWriter，避免与 kratos 的编码器打架。
func metricsMiddleware(reg *metrics.Registry) middleware.Middleware {
	reg.Describe("http_server_requests_total", "服务端请求总数（按路由/方法/状态）", metrics.TypeCounter)
	reg.Describe("http_server_request_duration_seconds", "服务端请求耗时（秒）", metrics.TypeHistogram)
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			begin := time.Now()
			op := operation(ctx)
			if op == "" {
				op = "unknown"
			}
			labels := metrics.Labels{"route": op, "method": httpMethod(ctx)}
			resp, err := next(ctx, req)
			labels["status"] = statusLabel(err)
			reg.Inc("http_server_requests_total", labels)
			reg.Observe("http_server_request_duration_seconds", metrics.Labels{"route": op}, time.Since(begin).Seconds())
			return resp, err
		}
	}
}

// accessLog 结构化访问日志：一条请求一行，带身份、路由、耗时与错误。
// 探活与指标端点不记日志（会被探针刷屏）。
func accessLog() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			begin := time.Now()
			resp, err := next(ctx, req)
			path := requestPath(ctx)
			if path == "/metrics" || path == "/healthz" {
				return resp, err
			}
			id := identity.From(ctx)
			attrs := []any{
				"method", httpMethod(ctx),
				"route", operation(ctx),
				"path", path,
				"status", statusLabel(err),
				"latency_ms", time.Since(begin).Milliseconds(),
				"user", id.UserID,
				"tenant", id.TenantID,
			}
			if err != nil {
				attrs = append(attrs, "err", err.Error())
				log.Warn("request", attrs...)
			} else {
				log.Info("request", attrs...)
			}
			return resp, err
		}
	}
}

func httpMethod(ctx context.Context) string {
	if req, ok := khttp.RequestFromServerContext(ctx); ok && req != nil {
		return req.Method
	}
	return "GRPC"
}

// statusLabel 错误 → 状态码标签；kratos 的 Error.Code 就是 HTTP 码。
func statusLabel(err error) string {
	if err == nil {
		return "200"
	}
	return strconv.Itoa(int(kerrors.FromError(err).Code))
}

// roleList 静态令牌的角色段转角色列表（空段 = 无角色 → 默认 user）
func roleList(role string) []string {
	role = strings.TrimSpace(role)
	if role == "" {
		return nil
	}
	return []string{role}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
