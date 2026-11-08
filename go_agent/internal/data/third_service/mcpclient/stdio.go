package mcpclient

// stdio 传输：启动子进程，按换行分隔的 JSON 收发。
//
// 进程生命周期与请求 ctx 解耦（不用 exec.CommandContext）：
// 一次 tools/list 超时不应该把常驻的 MCP 子进程杀掉，
// 关闭统一走 Close（先关 stdin 触发优雅退出，超时再 Kill）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"go_agent/internal/biz/mcp"
)

// exitTimeout 优雅退出等待时间，超时后强杀
const exitTimeout = 3 * time.Second

type stdioClient struct {
	*base
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	stderr *tailBuffer

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan rpcMessage

	done    chan struct{}
	waitErr error
}

func newStdioClient(s mcp.Server) (*stdioClient, error) {
	name := strings.TrimSpace(s.Command)
	if name == "" {
		return nil, fmt.Errorf("stdio 模式缺少 command")
	}
	// 执行前二次校验：库里可能已有历史数据，或有人绕过 API 直接写库。
	// 策略默认拒绝（见 mcp.ValidateStdio），所以这里不是「重复校验」而是兜底。
	if err := mcp.ValidateStdio(name, s.Args, s.Env); err != nil {
		return nil, fmt.Errorf("stdio 命令被策略拒绝: %w", err)
	}
	args := append([]string(nil), s.Args...)
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" && isScript(name) {
		// .cmd/.bat 必须经 cmd.exe 执行，否则 exec 直接报错
		cmd = exec.Command("cmd", append([]string{"/c", name}, args...)...)
	} else {
		cmd = exec.Command(name, args...)
	}
	cmd.Env = mergeEnv(s.Env)

	stderr := &tailBuffer{limit: 8 << 10}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdio 管道（stdin）: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdio 管道（stdout）: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 进程 %s: %w", name, err)
	}

	c := &stdioClient{
		cmd:     cmd,
		stdin:   stdin,
		reader:  bufio.NewReaderSize(stdout, 64<<10),
		stderr:  stderr,
		pending: make(map[int64]chan rpcMessage),
		done:    make(chan struct{}),
	}
	c.base = newBase(c)
	go c.readLoop()
	return c, nil
}

// readLoop 读响应并按 id 分发；收到 EOF 说明进程退出，唤醒所有等待者
func (c *stdioClient) readLoop() {
	defer close(c.done)
	for {
		line, err := c.reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var msg rpcMessage
			if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err == nil && msg.ID != nil {
				c.mu.Lock()
				ch, ok := c.pending[*msg.ID]
				c.mu.Unlock()
				if ok {
					select {
					case ch <- msg:
					default:
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") {
				c.stderr.WriteString("\n[read] " + err.Error())
			}
			return
		}
	}
}

func (c *stdioClient) roundTrip(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	ch := make(chan rpcMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	msg := rpcMessage{JSONRPC: "2.0", ID: &id, Method: method}
	if params != nil {
		msg.Params = params
	}
	if err := c.send(msg); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, c.exitErr()
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-c.done:
		return nil, c.exitErr()
	}
}

func (c *stdioClient) notify(ctx context.Context, method string, params any) error {
	msg := rpcMessage{JSONRPC: "2.0", Method: method}
	if params != nil {
		msg.Params = params
	}
	return c.send(msg)
}

func (c *stdioClient) send(msg rpcMessage) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(raw); err != nil {
		return fmt.Errorf("写入 MCP 进程失败: %w（%s）", err, c.stderr.Tail(400))
	}
	return nil
}

func (c *stdioClient) close() error {
	_ = c.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(exitTimeout):
		_ = c.cmd.Process.Kill()
		<-done
		return fmt.Errorf("MCP 进程未在 %s 内退出，已强杀", exitTimeout)
	}
}

// exitErr 进程退出时的可读错误（带 stderr 尾部）
func (c *stdioClient) exitErr() error {
	msg := c.stderr.Tail(800)
	if msg == "" {
		return fmt.Errorf("MCP 进程已退出")
	}
	return fmt.Errorf("MCP 进程已退出: %s", msg)
}

// isScript 是否为需要 cmd.exe 转发的批处理脚本
func isScript(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat")
}

// mergeEnv 当前进程环境 + 配置的 env（后者覆盖同名变量）
func mergeEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return os.Environ()
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			key = kv[:i]
		}
		if _, ok := extra[key]; ok {
			continue // 交给 extra 覆盖
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+os.ExpandEnv(v))
	}
	return out
}

// tailBuffer 保留 stderr 末尾若干字节（子进程不会因为 stderr 写满而阻塞）
type tailBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if b.buf.Len() > b.limit {
		trimmed := b.buf.Bytes()[b.buf.Len()-b.limit:]
		next := make([]byte, len(trimmed))
		copy(next, trimmed)
		b.buf.Reset()
		b.buf.Write(next)
	}
	return len(p), nil
}

func (b *tailBuffer) WriteString(s string) {
	_, _ = b.Write([]byte(s))
}

func (b *tailBuffer) Tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw := strings.TrimSpace(b.buf.String())
	if len(raw) <= n {
		return raw
	}
	return raw[len(raw)-n:]
}
