package server

import (
	"context"
	"net/http"
	"testing"

	"go_agent/internal/conf"
	"go_agent/internal/pkg/identity"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/transport"
)

// 认证中间件：无令牌必须 401，静态令牌必须放行并写入身份。
func TestAuthMiddleware(t *testing.T) {
	sec := &conf.Security{Auth: &conf.Auth{
		Enabled:      true,
		JwtSecret:    "test-secret",
		Header:       "Authorization",
		DevUser:      "dev",
		DevTenant:    "dev-tenant",
		PublicPaths:  []string{"/healthz"},
		StaticTokens: map[string]string{"svc-token": "svc:ops"},
	}}
	g := newGate(sec)

	var got identity.Info
	next := func(ctx context.Context, _ any) (any, error) {
		got = identity.From(ctx)
		return "ok", nil
	}
	h := g.middleware()(next)

	// 1) 无令牌 → 401
	if _, err := h(context.Background(), nil); err == nil {
		t.Fatal("无令牌应被拒绝")
	} else if code := kerrors.FromError(err).Code; code != 401 {
		t.Fatalf("期望 401，得到 %d", code)
	}

	// 2) 静态令牌 → 放行，身份来自配置（user:tenant）
	if _, err := h(withToken(context.Background(), "svc-token"), nil); err != nil {
		t.Fatalf("静态令牌应放行: %v", err)
	}
	if got.UserID != "svc" || got.TenantID != "ops" || got.Auth != "static" {
		t.Fatalf("身份解析不对: %+v", got)
	}
}

// 认证关闭时必须回落到开发态身份，且不能因为缺令牌报错（本地开发/单机模式）。
func TestAuthDisabledFallsBackToDev(t *testing.T) {
	g := newGate(&conf.Security{Auth: &conf.Auth{DevUser: "dev", DevTenant: "dev-tenant"}})
	var got identity.Info
	h := g.middleware()(func(ctx context.Context, _ any) (any, error) {
		got = identity.From(ctx)
		return nil, nil
	})
	if _, err := h(context.Background(), nil); err != nil {
		t.Fatalf("认证关闭时不应报错: %v", err)
	}
	if got.UserID != "dev" || got.TenantID != "dev-tenant" {
		t.Fatalf("应使用开发态身份: %+v", got)
	}
}

// 白名单路径不需要令牌
func TestPublicPathBypassesAuth(t *testing.T) {
	g := newGate(&conf.Security{Auth: &conf.Auth{Enabled: true, JwtSecret: "s", PublicPaths: []string{"/healthz", "/metrics"}}})
	if !g.isPublic("/healthz") || !g.isPublic("/metrics") {
		t.Fatal("白名单应命中")
	}
	if g.isPublic("/api/conversations") {
		t.Fatal("业务路径不应在白名单内")
	}
}

// fakeTransport 最小可用的 HTTP transporter：只实现中间件需要的部分
// （取请求头与路径），避免为了测试去启动真实的 HTTP 服务。
type fakeTransport struct {
	req *http.Request
	op  string
}

func (t *fakeTransport) Kind() transport.Kind            { return transport.KindHTTP }
func (t *fakeTransport) Endpoint() string                { return "" }
func (t *fakeTransport) Operation() string               { return t.op }
func (t *fakeTransport) RequestHeader() transport.Header { return transport.Header(nil) }
func (t *fakeTransport) ReplyHeader() transport.Header   { return transport.Header(nil) }
func (t *fakeTransport) PathTemplate() string            { return t.op }
func (t *fakeTransport) Request() *http.Request          { return t.req }

// withToken 构造带 Authorization 头的 HTTP transport 上下文（模拟网关解析后的请求）。
func withToken(ctx context.Context, token string) context.Context {
	req, _ := http.NewRequest(http.MethodGet, "/api/conversations", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return transport.NewServerContext(ctx, &fakeTransport{req: req, op: "/api/conversations"})
}

// withRemote 构造带 RemoteAddr 的 HTTP transport 上下文（模拟不同来源的请求）。
func withRemote(ctx context.Context, addr, path string) context.Context {
	req, _ := http.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = addr
	return transport.NewServerContext(ctx, &fakeTransport{req: req, op: path})
}

// TestDevModeAdminOnlyFromLoopback 认证关闭时：
// 本机回环 → 视为本机管理员（保证开发可用）；远程匿名 → 一律不是管理员。
// 这是 P0 的核心闸门：AUTH_ENABLED=false + 0.0.0.0 监听时不能让公网配置 MCP。
func TestDevModeAdminOnlyFromLoopback(t *testing.T) {
	g := newGate(&conf.Security{Auth: &conf.Auth{DevUser: "dev", DevTenant: "dev"}})
	var got identity.Info
	h := g.middleware()(func(ctx context.Context, _ any) (any, error) {
		got = identity.From(ctx)
		return nil, nil
	})

	// 1) 远程来源：放行认证（开发态不拦业务），但没有管理权限
	if _, err := h(withRemote(context.Background(), "203.0.113.9:40000", "/api/mcp/servers"), nil); err != nil {
		t.Fatalf("开发态不应报认证错误: %v", err)
	}
	if got.Local || got.Admin || identity.AdminAllowed(identity.With(context.Background(), got)) {
		t.Fatalf("远程匿名请求不应获得管理权限: %+v", got)
	}

	// 2) 回环来源：允许本机管理
	if _, err := h(withRemote(context.Background(), "127.0.0.1:40000", "/api/mcp/servers"), nil); err != nil {
		t.Fatalf("回环请求不应报错: %v", err)
	}
	if !got.Local || !identity.AdminAllowed(identity.With(context.Background(), got)) {
		t.Fatalf("回环请求应获得管理权限: %+v", got)
	}
}

// TestStaticTokenAdminSegment 静态令牌 user:tenant:admin 才授予管理员
func TestStaticTokenAdminSegment(t *testing.T) {
	g := newGate(&conf.Security{Auth: &conf.Auth{
		Enabled:      true,
		JwtSecret:    "s",
		StaticTokens: map[string]string{"admin-tok": "ops:acme:admin", "user-tok": "dev:acme"},
	}})
	var got identity.Info
	h := g.middleware()(func(ctx context.Context, _ any) (any, error) {
		got = identity.From(ctx)
		return nil, nil
	})
	if _, err := h(withToken(context.Background(), "admin-tok"), nil); err != nil {
		t.Fatalf("admin 令牌应放行: %v", err)
	}
	if !got.Admin {
		t.Fatalf("admin-tok 应为管理员: %+v", got)
	}
	if _, err := h(withToken(context.Background(), "user-tok"), nil); err != nil {
		t.Fatalf("普通令牌应放行: %v", err)
	}
	if got.Admin {
		t.Fatalf("user-tok 不应为管理员: %+v", got)
	}
}

// 启动闸门：认证关闭 + 对外监听 → 拒绝启动；回环监听或显式开发模式 → 放行
func TestCheckInsecureExposure(t *testing.T) {
	off := &conf.Security{Auth: &conf.Auth{Enabled: false}}
	on := &conf.Security{Auth: &conf.Auth{Enabled: true, JwtSecret: "s"}}

	// 认证关闭 + 0.0.0.0：默认拒绝（这是最容易被搬上生产的默认配置）
	if err := CheckInsecureExposure(off, "0.0.0.0:8000"); err == nil {
		t.Fatal("认证关闭且监听 0.0.0.0 必须拒绝启动")
	}
	// 认证关闭 + 具体公网地址：同样拒绝
	if err := CheckInsecureExposure(off, "10.1.2.3:8000"); err == nil {
		t.Fatal("认证关闭且监听对外地址必须拒绝启动")
	}
	// 认证关闭 + 仅回环：本机开发，放行
	if err := CheckInsecureExposure(off, "127.0.0.1:8000", "127.0.0.1:9000"); err != nil {
		t.Fatalf("仅回环监听应放行: %v", err)
	}
	// localhost / [::1] 同样是回环，不能因为解不出 IP 就误判成对外监听
	if err := CheckInsecureExposure(off, "localhost:8000", "[::1]:9000"); err != nil {
		t.Fatalf("localhost / ::1 应视为回环: %v", err)
	}
	// 空 host（":8000"）= 所有网卡；绑定主机名同理无法在启动期确认解析结果
	if err := CheckInsecureExposure(off, ":8000"); err == nil {
		t.Fatal("空 host 等价于 0.0.0.0，必须拒绝启动")
	}
	if err := CheckInsecureExposure(off, "devbox:8000"); err == nil {
		t.Fatal("绑定主机名应按对外监听处理")
	}
	// 认证开启：放行
	if err := CheckInsecureExposure(on, "0.0.0.0:8000"); err != nil {
		t.Fatalf("认证开启应放行: %v", err)
	}
	// 显式开发模式：放行（逃生阀）
	t.Setenv("AUTH_DEV_INSECURE", "true")
	if err := CheckInsecureExposure(off, "0.0.0.0:8000"); err != nil {
		t.Fatalf("AUTH_DEV_INSECURE=true 应放行: %v", err)
	}
}
