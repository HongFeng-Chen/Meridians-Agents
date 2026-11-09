package egress

// 出站传输的运行时闸门：把 Policy 挂到 net.Dialer 与 http.Client 上。
//
// 为什么不能只做「保存时校验」：
//   - 域名可以解析到内网（DNS rebinding：保存时解析到 1.2.3.4，调用时解析到 127.0.0.1）；
//   - 服务端可以 302 到 169.254.169.254，让客户端替它去取云元数据；
//   - 存量数据可能是在这套策略之前写进库的（历史配置、越权写入）。
//
// 所以校验必须在**真正发起连接的那一刻**做，并且覆盖每一跳重定向。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Options 传输级校验的开关
type Options struct {
	// SkipDialCheck 走显式配置的 HTTP 代理时置 true：
	// 真正 connect 的是代理地址，目标域名在代理侧解析，本地无法也不需要校验 IP。
	// 此时仍保留「保存时静态校验 + 重定向校验」，只是少了 IP 段这一层。
	SkipDialCheck bool
	// SkipRedirectCheck 不校验重定向目标（仅供「地址完全由进程自己生成」的
	// 内置服务使用：如平台自注册的 /mcp/platform 指向 127.0.0.1）。
	SkipRedirectCheck bool
	// MaxRedirects 覆盖策略里的最大跳数（0 表示用策略值）
	MaxRedirects int
}

// Transport 返回带出站校验的 Transport（在 base 的克隆上改，不影响全局默认值）。
func Transport(base *http.Transport, opts Options) *http.Transport {
	if base == nil {
		base = &http.Transport{}
	}
	tr := base.Clone()
	if opts.SkipDialCheck {
		return tr
	}

	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("出站校验：目标 %q 无法解析: %w", addr, err)
		}
		// 主机名层面的白名单（IP 段判断交给 Control 回调）
		if err := CheckHost(host); err != nil {
			return nil, err
		}
		// 每次连接单独建 Dialer，只为了把 host 闭包进 Control：
		// Control 拿到的 address 是**已解析的 IP:port**，是唯一能挡住 DNS rebinding
		// 的位置（解析结果已确定、连接尚未建立，此时拒绝零成本）；而
		// EGRESS_ALLOW_PRIVATE_HOSTS 需要「这次连的是哪台主机」才能收窄放行范围。
		d := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				ipStr, _, err := net.SplitHostPort(address)
				if err != nil {
					return fmt.Errorf("出站校验：地址 %q 无法解析: %w", address, err)
				}
				ip := net.ParseIP(ipStr)
				if ip == nil {
					return fmt.Errorf("出站校验：%q 不是合法 IP", ipStr)
				}
				return CheckIPForHost(ip, host)
			},
		}
		return d.DialContext(ctx, network, addr)
	}
	return tr
}

// Client 组装带校验的 http.Client（含重定向校验）。
func Client(timeout time.Duration, opts Options) *http.Client {
	return &http.Client{
		Transport:     Transport(http.DefaultTransport.(*http.Transport), opts),
		Timeout:       timeout,
		CheckRedirect: redirectGuard(opts),
	}
}

// GuardRedirects 给已有 Client 补重定向校验（transport 由调用方负责）。
func GuardRedirects(c *http.Client, opts Options) *http.Client {
	if c == nil || opts.SkipRedirectCheck {
		return c
	}
	c.CheckRedirect = redirectGuard(opts)
	return c
}

// redirectGuard 每一跳目标都重新做一遍 URL 校验：
// 否则一个 public endpoint 回 302 Location: http://169.254.169.254/ 就能绕过全部静态检查。
func redirectGuard(opts Options) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		max := CurrentPolicy().MaxRedirects
		if opts.MaxRedirects > 0 {
			max = opts.MaxRedirects
		}
		if len(via) > max {
			return fmt.Errorf("%w: 重定向超过 %d 跳", ErrBlocked, max)
		}
		if err := ValidateURL(req.URL.String()); err != nil {
			return fmt.Errorf("重定向目标被出站策略拒绝: %w", err)
		}
		return nil
	}
}
