package mcpclient

// Streamable HTTP 传输：
//   - 每个请求一次 POST，Accept 同时声明 application/json 与 text/event-stream；
//   - 响应可能是普通 JSON，也可能是 SSE（data: {...} 事件流），两种都支持；
//   - 会话靠响应头 Mcp-Session-Id 维持，后续请求带同名字段；
//   - 握手后带上 MCP-Protocol-Version，老服务端不认也不会报错（多数实现忽略）。
//
// 使用的 *http.Client 由外部注入，因此超时/代理/TLS/连接池按实例独立。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go_agent/internal/biz/mcp"
)

const maxBodyBytes = 8 << 20

type httpTransport struct {
	*base
	endpoint string
	headers  map[string]string
	hc       *http.Client

	mu        sync.Mutex
	sessionID string
}

func newHTTPTransport(s mcp.Server, hc *http.Client) (*httpTransport, error) {
	if strings.TrimSpace(s.Endpoint) == "" {
		return nil, fmt.Errorf("http 模式缺少 endpoint")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	t := &httpTransport{
		endpoint: s.Endpoint,
		headers:  s.Headers,
		hc:       hc,
	}
	t.base = newBase(t)
	return t, nil
}

func (c *httpTransport) roundTrip(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	msg := rpcMessage{JSONRPC: "2.0", ID: &id, Method: method}
	if params != nil {
		msg.Params = params
	}
	resp, err := c.post(ctx, msg)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("MCP 服务对 %s 返回空响应（检查 endpoint 是否为 Streamable HTTP 入口）", method)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

func (c *httpTransport) notify(ctx context.Context, method string, params any) error {
	msg := rpcMessage{JSONRPC: "2.0", Method: method}
	if params != nil {
		msg.Params = params
	}
	_, err := c.post(ctx, msg)
	return err
}

func (c *httpTransport) post(ctx context.Context, msg rpcMessage) (*rpcMessage, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造 MCP 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, os.ExpandEnv(v))
	}
	if sid := c.session(); sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if pv := c.base.ProtocolVersion(); pv != "" {
		req.Header.Set("MCP-Protocol-Version", pv)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 MCP 服务 %s 失败: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("MCP 服务返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case strings.Contains(ct, "text/event-stream"):
		return readSSE(resp.Body, msg.ID)
	case strings.Contains(ct, "application/json"):
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if err != nil {
			return nil, fmt.Errorf("读取 MCP 响应: %w", err)
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return nil, nil
		}
		var out rpcMessage
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("解析 MCP 响应: %w（%s）", err, truncate(string(body), 200))
		}
		return &out, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(bytes.TrimSpace(body)) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("MCP 服务返回非预期 Content-Type %q: %s", ct, truncate(string(body), 200))
	}
}

func (c *httpTransport) session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *httpTransport) close() error {
	// 主动结束会话（服务端可选择忽略）：失败不影响关闭流程
	if sid := c.session(); sid != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", sid)
			for k, v := range c.headers {
				req.Header.Set(k, os.ExpandEnv(v))
			}
			if resp, derr := c.hc.Do(req); derr == nil {
				_ = resp.Body.Close()
			}
		}
	}
	return nil
}

// readSSE 从事件流里取出第一条匹配 id 的消息（id 为 nil 表示通知，取第一条）
func readSSE(r io.Reader, id *int64) (*rpcMessage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxBodyBytes)
	var data bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				if msg := tryDecodeSSEMessage(data.Bytes(), id); msg != nil {
					return msg, nil
				}
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			data.WriteString(strings.TrimPrefix(payload, " "))
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 SSE 流: %w", err)
	}
	if data.Len() > 0 {
		if msg := tryDecodeSSEMessage(data.Bytes(), id); msg != nil {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("SSE 流结束但未收到匹配的响应")
}

func tryDecodeSSEMessage(raw []byte, id *int64) *rpcMessage {
	var msg rpcMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &msg); err != nil {
		return nil
	}
	if id == nil {
		return &msg
	}
	if msg.ID != nil && *msg.ID == *id {
		return &msg
	}
	if msg.Error != nil && msg.ID == nil {
		return &msg // 服务端级错误（无 id）
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
