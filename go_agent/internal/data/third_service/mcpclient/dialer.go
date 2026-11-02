package mcpclient

// 传输选择与 http.Client 工厂。
//
// http.Client 工厂是「动态注入」的落点：biz 层把 Server 配置和运行时覆盖项
// （超时/代理/跳过 TLS 校验）交给它，换代理只换 client，不改数据库。

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go_agent/internal/biz/mcp"
)

// Dialer 按服务配置建立客户端（实现 mcp.Dialer）
type Dialer struct{}

var _ mcp.Dialer = (*Dialer)(nil)

// NewDialer 创建传输选择器
func NewDialer() *Dialer { return &Dialer{} }

// Dial http 模式使用注入的 hc；stdio 模式忽略 hc
func (d *Dialer) Dial(ctx context.Context, s mcp.Server, hc *http.Client) (mcp.Client, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	switch strings.ToLower(strings.TrimSpace(s.Transport)) {
	case mcp.TransportHTTP:
		return newHTTPTransport(s, hc)
	case mcp.TransportStdio:
		return newStdioClient(s)
	default:
		return nil, fmt.Errorf("不支持的 transport: %q（只支持 stdio / http）", s.Transport)
	}
}

// HTTPClientProvider 按「服务 + 覆盖项」缓存 http.Client。
//
// 同一个服务在不同覆盖项下会得到不同 client；缓存避免每次连接都新建连接池。
type HTTPClientProvider struct {
	mu    sync.Mutex
	cache map[string]*http.Client
}

var _ mcp.ClientProvider = (*HTTPClientProvider)(nil)

// NewHTTPClientProvider 创建工厂
func NewHTTPClientProvider() *HTTPClientProvider {
	return &HTTPClientProvider{cache: make(map[string]*http.Client)}
}

// Client 返回（并缓存）该服务的 http.Client
func (p *HTTPClientProvider) Client(s mcp.Server, opts mcp.ClientOptions) *http.Client {
	key := fmt.Sprintf("%d|%d|%s|%t", s.ID, opts.TimeoutSec, opts.ProxyURL, opts.InsecureTLS)
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[key]; ok {
		return c
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 16
	if opts.InsecureTLS {
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	if proxy := strings.TrimSpace(opts.ProxyURL); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	timeout := 60 * time.Second
	if opts.TimeoutSec > 0 {
		timeout = time.Duration(opts.TimeoutSec) * time.Second
	}
	c := &http.Client{Transport: tr, Timeout: timeout}
	p.cache[key] = c
	return c
}

// Describe 运行时客户端摘要（写进状态里，前端能看出当前实例用的是哪套参数）
func Describe(opts mcp.ClientOptions) string {
	parts := []string{"mode=http"}
	timeout := opts.TimeoutSec
	if timeout <= 0 {
		timeout = 60
	}
	parts = append(parts, fmt.Sprintf("timeout=%ds", timeout))
	if opts.ProxyURL != "" {
		parts = append(parts, "proxy="+opts.ProxyURL)
	}
	if opts.InsecureTLS {
		parts = append(parts, "insecure_tls")
	}
	return strings.Join(parts, " ")
}
