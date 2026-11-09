package egress

// 出站策略单测：覆盖「写路径静态校验」与「Dial 阶段 IP 校验」两条主线。
//
// 这里的用例是 SSRF 防护的回归基线——任何一条从「拒绝」变成「放行」，
// 都意味着后端又能被当成内网跳板了。

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withPolicy(t *testing.T, p Policy) {
	t.Helper()
	prev := CurrentPolicy()
	SetPolicy(p)
	t.Cleanup(func() { SetPolicy(prev) })
}

func TestValidateURL_DefaultPolicy(t *testing.T) {
	withPolicy(t, Policy{MaxRedirects: 5})

	blocked := []string{
		"http://127.0.0.1:8000/mcp",               // 回环
		"http://localhost:8000/mcp",               // 域名（Dial 阶段才拦，静态放行——见下一个用例）
		"http://169.254.169.254/latest/meta-data", // 云元数据
		"http://10.0.0.5:8500/v1/agent/self",      // 私网
		"http://172.20.1.1/",                      // 私网
		"http://192.168.1.1/",                     // 私网
		"http://100.64.0.1/",                      // CGNAT
		"http://0.0.0.0:9000/",                    // 未指定
		"http://[::1]:8000/mcp",                   // IPv6 回环
		"http://[fd00::1]/mcp",                    // IPv6 ULA
		"http://[::ffff:127.0.0.1]/mcp",           // IPv4-mapped
		"http://user:pwd@example.com/mcp",         // userinfo 混淆
		"file:///etc/passwd",                      // 非 http(s)
		"ftp://example.com/mcp",                   // 非 http(s)
		"https://metadata.google.internal/computeMetadata/v1/",
		"", // 空
	}
	for _, raw := range blocked {
		if err := ValidateURL(raw); err == nil {
			t.Errorf("应当拒绝但放行了: %q", raw)
		}
	}

	allowed := []string{
		"https://mcp.example.com/sse",
		"http://api.vendor.com:8080/mcp",
		"https://1.1.1.1/", // 公网字面 IP
	}
	for _, raw := range allowed {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("应当放行但拒绝了: %q → %v", raw, err)
		}
	}
}

func TestValidateURL_AllowPrivate(t *testing.T) {
	withPolicy(t, Policy{AllowPrivate: true, MaxRedirects: 5})

	for _, raw := range []string{"http://127.0.0.1:8000/mcp", "http://10.1.2.3/mcp", "http://192.168.1.1/"} {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("AllowPrivate 打开后应当放行 %q，实际 %v", raw, err)
		}
	}
	// 元数据域名是内置黑名单，AllowPrivate 也不能放
	if err := ValidateURL("https://metadata.google.internal/x"); err == nil {
		t.Error("云元数据域名在任何配置下都应被拒绝")
	}
	// 链路本地段是元数据服务的实际地址，AllowPrivate 同样不能放
	if err := ValidateURL("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Error("链路本地地址在任何配置下都应被拒绝")
	}
}

func TestValidateURL_AllowList(t *testing.T) {
	withPolicy(t, Policy{
		AllowedHosts: []string{".corp.internal", "mcp.vendor.com"},
		MaxRedirects: 5,
	})

	for _, raw := range []string{
		"https://mcp.vendor.com/sse",
		"https://a.b.corp.internal/mcp",
		"https://corp.internal/mcp",
	} {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("白名单内主机应放行 %q，实际 %v", raw, err)
		}
	}
	for _, raw := range []string{
		"https://evil.com/mcp",
		"https://corp.internal.evil.com/mcp", // 后缀伪装
	} {
		if err := ValidateURL(raw); err == nil {
			t.Errorf("白名单外主机应拒绝: %q", raw)
		}
	}
}

// TestValidateURL_AllowPrivateHosts 按主机收窄放行：
// 只放行名单里的主机，同网段的其他地址仍按默认策略拒绝。
// 本机 / 集群内跑 MCP 服务时用这个，比 EGRESS_ALLOW_PRIVATE=true 收得紧。
func TestValidateURL_AllowPrivateHosts(t *testing.T) {
	withPolicy(t, Policy{
		AllowPrivateHosts: []string{"127.0.0.1", ".svc.cluster.local"},
		MaxRedirects:      5,
	})

	for _, raw := range []string{
		"http://127.0.0.1:8080/mcp",                         // 本机 MCP
		"http://k8s-mcp.default.svc.cluster.local:8080/mcp", // 集群内服务
	} {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("白名单主机应放行私网 %q，实际 %v", raw, err)
		}
	}
	// 同一网段但不在名单里 —— 继续拒绝，否则「放行一台机器」就退化成「放行全网段」
	if err := ValidateURL("http://127.0.0.2:8080/mcp"); err == nil {
		t.Error("未列入白名单的回环地址应继续拒绝")
	}
	if err := ValidateURL("http://10.0.0.5:8500/v1/agent/self"); err == nil {
		t.Error("未列入白名单的私网地址应继续拒绝")
	}
	// 链路本地是云元数据地址，任何白名单都不能放
	if err := ValidateURL("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Error("链路本地地址即便配了私网白名单也应拒绝")
	}
}

// TestTransport_AllowPrivateHostsDial 运行时（Dial 阶段）同样按主机收窄：
// 同一份 Client 先被默认策略拒绝，放行名单加上该主机后必须能连上。
func TestTransport_AllowPrivateHostsDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	withPolicy(t, Policy{MaxRedirects: 5})
	if resp, err := Client(2*time.Second, Options{}).Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("默认策略下回环地址应被 Dial 阶段拒绝")
	}

	withPolicy(t, Policy{AllowPrivateHosts: []string{"127.0.0.1"}, MaxRedirects: 5})
	resp, err := Client(2*time.Second, Options{}).Get(srv.URL)
	if err != nil {
		t.Fatalf("私网白名单内主机应可连接，实际 %v", err)
	}
	_ = resp.Body.Close()
}

func TestValidateURL_DeniedListWins(t *testing.T) {
	withPolicy(t, Policy{
		AllowedHosts: []string{".example.com"},
		DeniedHosts:  []string{"bad.example.com"},
		MaxRedirects: 5,
	})
	if err := ValidateURL("https://bad.example.com/mcp"); err == nil {
		t.Error("黑名单应优先于白名单")
	}
}

func TestValidateURL_PortAllowList(t *testing.T) {
	withPolicy(t, Policy{AllowedPorts: []int{443, 8443}, MaxRedirects: 5})
	if err := ValidateURL("https://mcp.example.com/mcp"); err != nil {
		t.Errorf("默认端口应放行: %v", err)
	}
	if err := ValidateURL("https://mcp.example.com:8443/mcp"); err != nil {
		t.Errorf("8443 应放行: %v", err)
	}
	if err := ValidateURL("https://mcp.example.com:9000/mcp"); err == nil {
		t.Error("9000 不在端口白名单内，应拒绝")
	}
}

func TestCheckIP(t *testing.T) {
	withPolicy(t, Policy{MaxRedirects: 5})
	blocked := []string{
		"127.0.0.1", "::1", "10.0.0.1", "169.254.169.254", "fe80::1",
		"fd12:3456::1", "::ffff:10.0.0.1", "0.0.0.0", "224.0.0.1",
	}
	for _, s := range blocked {
		if err := CheckIP(net.ParseIP(s)); err == nil {
			t.Errorf("应拒绝的 IP 放行了: %s", s)
		}
	}
	if err := CheckIP(net.ParseIP("8.8.8.8")); err != nil {
		t.Errorf("公网 IP 应放行: %v", err)
	}
}

// TestTransport_BlocksLoopbackDial 验证「保存时没拦住、运行时必须拦住」：
// 直接构造 Transport 并请求 127.0.0.1，应当被 Dial 阶段拒绝。
func TestTransport_BlocksLoopbackDial(t *testing.T) {
	withPolicy(t, Policy{MaxRedirects: 5})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should not reach"))
	}))
	defer srv.Close()

	c := Client(2*time.Second, Options{})
	resp, err := c.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("回环地址应当被 Dial 阶段拒绝，实际请求成功")
	}
	if !errors.Is(err, ErrBlocked) && !strings.Contains(err.Error(), "出站访问被拒绝") {
		t.Fatalf("错误应来自出站策略，实际: %v", err)
	}
}

// TestTransport_BlocksRedirectToMetadata 验证重定向不能绕过策略：
// 公网主机 302 到 169.254.169.254 时，第二跳必须被拦。
func TestTransport_BlocksRedirectToMetadata(t *testing.T) {
	withPolicy(t, Policy{AllowPrivate: true, MaxRedirects: 5}) // 放行回环，只测重定向判定

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := Client(2*time.Second, Options{})
	resp, err := c.Get(srv.URL + "/redirect")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("重定向到云元数据地址应当被拒绝")
	}
	if !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("错误应指明重定向被拒，实际: %v", err)
	}
}

func TestTransport_MaxRedirects(t *testing.T) {
	withPolicy(t, Policy{AllowPrivate: true, MaxRedirects: 1})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 无限自我重定向，直到撞上跳数上限
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	c := Client(2*time.Second, Options{})
	resp, err := c.Get(srv.URL + "/loop")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("超过最大重定向跳数应当报错")
	}
	if !strings.Contains(err.Error(), "重定向超过") {
		t.Fatalf("错误应提示跳数超限，实际: %v", err)
	}
}
