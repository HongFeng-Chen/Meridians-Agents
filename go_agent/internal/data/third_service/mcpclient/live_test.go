package mcpclient

// 真实链路联调（可选）：对着本地桩服务跑一遍完整协议流程。
//
//	go run ./scripts 不起作用，桩服务是 Node 脚本：
//	  node scripts/mock_mcp_server.mjs --http 7788
//	  MCP_LIVE_URL=http://127.0.0.1:7788/mcp go test ./internal/data/third_service/mcpclient/ -run TestLiveHTTP -v
//
// 未设置 MCP_LIVE_URL 时自动跳过，因此不影响 CI。

import (
	"context"
	"os"
	"testing"
	"time"

	"go_agent/internal/biz/mcp"
)

func TestLiveHTTP(t *testing.T) {
	url := os.Getenv("MCP_LIVE_URL")
	if url == "" {
		t.Skip("未设置 MCP_LIVE_URL，跳过真实链路联调")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	srv := mcp.Server{
		ID:        1,
		Name:      "live",
		Namespace: "live",
		Transport: mcp.TransportHTTP,
		Endpoint:  url,
	}
	cli, err := NewDialer().Dial(ctx, srv, NewHTTPClientProvider().Client(srv, mcp.ClientOptions{TimeoutSec: 15}))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cli.Close() }()

	info, err := cli.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Logf("initialize ok: protocol=%s server=%s v%s", info.ProtocolVersion, info.Name, info.Version)

	tools, err := cli.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	t.Logf("tools/list ok: %d 个", len(tools))
	for _, tl := range tools {
		t.Logf("  - %s  %s", tl.Name, tl.Description)
	}
	if len(tools) == 0 {
		t.Fatal("期望至少一个工具")
	}

	res, err := cli.CallTool(ctx, tools[0].Name, map[string]any{"owner": "octocat", "repo": "hello-world"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("工具返回错误: %+v", res.Content)
	}
	t.Logf("tools/call ok: %d 个内容块，structured=%s", len(res.Content), res.Structured)

	if err := cli.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
