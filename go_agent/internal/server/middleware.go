package server

import (
	"context"
	"strconv"
	"strings"
	"time"

	"go_agent/internal/conf"
	"go_agent/internal/pkg/auth"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/metrics"
	"go_agent/internal/pkg/ratelimit"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/log"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/transport"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"google.golang.org/grpc/metadata"
)

// gate 网关策略集合：认证 + 限流。HTTP 与 gRPC 共用同一份实现，
// 保证两种协议的安全边界一致（不会出现「HTTP 要 token、gRPC 不要」的缺口）。
type gate struct {
	authOn   bool
	verifier *auth.Verifier
	header   string
	dev      identity.Info
	public   []string

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

	static := make(map[string]identity.Info, len(a.GetStaticTokens()))
	for tok, id := range a.GetStaticTokens() {
		user, tenant, _ := strings.Cut(id, ":")
		static[tok] = identity.Info{
			UserID:   firstNonEmpty(strings.TrimSpace(user), identity.DefaultUser),
			TenantID: firstNonEmpty(strings.TrimSpace(tenant), identity.DefaultTenant),
			Auth:     "static",
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

// isPublic 白名单前缀匹配（探活、指标、静态附件不需要令牌）
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

// middleware 组装认证中间件。认证失败返回 401（不区分「令牌缺失」与「令牌无效」，
// 避免给攻击者提供枚举信息）。
func (g *gate) middleware() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			if !g.authOn {
				return next(identity.With(ctx, g.dev), req)
			}
			if g.isPublic(requestPath(ctx)) {
				return next(identity.With(ctx, g.dev), req)
			}
			info, err := g.verifier.Verify(g.token(ctx))
			if err != nil {
				return nil, kerrors.Unauthorized("UNAUTHENTICATED", "未认证："+err.Error())
			}
			return next(identity.With(ctx, info), req)
		}
	}
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
