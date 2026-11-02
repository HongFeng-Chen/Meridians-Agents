package mcpclient

// 进程内单测：httptest 桩直接实现 MCP 的 JSON-RPC 语义，不依赖外部进程。
// stdio 传输（子进程）的完整链路由 scripts/mock_mcp_server.mjs 联调覆盖
// （见 live_test.go 与 README 的联调命令）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go_agent/internal/biz/mcp"
)

const (
	protoV = "2025-06-18"
)

type stubTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type stubMCP struct {
	mu sync.Mutex

	sse      bool          // true 时响应走 text/event-stream
	rejectGT string        // 拒绝比这新的协议版本（用 -32602），验证降级重试
	requests []stubRequest // 收到的请求摘要
	deleted  int           // 收到的 DELETE（会话结束）次数
}

type stubRequest struct {
	ID      any    `json:"id"`
	Method  string `json:"method"`
	Session string
	Proto   string
}

func (s *stubMCP) toolSet() []stubTool {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"q": map[string]any{"type": "string"},
	}}
	return []stubTool{
		{Name: "alpha", Description: "工具 A", InputSchema: schema},
		{Name: "beta", Description: "工具 B", InputSchema: schema},
		{Name: "gamma", Description: "工具 C", InputSchema: schema},
	}
}

func (s *stubMCP) Handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			s.mu.Lock()
			s.deleted++
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("不支持的请求方式 %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var msg rpcMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("解析请求: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, stubRequest{
			ID: func() any {
				if msg.ID == nil {
					return nil
				}
				return *msg.ID
			}(),
			Method: msg.Method, Session: r.Header.Get("Mcp-Session-Id"), Proto: r.Header.Get("MCP-Protocol-Version"),
		})
		s.mu.Unlock()

		// initialize 之前不带会话头；之后必须带上
		if msg.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "stub-session" {
			t.Errorf("%s 缺少会话头", msg.Method)
		}
		// 握手之后应带 MCP-Protocol-Version（降级场景下是协商出来的老版本）
		if msg.Method != "initialize" {
			pv := r.Header.Get("MCP-Protocol-Version")
			if !slices.Contains(protocolVersions, pv) {
				t.Errorf("%s 协议版本头 = %q，期望是已协商的版本之一", msg.Method, pv)
			}
		}

		var out any
		switch msg.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			// rpcMessage.Params 解码后可能是 map[string]any（非 json.RawMessage），两种都兼容
			switch pv := msg.Params.(type) {
			case json.RawMessage:
				_ = json.Unmarshal(pv, &p)
			case map[string]any:
				raw, _ := json.Marshal(pv)
				_ = json.Unmarshal(raw, &p)
			}
			if s.rejectGT != "" && p.ProtocolVersion != s.rejectGT {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": msg.ID,
					"error": map[string]any{"code": -32602, "message": "unsupported protocol version " + p.ProtocolVersion},
				})
				return
			}
			out = map[string]any{
				"protocolVersion": func() string {
					if s.rejectGT != "" {
						return s.rejectGT
					}
					return protoV
				}(),
				"capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo":   map[string]any{"name": "stub", "version": "9.9.9"},
			}
		case "ping":
			out = map[string]any{}
		case "tools/list":
			cursor := ""
			if m, ok := msg.Params.(map[string]any); ok {
				if c, ok := m["cursor"].(string); ok {
					cursor = c
				}
			}
			// 每页 2 条，验证客户端自动翻页
			all := s.toolSet()
			start, size := 0, 2
			if cursor == "2" {
				start = 2
			}
			end := start + size
			if end > len(all) {
				end = len(all)
			}
			res := map[string]any{"tools": all[start:end]}
			if end < len(all) {
				res["nextCursor"] = "2"
			}
			out = res
		case "tools/call":
			out = map[string]any{
				"content":           []map[string]any{{"type": "text", "text": "done"}},
				"structuredContent": map[string]any{"ok": true},
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32601, "message": "no such method"},
			})
			return
		}

		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": out})
		if s.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Mcp-Session-Id", "stub-session")
			// 先塞一条与请求 id 无关的通知，验证客户端按 id 过滤
			fmt.Fprintln(w, "event: message")
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"notifications/message","params":{}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, "event: message")
			fmt.Fprintf(w, "data: %s\n", raw)
			fmt.Fprintln(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "stub-session")
		_, _ = w.Write(raw)
	})
}

func dialStub(t *testing.T, srv mcp.Server) mcp.Client {
	t.Helper()
	cli, err := NewDialer().Dial(context.Background(), srv, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return cli
}

func runFlow(t *testing.T, cli mcp.Client, wantProto string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := cli.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "stub" || info.Version != "9.9.9" || info.ProtocolVersion != wantProto {
		t.Fatalf("握手信息不符合预期: %+v", info)
	}
	tools, err := cli.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 3 || tools[0].Name != "alpha" || tools[2].Name != "gamma" {
		t.Fatalf("翻页结果不符合预期: %+v", tools)
	}
	res, err := cli.CallTool(ctx, "alpha", map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "done" {
		t.Fatalf("content 解析错误: %+v", res.Content)
	}
	if !strings.Contains(res.Structured, `"ok":true`) {
		t.Fatalf("structuredContent 解析错误: %q", res.Structured)
	}
	if err := cli.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestHTTPTransportJSON(t *testing.T) {
	stub := &stubMCP{}
	ts := httptest.NewServer(stub.Handler(t))
	defer ts.Close()

	cli := dialStub(t, mcp.Server{ID: 1, Transport: mcp.TransportHTTP, Endpoint: ts.URL + "/mcp"})
	runFlow(t, cli, protoV)

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stub.deleted != 1 {
		t.Fatalf("close 应发送 DELETE 结束会话，实际 %d 次", stub.deleted)
	}
}

func TestHTTPTransportSSE(t *testing.T) {
	stub := &stubMCP{sse: true}
	ts := httptest.NewServer(stub.Handler(t))
	defer ts.Close()

	cli := dialStub(t, mcp.Server{ID: 1, Transport: mcp.TransportHTTP, Endpoint: ts.URL + "/mcp"})
	runFlow(t, cli, protoV)
}

func TestHTTPTransportVersionFallback(t *testing.T) {
	// 服务端只认 2024-11-05：客户端要自动降级并成功握手
	stub := &stubMCP{rejectGT: "2024-11-05"}
	ts := httptest.NewServer(stub.Handler(t))
	defer ts.Close()

	cli := dialStub(t, mcp.Server{ID: 1, Transport: mcp.TransportHTTP, Endpoint: ts.URL + "/mcp"})
	runFlow(t, cli, "2024-11-05")
}

func TestHTTPTransportServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token"}`))
	}))
	defer ts.Close()

	cli := dialStub(t, mcp.Server{ID: 1, Transport: mcp.TransportHTTP, Endpoint: ts.URL + "/mcp"})
	defer func() { _ = cli.Close() }()

	_, err := cli.Initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("期望返回带状态码的错误，实际: %v", err)
	}
}

func TestHTTPClientProviderCache(t *testing.T) {
	p := NewHTTPClientProvider()
	s := mcp.Server{ID: 7, Transport: mcp.TransportHTTP}
	a := p.Client(s, mcp.ClientOptions{})
	b := p.Client(s, mcp.ClientOptions{})
	if a != b {
		t.Fatal("相同参数应复用同一个 http.Client")
	}
	c := p.Client(s, mcp.ClientOptions{TimeoutSec: 5})
	if a == c {
		t.Fatal("不同参数应返回不同的 http.Client")
	}
	if a.Timeout <= 0 || c.Timeout != 5*time.Second {
		t.Fatalf("超时设置不符合预期: %v / %v", a.Timeout, c.Timeout)
	}
}

// TestStdioTransport 用测试二进制自身充当子进程（reexec 惯用法），
// 覆盖「子进程 + 换行分隔 JSON + 进程退出唤醒等待者」这条链路。
func TestStdioTransport(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		helperStdioMain() // 不会返回：stdin EOF 后直接退出
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试二进制路径: %v", err)
	}
	srv := mcp.Server{
		ID:        1,
		Transport: mcp.TransportStdio,
		Command:   exe,
		Args:      []string{"-test.run=TestHelperStdioProcess", "--"},
		Env:       map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
	}
	cli, err := NewDialer().Dial(context.Background(), srv, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	runFlow(t, cli, protoV)
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestHelperStdioProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("仅作为 stdio 子进程运行")
	}
	helperStdioMain()
	// 直接退出，避免 testing 向 stdout 打印 PASS 污染 JSON 流
	os.Exit(0)
}

func helperStdioMain() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": protoV,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "stub", "version": "9.9.9"},
			}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			schema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
			result = map[string]any{"tools": []stubTool{
				{Name: "alpha", Description: "工具 A", InputSchema: schema},
				{Name: "beta", Description: "工具 B", InputSchema: schema},
				{Name: "gamma", Description: "工具 C", InputSchema: schema},
			}}
		case "tools/call":
			result = map[string]any{
				"content":           []map[string]any{{"type": "text", "text": "done"}},
				"structuredContent": map[string]any{"ok": true},
			}
		default:
			continue
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		w.Write(raw)
		w.WriteByte('\n')
		w.Flush()
	}
}

func TestIsScript(t *testing.T) {
	// Windows 下 .cmd/.bat 需要 cmd /c 包装（npm 全局包的垫片都是 .cmd）
	if !isScript(`C:\npm\global\mcp-server.cmd`) || !isScript(`run.bat`) || isScript(`node.exe`) {
		t.Fatal("isScript 判断不符合预期")
	}
}
