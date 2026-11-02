// Package mcpclient 实现 MCP（Model Context Protocol）客户端：
// JSON-RPC 2.0 之上的 initialize / tools/list / tools/call / ping。
//
// 两种传输：
//   - stdio：子进程 + 换行分隔的 JSON（MCP stdio 传输规范）；
//   - http ：Streamable HTTP，一次 POST 一个请求，响应可能是 application/json
//     或 text/event-stream（SSE），会话靠 Mcp-Session-Id 维持。
//
// http 模式的 *http.Client 由外部注入（见 internal/biz/mcp 的 ClientProvider），
// 因此超时、代理、TLS、连接池都可以按实例定制，且不落库。
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go_agent/internal/biz/mcp"
)

// 协议版本：先试最新，服务端不接受再退到较老的版本
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const (
	clientName    = "go-agent"
	clientVersion = "1.0.0"
	maxTools      = 500
	maxPages      = 20
)

// rpcError JSON-RPC 错误
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("MCP 错误 %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("MCP 错误 %d: %s", e.Code, e.Message)
}

// rpcMessage 请求/响应/通知统一信封
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// transport 传输层：只负责一问一答，协议语义在 base 里
type transport interface {
	// roundTrip 发送一次请求并等待对应 id 的响应
	roundTrip(ctx context.Context, id int64, method string, params any) (json.RawMessage, error)
	// notify 发送通知（无响应），如 notifications/initialized
	notify(ctx context.Context, method string, params any) error
	close() error
}

// base 协议层：握手、工具列表、工具调用、探活
type base struct {
	tr  transport
	seq atomic.Int64

	initMu      sync.Mutex
	initialized bool
	initErr     error
	info        mcp.ServerInfo

	// verMu 只保护 version：init 期间 http 传输会回读协议版本，
	// 若与 initMu 共用一把不可重入的锁会直接自锁。
	verMu   sync.RWMutex
	version string

	// callMu 串行化调用：MCP 服务端多为单线程处理，并发请求收益低、排障成本高
	callMu sync.Mutex
}

func newBase(tr transport) *base { return &base{tr: tr} }

// Initialize 握手：initialize + notifications/initialized
func (b *base) Initialize(ctx context.Context) (mcp.ServerInfo, error) {
	return b.init(ctx)
}

func (b *base) init(ctx context.Context) (mcp.ServerInfo, error) {
	b.initMu.Lock()
	defer b.initMu.Unlock()
	if b.initialized {
		return b.info, nil
	}
	if b.initErr != nil {
		return mcp.ServerInfo{}, b.initErr
	}

	var lastErr error
	for _, v := range protocolVersions {
		params := map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
		}
		raw, err := b.roundTrip(ctx, "initialize", params)
		if err != nil {
			lastErr = err
			// 版本不匹配时服务端返回 -32602，换一个版本再试
			if isVersionMismatch(err) {
				continue
			}
			b.initErr = fmt.Errorf("MCP 初始化失败: %w", err)
			return mcp.ServerInfo{}, b.initErr
		}
		var res struct {
			ProtocolVersion string `json:"protocolVersion"`
			Instructions    string `json:"instructions"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		}
		_ = json.Unmarshal(raw, &res)
		ver := res.ProtocolVersion
		if ver == "" {
			ver = v
		}
		b.setVersion(ver)
		b.info = mcp.ServerInfo{
			Name:            res.ServerInfo.Name,
			Version:         res.ServerInfo.Version,
			ProtocolVersion: ver,
			Instructions:    res.Instructions,
		}
		// 通知失败不影响后续请求（部分实现会返回 202 空响应）
		_ = b.tr.notify(ctx, "notifications/initialized", map[string]any{})
		b.initialized = true
		return b.info, nil
	}
	b.initErr = fmt.Errorf("MCP 初始化失败（协议版本不兼容）: %w", lastErr)
	return mcp.ServerInfo{}, b.initErr
}

// ProtocolVersion 握手协商出的协议版本（供 http 传输带上 MCP-Protocol-Version 头）
func (b *base) ProtocolVersion() string {
	b.verMu.RLock()
	defer b.verMu.RUnlock()
	return b.version
}

func (b *base) setVersion(v string) {
	b.verMu.Lock()
	b.version = v
	b.verMu.Unlock()
}

// ListTools 拉取工具清单（自动翻页）
func (b *base) ListTools(ctx context.Context) ([]mcp.RemoteTool, error) {
	if _, err := b.init(ctx); err != nil {
		return nil, err
	}
	b.callMu.Lock()
	defer b.callMu.Unlock()

	out := make([]mcp.RemoteTool, 0, 16)
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := b.tr.roundTrip(ctx, b.nextID(), "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list 失败: %w", err)
		}
		var res struct {
			Tools []struct {
				Name         string          `json:"name"`
				Title        string          `json:"title"`
				Description  string          `json:"description"`
				InputSchema  json.RawMessage `json:"inputSchema"`
				OutputSchema json.RawMessage `json:"outputSchema"`
				Annotations  json.RawMessage `json:"annotations"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("tools/list 响应解析失败: %w", err)
		}
		for _, t := range res.Tools {
			name := strings.TrimSpace(t.Name)
			if name == "" {
				continue
			}
			title := strings.TrimSpace(t.Title)
			if title == "" {
				title = name
			}
			out = append(out, mcp.RemoteTool{
				Name:         name,
				Title:        title,
				Description:  strings.TrimSpace(t.Description),
				InputSchema:  t.InputSchema,
				OutputSchema: t.OutputSchema,
				Annotations:  t.Annotations,
			})
		}
		if res.NextCursor == "" || res.NextCursor == cursor || len(out) >= maxTools {
			break
		}
		cursor = res.NextCursor
	}
	if len(out) > maxTools {
		out = out[:maxTools]
	}
	return out, nil
}

// CallTool 调用工具
func (b *base) CallTool(ctx context.Context, name string, args map[string]any) (mcp.CallResult, error) {
	if _, err := b.init(ctx); err != nil {
		return mcp.CallResult{}, err
	}
	if strings.TrimSpace(name) == "" {
		return mcp.CallResult{}, fmt.Errorf("工具名为空")
	}
	if args == nil {
		args = map[string]any{}
	}
	b.callMu.Lock()
	defer b.callMu.Unlock()

	start := time.Now()
	raw, err := b.tr.roundTrip(ctx, b.nextID(), "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return mcp.CallResult{ElapsedMs: elapsed}, fmt.Errorf("tools/call 失败: %w", err)
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return mcp.CallResult{ElapsedMs: elapsed}, fmt.Errorf("tools/call 响应解析失败: %w", err)
	}
	out := mcp.CallResult{IsError: res.IsError, ElapsedMs: elapsed}
	for _, c := range res.Content {
		part := mcp.ContentPart{Type: c.Type, Text: c.Text}
		if c.Data != "" || c.MimeType != "" {
			b, _ := json.Marshal(map[string]string{"data": c.Data, "mimeType": c.MimeType})
			part.Data = string(b)
		}
		out.Content = append(out.Content, part)
	}
	if len(res.StructuredContent) > 0 {
		out.Structured = string(res.StructuredContent)
	}
	return out, nil
}

// Ping 探活（MCP ping 方法）
func (b *base) Ping(ctx context.Context) error {
	if _, err := b.init(ctx); err != nil {
		return err
	}
	b.callMu.Lock()
	defer b.callMu.Unlock()
	_, err := b.tr.roundTrip(ctx, b.nextID(), "ping", nil)
	return err
}

// Close 关闭底层传输（stdio 进程 / http 会话）
func (b *base) Close() error { return b.tr.close() }

func (b *base) nextID() int64 { return b.seq.Add(1) }

// roundTrip 发请求并解包结果：JSON-RPC 的 error 字段转成 Go error
func (b *base) roundTrip(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return b.tr.roundTrip(ctx, b.nextID(), method, params)
}

func isVersionMismatch(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "protocol version") ||
		strings.Contains(msg, "unsupported protocol") ||
		strings.Contains(msg, "-32602")
}
