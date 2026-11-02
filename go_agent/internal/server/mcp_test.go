package server

// 协议层单测：直连 newPlatformMCPHandler（mcp-go StreamableHTTP，无状态模式），
// 走真实 JSON-RPC 帧；不启动 Kratos，不碰数据库。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/conversation"
	"go_agent/internal/biz/history"
	"go_agent/internal/biz/platformmcp"
	"go_agent/internal/service"
)

type stubTasks struct{}

func (stubTasks) ListTasks(context.Context, agentfactory.TaskFilter) ([]agentfactory.Task, error) {
	return []agentfactory.Task{{ID: 42, AgentKey: "a", Status: "succeeded", Title: "t"}}, nil
}
func (stubTasks) GetTask(_ context.Context, id uint64) (*agentfactory.Task, error) {
	return &agentfactory.Task{ID: id, AgentKey: "a", Status: "succeeded"}, nil
}
func (stubTasks) TaskEvents(_ context.Context, _ uint64, _ int) ([]agentfactory.TaskEvent, error) {
	return []agentfactory.TaskEvent{{Type: agentfactory.EventFinal, Payload: `{"answer":"ok"}`}}, nil
}

type stubConvs struct{}

func (stubConvs) List(context.Context, conversation.Filter) ([]conversation.Conversation, int64, error) {
	return nil, 0, nil
}
func (stubConvs) Get(_ context.Context, id string) (*conversation.Conversation, error) {
	return &conversation.Conversation{ID: id}, nil
}
func (stubConvs) ListMessagesPage(context.Context, string, uint32, int64) ([]conversation.Message, bool, error) {
	return nil, false, nil
}

type stubHist struct{}

func (stubHist) List(context.Context, history.Filter) ([]history.Record, int64, error) {
	return nil, 0, nil
}

func newStubUC() *platformmcp.UseCase { return platformmcp.New(stubTasks{}, stubConvs{}, stubHist{}) }

// postRPC 向 handler 发一条 JSON-RPC 请求，回解析后的响应体
func postRPC(t *testing.T, h http.Handler, method string, params any) map[string]any {
	t.Helper()
	frame := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		frame["params"] = params
	}
	raw, _ := json.Marshal(frame)
	req := httptest.NewRequest(http.MethodPost, service.PlatformMCPPath, bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:5555" // httptest 默认是 TEST-NET 段，会被回环守卫拦截
	req.Header.Set("Content-Type", "application/json")
	// MCP 规范要求 Accept 同时声明两种；mcp-go 会校验
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s 响应不是 JSON (HTTP %d): %s", method, w.Code, w.Body.String())
	}
	return resp
}

func TestInitialize(t *testing.T) {
	h := newPlatformMCPHandler(newStubUC())
	resp := postRPC(t, h, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		t.Fatalf("initialize 缺少 result: %v", resp)
	}
	if v, _ := res["protocolVersion"].(string); v == "" {
		t.Fatalf("缺少 protocolVersion: %v", res)
	}
	if _, ok := res["capabilities"]; !ok {
		t.Fatal("缺少 capabilities")
	}
	ins, _ := res["instructions"].(string)
	if !contains(ins, "list_tasks") {
		t.Fatalf("instructions 未说明工具: %v", ins)
	}
}

func TestToolsListAndCall(t *testing.T) {
	h := newPlatformMCPHandler(newStubUC())

	resp := postRPC(t, h, "tools/list", nil)
	res, _ := resp["result"].(map[string]any)
	arr, _ := res["tools"].([]any)
	if len(arr) != 7 {
		t.Fatalf("tools = %d, want 7", len(arr))
	}
	first, _ := arr[0].(map[string]any)
	if first["name"] == "" || first["inputSchema"] == nil {
		t.Fatalf("工具字段缺失: %+v", first)
	}

	resp = postRPC(t, h, "tools/call", map[string]any{
		"name": "list_tasks", "arguments": map[string]any{"limit": float64(1)},
	})
	res, _ = resp["result"].(map[string]any)
	if res == nil || res["isError"] == true {
		t.Fatalf("tools/call 不应失败: %v", resp)
	}
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content 数量异常: %v", res)
	}
	c0, _ := content[0].(map[string]any)
	if c0["type"] != "text" || !contains(str(c0["text"]), `"task_id":42`) {
		t.Fatalf("content 异常: %+v", c0)
	}

	// 未知工具 → mcp-go 在协议层拦截为 JSON-RPC -32602（标准行为）
	resp = postRPC(t, h, "tools/call", map[string]any{"name": "not_exists"})
	e, _ := resp["error"].(map[string]any)
	if e == nil {
		t.Fatalf("未知工具应是协议层 error: %v", resp)
	}
	if code, _ := e["code"].(float64); code != -32602 {
		t.Fatalf("未知工具错误码应 -32602, got %v", e["code"])
	}

	// 已知工具但业务参数不合法 → uc.Call 报错，按 MCP 约定放进 isError
	resp = postRPC(t, h, "tools/call", map[string]any{"name": "get_task_trace", "arguments": map[string]any{}})
	res, _ = resp["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("业务错误应标记 isError: %v", resp)
	}
}

func TestUnknownMethod(t *testing.T) {
	h := newPlatformMCPHandler(newStubUC())
	resp := postRPC(t, h, "resources/list", nil)
	e, _ := resp["error"].(map[string]any)
	if e == nil {
		t.Fatalf("未知方法应返回 JSON-RPC error: %v", resp)
	}
	if code, _ := e["code"].(float64); code != -32601 {
		t.Fatalf("错误码应为 -32601, got %v", e["code"])
	}
}

func TestLoopbackGuard(t *testing.T) {
	h := newPlatformMCPHandler(newStubUC())
	req := httptest.NewRequest(http.MethodPost, service.PlatformMCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.RemoteAddr = "10.1.2.3:5555" // 非本机
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("非本机应 403, got %d", w.Code)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:54321", true},
		{"[::1]:54321", true},
		{"localhost:1234", true},
		{"10.1.2.3:1234", false},
		{"192.168.1.5:80", false},
		{"", true}, // 测试/Unix socket
	}
	for _, c := range cases {
		r := &http.Request{RemoteAddr: c.addr}
		if got := isLoopback(r); got != c.want {
			t.Fatalf("isLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && bytes.Contains([]byte(s), []byte(sub))
}
func str(v any) string {
	s, _ := v.(string)
	return s
}
