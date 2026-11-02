package server

// 平台 MCP 端点：POST /mcp/platform（基于 mark3labs/mcp-go 封装）
//
// 之前是手写的 JSON-RPC 2.0 子集；现改用 mcp-go 的标准实现：
//   - mcp.NewMCPServer 承载工具注册（initialize / ping / tools/list / tools/call
//     以及 JSON-RPC 分帧、错误码、通知 202、DELETE 会话终止全部由库处理）；
//   - mcp.NewToolWithRawSchema 直接复用 platformmcp 工具定义里的 JSON Schema；
//   - server.NewStreamableHTTPServer(WithStateLess) 提供 Streamable HTTP 传输，
//     无状态模式不签发 Mcp-Session-Id，与本平台自研 mcpclient（一次请求一次
//     POST、有无会话头都兼容）对齐。
//
// 业务能力零变化：仍然直接调用 biz/platformmcp.UseCase 的 Tools()/Call()，
// 工具级错误按 MCP 约定放进 result.isError，而不是 JSON-RPC error。
//
// 安全：仅允许本机回环访问——平台数据（任务、会话、链路）敏感，
// 不通过公网地址暴露；Agent 与本机 MCP 管理器都走 127.0.0.1。

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"go_agent/internal/biz/platformmcp"
	"go_agent/internal/service"

	khttp "github.com/go-kratos/kratos/v3/transport/http"
)

const platformMCPServerName = "go-agent-platform"
const platformMCPServerVersion = "1.0.0"

// registerPlatformMCP 把端点挂到 Kratos HTTP server 上
func registerPlatformMCP(srv *khttp.Server, svc *service.PlatformMCPService) {
	if srv == nil || svc == nil || svc.UseCase() == nil {
		return
	}
	r := srv.Route("/")
	handler := newPlatformMCPHandler(svc.UseCase())
	r.POST(service.PlatformMCPPath, func(ctx khttp.Context) error {
		handler.ServeHTTP(ctx.Response(), ctx.Request())
		return nil
	})
	// GET（SSE 监听流）：无状态模式下由库回 405，仍挂上保持语义完整
	r.GET(service.PlatformMCPPath, func(ctx khttp.Context) error {
		handler.ServeHTTP(ctx.Response(), ctx.Request())
		return nil
	})
	// 客户端关闭会话时会发 DELETE：无状态模式没有会话，直接 204
	r.DELETE(service.PlatformMCPPath, func(ctx khttp.Context) error {
		ctx.Response().WriteHeader(http.StatusNoContent)
		return nil
	})
}

// newPlatformMCPHandler 组装平台 MCP 的 http.Handler（独立出来便于单测直连）
func newPlatformMCPHandler(uc *platformmcp.UseCase) http.Handler {
	// 1. 注册工具：定义来自 biz 层（唯一事实源），Schema 原样透传
	s := mcpserver.NewMCPServer(platformMCPServerName, platformMCPServerVersion,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery(),
		mcpserver.WithInstructions("平台内置只读数据源：任务中心（list_tasks/get_task/get_task_trace）、"+
			"会话（list_conversations/get_conversation/get_conversation_trace）、"+
			"操作历史（list_operation_histories）。不提供任何写操作。"),
	)
	for _, t := range uc.Tools() {
		tool := t // 捕获循环变量
		desc := strings.TrimSpace(tool.Description)
		if tool.Title != "" {
			desc = strings.TrimSpace(tool.Title + "：" + desc)
		}
		mcpTool := mcp.NewToolWithRawSchema(tool.Name, desc, json.RawMessage(tool.InputSchema))
		s.AddTool(mcpTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			out, err := uc.Call(ctx, tool.Name, req.GetArguments())
			if err != nil {
				// 工具级错误按 MCP 约定放进 content.isError，而不是 JSON-RPC error
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(out), nil
		})
	}

	// 2. Streamable HTTP 传输：无状态模式（无会话，POST 直回 JSON）
	streamable := mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r) {
			http.Error(w, "platform MCP 仅允许本机访问", http.StatusForbidden)
			return
		}
		streamable.ServeHTTP(w, r)
	})
}

// isLoopback 只放行本机请求（含 unix socket / 测试环境的空 RemoteAddr）
func isLoopback(r *http.Request) bool {
	host := strings.TrimSpace(r.RemoteAddr)
	if host == "" {
		return true
	}
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
