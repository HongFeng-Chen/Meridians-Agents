package mcp

// MCP 域的出站校验接线用例：确认「写路径」用的是统一策略，
// 且平台内置服务（本机回环）是唯一豁免。

import (
	"testing"

	"go_agent/internal/pkg/egress"
)

func TestNormalizeRejectsPrivateEndpoint(t *testing.T) {
	// 本包默认放行回环（httptest 需要），这里显式恢复成生产默认
	defer withEgressPolicy(egress.Policy{MaxRedirects: 5})()

	cases := []string{
		"http://127.0.0.1:8000/mcp",
		"http://10.0.0.5:8500/v1/agent/self",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]:9000/mcp",
		"http://user:pass@mcp.example.com/mcp",
	}
	for _, ep := range cases {
		in := ServerInput{Name: "evil", Transport: TransportHTTP, Endpoint: ep}
		if err := in.Normalize(); err == nil {
			t.Errorf("endpoint %q 应当被拒绝", ep)
		}
	}

	// 公网域名放行
	in := ServerInput{Name: "vendor", Transport: TransportHTTP, Endpoint: "https://mcp.example.com/sse"}
	if err := in.Normalize(); err != nil {
		t.Errorf("公网 endpoint 应当放行: %v", err)
	}
}

// TestInternalServerLoopbackExempt 内置服务（/mcp/platform）指向本机回环，
// 必须豁免——否则默认配置下内置工具源直接不可用。
func TestInternalServerLoopbackExempt(t *testing.T) {
	defer withEgressPolicy(egress.Policy{MaxRedirects: 5})()

	ok := ServerInput{
		Name:      InternalServerName,
		Transport: TransportHTTP,
		Endpoint:  "http://127.0.0.1:8000/mcp/platform",
	}
	if err := ok.Normalize(); err != nil {
		t.Fatalf("内置服务应豁免出站策略: %v", err)
	}

	// 同一个地址换个名字就必须被拒（豁免只针对内置服务，不是地址白名单）
	bad := ServerInput{
		Name:      "not-internal",
		Transport: TransportHTTP,
		Endpoint:  "http://127.0.0.1:8000/mcp/platform",
	}
	if err := bad.Normalize(); err == nil {
		t.Fatal("非内置服务的回环地址应当被拒绝")
	}
}
