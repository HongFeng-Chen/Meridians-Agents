package service

// P1-03：MCP 服务的读接口不得回显凭证。

import (
	"strings"
	"testing"

	"go_agent/internal/biz/mcp"
)

func TestToMCPServerRedactsCredentials(t *testing.T) {
	src := &mcp.Server{
		ID:        1,
		Name:      "billing-api",
		Transport: mcp.TransportHTTP,
		Endpoint:  "https://api.example.com",
		Headers:   map[string]string{"Authorization": "Bearer sk-live-123", "X-Tenant": "acme"},
		Env:       map[string]string{"DB_PASSWORD": "p@ss"},
		ProxyConfig: `{"base_url":"https://api.example.com",` +
			`"auth":{"type":"static","token":"tk-live-456"}}`,
	}

	out := toMCPServer(src, false)

	for k, v := range out.GetHeaders() {
		if v != mcp.MaskValue {
			t.Fatalf("header %s 的值不应回显，实际 %q", k, v)
		}
	}
	if _, ok := out.GetHeaders()["Authorization"]; !ok {
		t.Fatal("键名应保留，前端才能显示已配置哪些头")
	}
	if out.GetEnv()["DB_PASSWORD"] != mcp.MaskValue {
		t.Fatalf("env 值不应回显，实际 %q", out.GetEnv()["DB_PASSWORD"])
	}
	if strings.Contains(out.GetProxyConfig(), "tk-live-456") {
		t.Fatalf("代理配置里的 token 不应回显: %s", out.GetProxyConfig())
	}
	if !out.GetHasCredentials() {
		t.Fatal("配了凭证应给出 has_credentials=true")
	}
	// 打码不能就地改坏内存里的服务对象（否则连接时会拿掩码去鉴权）
	if src.Headers["Authorization"] != "Bearer sk-live-123" {
		t.Fatalf("源对象被就地修改: %q", src.Headers["Authorization"])
	}
	if !strings.Contains(src.ProxyConfig, "tk-live-456") {
		t.Fatal("源代理配置被就地修改")
	}
}
