package mcp

// 幂等键与回放链路测试：TaskCallKey 稳定性、CallTool 命中回放 / 未命中执行 /
// 失败不缓存。回放走 proxy 传输（httptest 后端计数），不需要真的 MCP 连接。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskCallKeyStable(t *testing.T) {
	// 同一参数（分别构造）必须得到同一个 key：encoding/json 对 map 按 key 排序
	a := map[string]any{"id": "u1", "title": "发布", "count": 3}
	b := map[string]any{"count": 3, "title": "发布", "id": "u1"}
	k1 := TaskCallKey(7, 3, "default__create_issue", a)
	k2 := TaskCallKey(7, 3, "default__create_issue", b)
	require.Equal(t, k1, k2)

	// 参数 / 轮次 / 任务 / 工具任一不同 → 不同 key
	require.NotEqual(t, k1, TaskCallKey(7, 3, "default__create_issue", map[string]any{"id": "u2", "title": "发布", "count": 3}))
	require.NotEqual(t, k1, TaskCallKey(7, 4, "default__create_issue", a))
	require.NotEqual(t, k1, TaskCallKey(8, 3, "default__create_issue", a))
	require.NotEqual(t, k1, TaskCallKey(7, 3, "default__close_issue", a))

	// key 形态：task 前缀可反解出任务 ID（观测列用）
	require.Contains(t, k1, "task:7:turn:3:default__create_issue:")
	require.Equal(t, uint64(7), taskIDFromIdemKey(k1))
	require.Equal(t, uint64(0), taskIDFromIdemKey("not-a-key"))

	// WithIdemKey / IdemKeyFrom 往返；空键等于不带
	ctx := WithIdemKey(context.Background(), "k1")
	require.Equal(t, "k1", IdemKeyFrom(ctx))
	// 空键不覆盖：WithIdemKey 原样返回，已有 key 保持不变
	require.Equal(t, "", IdemKeyFrom(WithIdemKey(context.Background(), "")))
	require.Equal(t, "", IdemKeyFrom(context.Background()))
}

// fakeIdemRepo 只实现 CallTool 链路用到的方法，其余内嵌 nil 接口（误用即 panic）
type fakeIdemRepo struct {
	Repo
	server Server
	tool   Tool

	mu      sync.Mutex
	records map[string]IdemRecord
}

func newFakeIdemRepo(s Server, tool Tool) *fakeIdemRepo {
	return &fakeIdemRepo{server: s, tool: tool, records: map[string]IdemRecord{}}
}

func (r *fakeIdemRepo) GetToolByUniqueName(_ context.Context, name string) (*Tool, error) {
	t := r.tool
	return &t, nil
}

func (r *fakeIdemRepo) GetServer(_ context.Context, _ uint64) (*Server, error) {
	s := r.server
	return &s, nil
}

func (r *fakeIdemRepo) GetCallRecord(_ context.Context, key string) (*IdemRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.records[key]; ok {
		copied := rec
		if copied.Result != nil {
			res := *copied.Result
			copied.Result = &res
		}
		return &copied, nil
	}
	return nil, nil
}

func (r *fakeIdemRepo) SaveCallRecord(_ context.Context, rec *IdemRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := *rec
	if rec.Result != nil {
		res := *rec.Result
		copied.Result = &res
	}
	r.records[rec.IdemKey] = copied
	return nil
}

type fakeStatusCache struct {
	Cache
	mu sync.Mutex
	st map[uint64]Status
}

func (c *fakeStatusCache) GetStatus(_ context.Context, id uint64) (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.st[id]
	return st, ok
}

func (c *fakeStatusCache) PutStatus(_ context.Context, st Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st == nil {
		c.st = map[uint64]Status{}
	}
	c.st[st.ServerID] = st
}

type noopDialer struct{ Dialer }
type nilVec struct{ VectorStore }
type nilEmb struct{}

func (noopDialer) Dial(context.Context, Server, *http.Client) (Client, error) { panic("不应建连") }
func (nilVec) Embed(context.Context, []string) ([][]float32, error)           { panic("不应向量化") }
func (nilVec) Dim() int                                                       { return 0 }
func (nilEmb) Embed(context.Context, []string) ([][]float32, error)           { panic("不应向量化") }
func (nilEmb) Dim() int                                                       { return 0 }

// newIdemUseCase 装配一个走 proxy 传输的 UseCase（后端由测试传入）
func newIdemUseCase(t *testing.T, s Server, tool Tool) *UseCase {
	t.Helper()
	u, err := New(newFakeIdemRepo(s, tool), nilVec{}, &fakeStatusCache{}, noopDialer{}, nilClientProvider{}, nilEmb{}, nil)
	require.NoError(t, err)
	return u
}

type nilClientProvider struct{ ClientProvider }

func (nilClientProvider) Client(Server, ClientOptions) *http.Client { panic("proxy 不用") }

func TestCallToolIdemReplay(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"exec":` + itoa(n) + `}`))
	}))
	t.Cleanup(srv.Close)

	e := proxyServerFor(t, srv.URL, "")
	e.Enabled = true // proxyServerFor 不设 Enabled，CallTool 会拦停用的服务
	op := ProxyOp{Name: "create_issue", Method: "POST", Path: "/issues"}
	tool := Tool{ToolName: "create_issue", UniqueName: "default__create_issue", Enabled: true, Route: op.RouteJSON("json")}
	u := newIdemUseCase(t, e, tool)

	key := TaskCallKey(42, 1, "default__create_issue", map[string]any{"title": "上线"})
	ctx := WithIdemKey(context.Background(), key)

	// 第一次：真正执行
	r1, err := u.CallTool(ctx, 0, "default__create_issue", map[string]any{"title": "上线"})
	require.NoError(t, err)
	require.False(t, r1.Replayed)
	mu.Lock()
	require.Equal(t, 1, hits)
	mu.Unlock()

	// 第二次（同 key 同参数）：命中回放，后端不再被调用
	r2, err := u.CallTool(ctx, 0, "default__create_issue", map[string]any{"title": "上线"})
	require.NoError(t, err)
	require.True(t, r2.Replayed)
	require.Equal(t, r1.Structured, r2.Structured)
	mu.Lock()
	require.Equal(t, 1, hits)
	mu.Unlock()

	// 不同 key：照常执行
	_, err = u.CallTool(WithIdemKey(context.Background(), TaskCallKey(42, 2, "default__create_issue", map[string]any{"title": "上线"})),
		0, "default__create_issue", map[string]any{"title": "上线"})
	require.NoError(t, err)
	mu.Lock()
	require.Equal(t, 2, hits)
	mu.Unlock()

	// 不带 key（管理界面手动调用）：不去重
	_, err = u.CallTool(context.Background(), 0, "default__create_issue", map[string]any{"title": "上线"})
	require.NoError(t, err)
	mu.Lock()
	require.Equal(t, 3, hits)
	mu.Unlock()
}

func TestCallToolIdemFailureNotCached(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		current := n
		mu.Unlock()
		if current == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	e := proxyServerFor(t, srv.URL, "")
	e.Enabled = true
	op := ProxyOp{Name: "create_issue", Method: "POST", Path: "/issues"}
	tool := Tool{ToolName: "create_issue", UniqueName: "default__create_issue", Enabled: true, Route: op.RouteJSON("json")}
	u := newIdemUseCase(t, e, tool)

	key := TaskCallKey(9, 0, "default__create_issue", map[string]any{"title": "x"})
	ctx := WithIdemKey(context.Background(), key)

	// 第一次失败（后端 500，代理层以 IsError 结果呈现）：不缓存
	r1, err := u.CallTool(ctx, 0, "default__create_issue", map[string]any{"title": "x"})
	require.NoError(t, err)
	require.True(t, r1.IsError)
	mu.Lock()
	require.Equal(t, 1, n)
	mu.Unlock()

	// 同 key 重试：IsError 不缓存，真正再执行（这次成功）
	r2, err := u.CallTool(ctx, 0, "default__create_issue", map[string]any{"title": "x"})
	require.NoError(t, err)
	require.False(t, r2.IsError)
	require.False(t, r2.Replayed)
	mu.Lock()
	require.Equal(t, 2, n)
	mu.Unlock()

	// 成功之后才进缓存
	r3, err := u.CallTool(ctx, 0, "default__create_issue", map[string]any{"title": "x"})
	require.NoError(t, err)
	require.True(t, r3.Replayed)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
