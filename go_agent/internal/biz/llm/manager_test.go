package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go_agent/internal/conf"

	"google.golang.org/protobuf/types/known/durationpb"
)

// fakeRepo 内存仓库，模拟 MySQL
type fakeRepo struct {
	mu        sync.Mutex
	providers map[string]*Provider
}

func newFakeRepo(list ...Provider) *fakeRepo {
	r := &fakeRepo{providers: map[string]*Provider{}}
	for i := range list {
		p := list[i]
		r.providers[p.Name] = &p
	}
	return r
}

func (r *fakeRepo) List(ctx context.Context) ([]Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, *p)
	}
	return out, nil
}

func (r *fakeRepo) Get(ctx context.Context, name string) (*Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[name]
	if !ok {
		return nil, ErrProviderNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *fakeRepo) Create(ctx context.Context, p *Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[p.Name]; ok {
		return ErrProviderExists
	}
	cp := *p
	r.providers[p.Name] = &cp
	return nil
}

func (r *fakeRepo) Update(ctx context.Context, p *Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[p.Name]; !ok {
		return ErrProviderNotFound
	}
	cp := *p
	r.providers[p.Name] = &cp
	return nil
}

func (r *fakeRepo) Delete(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[name]; !ok {
		return ErrProviderNotFound
	}
	delete(r.providers, name)
	return nil
}

func (r *fakeRepo) SetDefault(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[name]; !ok {
		return ErrProviderNotFound
	}
	for _, p := range r.providers {
		p.IsDefault = p.Name == name
	}
	return nil
}

func (r *fakeRepo) Stats(ctx context.Context) (Stats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Stats{Providers: int64(len(r.providers))}
	for _, p := range r.providers {
		if p.Enabled {
			st.Enabled++
		}
		if p.IsDefault {
			st.Default = p.Name
		}
	}
	return st, nil
}

func openAIServer(t *testing.T, reply string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "upstream down", status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": reply}}},
		})
	}))
}

func testConf(baseURL, model string) *conf.Data {
	return &conf.Data{Llm: &conf.Data_LLM{
		BaseURL:     baseURL,
		Model:       model,
		Temperature: 0.3,
		HttpTimeout: durationpb.New(5 * time.Second),
		ReloadTTL:   durationpb.New(time.Minute),
	}}
}

// 多数据源按优先级 + 失败自动切换
func TestChatFailover(t *testing.T) {
	bad := openAIServer(t, "", http.StatusInternalServerError)
	defer bad.Close()
	good := openAIServer(t, "hello from b", http.StatusOK)
	defer good.Close()

	repo := newFakeRepo(
		Provider{Name: "a", BaseURL: bad.URL, DefaultModel: "m1", Enabled: true,
			IsDefault: true, Priority: 100, Temperature: 0.2, TimeoutSec: 5, Scenes: []string{SceneGeneral}},
		Provider{Name: "b", BaseURL: good.URL, DefaultModel: "m2", Enabled: true,
			Priority: 10, Temperature: 0.2, TimeoutSec: 5, Scenes: []string{SceneGeneral}},
	)
	m := &Manager{repo: repo, fallback: NewTarget(Target{BaseURL: "http://127.0.0.1:1", Model: "none", Timeout: time.Second}), ttl: time.Minute}

	out, err := m.Chat(context.Background(), "sys", "hi")
	if err != nil {
		t.Fatalf("Chat 应自动切换到可用数据源, got err: %v", err)
	}
	if out != "hello from b" {
		t.Fatalf("got %q", out)
	}
}

// 场景路由：声明 rag 场景的数据源优先于默认数据源
func TestChatSceneRouting(t *testing.T) {
	ragSrv := openAIServer(t, "from rag", http.StatusOK)
	defer ragSrv.Close()
	defSrv := openAIServer(t, "from default", http.StatusOK)
	defer defSrv.Close()

	repo := newFakeRepo(
		Provider{Name: "def", BaseURL: defSrv.URL, DefaultModel: "d", Enabled: true,
			IsDefault: true, Priority: 100, TimeoutSec: 5, Scenes: []string{SceneGeneral}},
		Provider{Name: "rag-llm", BaseURL: ragSrv.URL, DefaultModel: "r", Enabled: true,
			Priority: 1, TimeoutSec: 5, Scenes: []string{SceneRAG}},
	)
	m := &Manager{repo: repo, fallback: NewTarget(Target{}), ttl: time.Minute}

	out, err := m.ChatScene(context.Background(), SceneRAG, "sys", "hi")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != "from rag" {
		t.Fatalf("应按场景路由到 rag-llm, got %q", out)
	}
	// 通用场景仍走默认数据源
	if out, _ := m.Chat(context.Background(), "sys", "hi"); out != "from default" {
		t.Fatalf("通用场景应走默认数据源, got %q", out)
	}
}

// 表为空时用 conf 播种 default
func TestNewManagerSeedsFromConf(t *testing.T) {
	repo := newFakeRepo()
	m := NewManager(testConf("https://api.example.com/v1", "m1"), repo, nil)

	list, err := repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "default" || !list[0].IsDefault {
		t.Fatalf("应播种一个 default 数据源, got %+v", list)
	}
	if m.Model() != "m1" {
		t.Fatalf("Model()=%q", m.Model())
	}
}

// 密钥脱敏
func TestListProvidersMasksKey(t *testing.T) {
	repo := newFakeRepo(Provider{Name: "a", BaseURL: "https://x", DefaultModel: "m",
		APIKey: "sk-1234567890abcd", Enabled: true})
	m := &Manager{repo: repo, fallback: NewTarget(Target{}), ttl: time.Minute}

	list, err := m.ListProviders(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].APIKey == "sk-1234567890abcd" || list[0].APIKey == "" {
		t.Fatalf("key 未脱敏: %q", list[0].APIKey)
	}
	if !list[0].HasKey {
		t.Fatal("has_key 应为 true")
	}
	if MaskKey("abc") != "****" {
		t.Fatalf("短 key 脱敏异常: %q", MaskKey("abc"))
	}
}

// 创建校验
func TestCreateProviderValidation(t *testing.T) {
	repo := newFakeRepo()
	m := &Manager{repo: repo, fallback: NewTarget(Target{}), ttl: time.Minute}
	cases := []CreateProviderInput{
		{Name: "-bad", BaseURL: "https://x", DefaultModel: "m"},
		{Name: "ok", BaseURL: "x", DefaultModel: "m"},
		{Name: "ok", BaseURL: "https://x"},
	}
	for i, in := range cases {
		if _, err := m.CreateProvider(context.Background(), in); err == nil {
			t.Fatalf("case %d 应校验失败: %+v", i, in)
		}
	}
	p, err := m.CreateProvider(context.Background(), CreateProviderInput{
		Name: "deepseek", Kind: KindDeepSeek, BaseURL: "https://api.deepseek.com/",
		APIKey: "sk-abc", Models: []string{"deepseek-chat", "deepseek-reasoner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("base_url 应去掉尾部斜杠: %q", p.BaseURL)
	}
	if p.DefaultModel != "deepseek-chat" {
		t.Fatalf("未自动填默认模型: %q", p.DefaultModel)
	}
	if p.TimeoutSec != 120 {
		t.Fatalf("未填默认超时: %d", p.TimeoutSec)
	}
}

// 不允许删除唯一启用的数据源
func TestDeleteLastEnabledProvider(t *testing.T) {
	repo := newFakeRepo(Provider{Name: "only", BaseURL: "https://x", DefaultModel: "m", Enabled: true})
	m := &Manager{repo: repo, fallback: NewTarget(Target{}), ttl: time.Minute}
	if err := m.DeleteProvider(context.Background(), "only"); err != ErrLastProvider {
		t.Fatalf("应拒绝删除唯一启用数据源, got %v", err)
	}
}

// 测试连通性返回结构化结果（失败也返回 200 结构）
func TestTestProvider(t *testing.T) {
	srv := openAIServer(t, "OK", http.StatusOK)
	defer srv.Close()
	repo := newFakeRepo(Provider{Name: "a", BaseURL: srv.URL, DefaultModel: "m", Enabled: true, TimeoutSec: 5})
	m := &Manager{repo: repo, fallback: NewTarget(Target{}), ttl: time.Minute}
	res, err := m.TestProvider(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Reply != "OK" {
		t.Fatalf("测试结果异常: %+v", res)
	}
}

// 指定数据源调用失败 → 回退默认链路（default 优先）
func TestChatSourceFallback(t *testing.T) {
	bad := openAIServer(t, "", http.StatusInternalServerError)
	defer bad.Close()
	good := openAIServer(t, "hello from default", http.StatusOK)
	defer good.Close()

	repo := newFakeRepo(
		Provider{Name: "qwen", BaseURL: bad.URL, DefaultModel: "m-q", Enabled: true,
			Priority: 200, Temperature: 0.2, TimeoutSec: 5, Scenes: []string{SceneGeneral}},
		Provider{Name: "default", BaseURL: good.URL, DefaultModel: "m-d", Enabled: true,
			IsDefault: true, Priority: 100, Temperature: 0.2, TimeoutSec: 5, Scenes: []string{SceneGeneral}},
	)
	m := &Manager{repo: repo, fallback: NewTarget(Target{BaseURL: "http://127.0.0.1:1", Model: "none", Timeout: time.Second}), ttl: time.Minute}
	ctx := context.Background()

	// 命中 qwen 但上游 500 → 回退 default
	out, err := m.ChatSource(ctx, "qwen", "sys", "hi", 0.3)
	if err != nil {
		t.Fatalf("ChatSource 应回退 default, got err: %v", err)
	}
	if out != "hello from default" {
		t.Fatalf("got %q", out)
	}

	// 数据源名不存在 → 直接走默认链路
	out, err = m.ChatSource(ctx, "no-such-source", "sys", "hi", 0.3)
	if err != nil || out != "hello from default" {
		t.Fatalf("unknown source: out=%q err=%v", out, err)
	}

	// 空数据源名 = 普通 Chat
	out, err = m.ChatSource(ctx, "", "sys", "hi", 0.3)
	if err != nil || out != "hello from default" {
		t.Fatalf("empty source: out=%q err=%v", out, err)
	}

	// 指定源可用时优先用它
	ok2 := openAIServer(t, "hello from qwen", http.StatusOK)
	defer ok2.Close()
	if err := repo.Update(ctx, &Provider{Name: "qwen", BaseURL: ok2.URL, DefaultModel: "m-q", Enabled: true,
		Priority: 200, Temperature: 0.2, TimeoutSec: 5, Scenes: []string{SceneGeneral}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	m.Invalidate()
	out, err = m.ChatSource(ctx, "qwen", "sys", "hi", 0.3)
	if err != nil || out != "hello from qwen" {
		t.Fatalf("prefer source: out=%q err=%v", out, err)
	}
}
