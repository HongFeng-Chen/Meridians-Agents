package aiplatform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/skill"
)

// —— fakes ——

type fakeRepo struct {
	items   map[string]*Platform
	deleted []string
}

func newFakeRepo() *fakeRepo { return &fakeRepo{items: map[string]*Platform{}} }

func (r *fakeRepo) List(_ context.Context, f Filter) ([]Platform, error) {
	out := make([]Platform, 0, len(r.items))
	for _, p := range r.items {
		if f.Keyword != "" && !strings.Contains(p.Name, f.Keyword) {
			continue
		}
		if f.Enabled != nil && p.Enabled != *f.Enabled {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (r *fakeRepo) Get(_ context.Context, name string) (*Platform, error) {
	p, ok := r.items[name]
	if !ok {
		return nil, ErrPlatformNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *fakeRepo) Save(_ context.Context, p *Platform) error {
	cp := *p
	r.items[p.Name] = &cp
	return nil
}

func (r *fakeRepo) Delete(_ context.Context, name string) error {
	if _, ok := r.items[name]; !ok {
		return ErrPlatformNotFound
	}
	delete(r.items, name)
	r.deleted = append(r.deleted, name)
	return nil
}

func (r *fakeRepo) SetDefault(_ context.Context, name string) error {
	p, ok := r.items[name]
	if !ok {
		return ErrPlatformNotFound
	}
	for _, other := range r.items {
		other.IsDefault = false
	}
	p.IsDefault = true
	return nil
}

type fakeCatalog struct {
	items []RemoteMCP
	err   error
	calls int
}

func (c *fakeCatalog) Search(_ context.Context, _ Platform, _ string, _ int) ([]RemoteMCP, error) {
	c.calls++
	return c.items, c.err
}

type fakeMCP struct {
	servers   []mcp.Server
	created   []mcp.ServerInput
	deleted   []uint64
	connected []uint64
	synced    []uint64
	nextID    uint64

	connectErr error
	syncErr    error
	fetched    int
}

func (m *fakeMCP) ListServers(_ context.Context, f mcp.ServerFilter) ([]mcp.Server, error) {
	out := make([]mcp.Server, 0, len(m.servers))
	for _, s := range m.servers {
		if f.Keyword != "" && !strings.Contains(s.Name, f.Keyword) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *fakeMCP) CreateServer(_ context.Context, in mcp.ServerInput) (*mcp.Server, error) {
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	m.nextID++
	m.created = append(m.created, in)
	s := mcp.Server{ID: m.nextID, Name: in.Name, Namespace: in.Namespace, Transport: in.Transport, Endpoint: in.Endpoint, Headers: in.Headers, Enabled: true}
	m.servers = append(m.servers, s)
	return &s, nil
}

func (m *fakeMCP) DeleteServer(_ context.Context, id uint64) error {
	m.deleted = append(m.deleted, id)
	return nil
}

func (m *fakeMCP) ConnectServer(_ context.Context, id uint64) (mcp.Status, error) {
	m.connected = append(m.connected, id)
	if m.connectErr != nil {
		return mcp.Status{ServerID: id, Running: false}, m.connectErr
	}
	return mcp.Status{ServerID: id, Running: true}, nil
}

func (m *fakeMCP) SyncTools(_ context.Context, id uint64) (*mcp.SyncResult, error) {
	m.synced = append(m.synced, id)
	if m.syncErr != nil {
		return nil, m.syncErr
	}
	return &mcp.SyncResult{ServerID: id, Fetched: m.fetched}, nil
}

type fakeHubs struct {
	hubs    []skill.Hub
	created []skill.HubInput
	patched []string
}

func (h *fakeHubs) ListHubs(_ context.Context) ([]skill.Hub, error) { return h.hubs, nil }

func (h *fakeHubs) CreateHub(_ context.Context, in skill.HubInput) (*skill.Hub, error) {
	h.created = append(h.created, in)
	return &skill.Hub{Name: in.Name, BaseURL: in.BaseURL}, nil
}

func (h *fakeHubs) UpdateHub(_ context.Context, name string, _ skill.HubPatch) (*skill.Hub, error) {
	h.patched = append(h.patched, name)
	return &skill.Hub{Name: name}, nil
}

func newUC() (*UseCase, *fakeRepo, *fakeCatalog, *fakeMCP, *fakeHubs) {
	repo := newFakeRepo()
	cat := &fakeCatalog{}
	m := &fakeMCP{}
	h := &fakeHubs{}
	return New(repo, cat, m, h), repo, cat, m, h
}

func qwenPlatform(t *testing.T) *Platform {
	t.Helper()
	kind := KindQwen
	in := Input{Name: "qwen", Kind: kind}
	if err := in.Normalize(true); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	p := &Platform{Name: in.Name}
	applyInput(p, &in)
	p.Enabled = true
	return p
}

// —— 用例 ——

func TestNormalizeAppliesQwenDefaults(t *testing.T) {
	in := Input{Name: "qwen", Kind: KindQwen}
	if err := in.Normalize(true); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got := deref(in.BaseURL); got != QwenBaseURL {
		t.Fatalf("base_url = %q, want %q", got, QwenBaseURL)
	}
	if got := deref(in.StreamablePath); got != QwenStreamablePath {
		t.Fatalf("streamable_path = %q", got)
	}
	if got := deref(in.CatalogPath); got != QwenCatalogPath {
		t.Fatalf("catalog_path = %q", got)
	}
	if got := deref(in.AuthScheme); got != AuthBearer {
		t.Fatalf("auth_scheme = %q", got)
	}
	if got := deref(in.SSEPath); got != QwenSSEPath {
		t.Fatalf("sse_path = %q", got)
	}
}

// kind 省略时按名称推断，保证「name=qwen + api_key 即可」这条捷径成立
func TestNormalizeInfersKindFromName(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		input   Input
		wantURL string
	}{
		{"qwen", KindQwen, Input{Name: "qwen"}, QwenBaseURL},
		{"bailian", KindBailian, Input{Name: "bailian"}, QwenBaseURL},
		{"my-gateway", KindGeneric, Input{Name: "my-gateway", BaseURL: strp("https://gw.internal")}, "https://gw.internal"},
	}
	for _, c := range cases {
		in := c.input
		if err := in.Normalize(true); err != nil {
			t.Fatalf("%s: Normalize: %v", c.name, err)
		}
		if in.Kind != c.kind {
			t.Fatalf("%s: kind = %q, want %q", c.name, in.Kind, c.kind)
		}
		if got := deref(in.BaseURL); got != c.wantURL {
			t.Fatalf("%s: base_url = %q, want %q", c.name, got, c.wantURL)
		}
	}
}

func TestNormalizeRejectsBadNameAndPath(t *testing.T) {
	bad := []Input{
		{Name: "Qwen 平台", Kind: KindQwen},
		{Name: "qwen", Kind: "unknown"},
		{Name: "qwen", Kind: KindQwen, StreamablePath: strp("/mcps/{id}/mcp")},
	}
	for i, in := range bad {
		if err := in.Normalize(true); err == nil {
			t.Fatalf("case %d: 期望报错，实际通过", i)
		}
	}
	// 非 qwen 类型没有默认 base_url，必须显式给
	generic := Input{Name: "internal", Kind: KindGeneric}
	if err := generic.Normalize(true); err == nil {
		t.Fatal("generic 平台缺 base_url 应报错")
	}
}

func TestResolveEndpointAndAuthHeaders(t *testing.T) {
	p := qwenPlatform(t)
	p.APIKey = "sk-test"
	p.AuthScheme = AuthBearer
	p.Headers = map[string]string{"X-Tenant": "t1"}

	ep, err := p.ResolveEndpoint("WebSearch", mcp.TransportHTTP)
	if err != nil {
		t.Fatalf("ResolveEndpoint: %v", err)
	}
	if ep != "https://dashscope.aliyuncs.com/api/v1/mcps/WebSearch/mcp" {
		t.Fatalf("endpoint = %q", ep)
	}
	sse, err := p.ResolveEndpoint("amap-maps", TransportSSE)
	if err != nil || !strings.HasSuffix(sse, "/amap-maps/sse") {
		t.Fatalf("sse endpoint = %q, err=%v", sse, err)
	}

	h := p.AuthHeaders()
	if h["Authorization"] != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", h["Authorization"])
	}
	if h["X-Tenant"] != "t1" {
		t.Fatalf("附加头丢失: %v", h)
	}

	// header 模式
	p.AuthScheme = AuthHeader
	p.AuthHeader = "X-Qwen-Key"
	h = p.AuthHeaders()
	if h["X-Qwen-Key"] != "sk-test" {
		t.Fatalf("自定义鉴权头 = %v", h)
	}
}

func TestImportMCPCreatesAndSyncs(t *testing.T) {
	uc, repo, _, m, _ := newUC()
	if err := repo.Save(context.Background(), qwenPlatform(t)); err != nil {
		t.Fatal(err)
	}
	repo.items["qwen"].APIKey = "sk-test"
	m.fetched = 3

	res, err := uc.ImportMCP(context.Background(), ImportInput{Code: "WebSearch", Namespace: ""})
	if err != nil {
		t.Fatalf("ImportMCP: %v", err)
	}
	if len(m.created) != 1 {
		t.Fatalf("created = %d", len(m.created))
	}
	in := m.created[0]
	if in.Name != "qwen-websearch" {
		t.Fatalf("server name = %q", in.Name)
	}
	if in.Namespace != "qwen" {
		t.Fatalf("namespace = %q（默认应取平台名）", in.Namespace)
	}
	if in.Endpoint != "https://dashscope.aliyuncs.com/api/v1/mcps/WebSearch/mcp" {
		t.Fatalf("endpoint = %q", in.Endpoint)
	}
	if in.Headers["Authorization"] != "Bearer sk-test" {
		t.Fatalf("headers = %v", in.Headers)
	}
	if res.ToolCount != 3 || res.ServerID == 0 || res.Reused {
		t.Fatalf("result = %+v", res)
	}
}

func TestImportMCPRollsBackWhenConnectFails(t *testing.T) {
	uc, repo, _, m, _ := newUC()
	_ = repo.Save(context.Background(), qwenPlatform(t))
	m.connectErr = errors.New("401 unauthorized")

	_, err := uc.ImportMCP(context.Background(), ImportInput{Code: "WebSearch"})
	if err == nil || !errors.Is(err, ErrImportFailed) {
		t.Fatalf("err = %v，期望 ErrImportFailed", err)
	}
	if len(m.deleted) != 1 {
		t.Fatalf("连接失败应回滚删除服务，deleted=%v", m.deleted)
	}
}

func TestImportMCPReusesExisting(t *testing.T) {
	uc, repo, _, m, _ := newUC()
	_ = repo.Save(context.Background(), qwenPlatform(t))
	m.servers = []mcp.Server{{ID: 9, Name: "qwen-websearch", Namespace: "qwen", Transport: mcp.TransportHTTP}}
	m.fetched = 2

	res, err := uc.ImportMCP(context.Background(), ImportInput{Code: "WebSearch"})
	if err != nil {
		t.Fatalf("ImportMCP: %v", err)
	}
	if !res.Reused || res.ServerID != 9 || len(m.created) != 0 {
		t.Fatalf("应复用已有服务，result=%+v created=%d", res, len(m.created))
	}
	if res.ToolCount != 2 {
		t.Fatalf("tool count = %d", res.ToolCount)
	}
}

func TestImportMCPSSEFallsBackToHTTP(t *testing.T) {
	uc, repo, _, m, _ := newUC()
	_ = repo.Save(context.Background(), qwenPlatform(t))

	res, err := uc.ImportMCP(context.Background(), ImportInput{Code: "WebSearch", Transport: TransportSSE})
	if err != nil {
		t.Fatalf("ImportMCP: %v", err)
	}
	if !strings.Contains(res.Message, "Streamable HTTP") {
		t.Fatalf("应提示已改用 http：%q", res.Message)
	}
	if got := m.created[0].Endpoint; !strings.HasSuffix(got, "/mcp") {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestSearchMCPMarksInstalled(t *testing.T) {
	uc, repo, cat, m, _ := newUC()
	_ = repo.Save(context.Background(), qwenPlatform(t))
	cat.items = []RemoteMCP{{Code: "WebSearch", Name: "联网搜索"}, {Code: "amap-maps", Name: "高德地图"}}
	m.servers = []mcp.Server{{ID: 1, Name: "qwen-websearch"}}

	items, err := uc.SearchMCP(context.Background(), "", "地图", 10)
	if err != nil {
		t.Fatalf("SearchMCP: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if !items[0].Installed || items[1].Installed {
		t.Fatalf("installed 标记错误: %+v", items)
	}
	if items[0].Provider != "qwen" || items[0].Endpoint == "" {
		t.Fatalf("条目未补全: %+v", items[0])
	}
}

func TestSearchMCPNoPlatform(t *testing.T) {
	uc, _, _, _, _ := newUC()
	if _, err := uc.SearchMCP(context.Background(), "", "x", 5); !errors.Is(err, ErrNoPlatform) {
		t.Fatalf("err = %v", err)
	}
}

func TestTestPlatformFallsBackToTemplate(t *testing.T) {
	uc, repo, cat, _, _ := newUC()
	_ = repo.Save(context.Background(), qwenPlatform(t))
	cat.err = errors.New("404 not found")

	res, err := uc.Test(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK || res.CatalogOK {
		t.Fatalf("目录不可用但模板可用时应 OK=true, result=%+v", res)
	}
	if !strings.Contains(res.Message, "/api/v1/mcps/probe/mcp") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestCreatePlatformRegistersSkillHub(t *testing.T) {
	uc, _, _, _, hubs := newUC()
	in := Input{
		Name:         "qwen",
		Kind:         KindQwen,
		APIKey:       strp("sk-test"),
		SkillBaseURL: strp("https://dashscope.aliyuncs.com/api/v1/skills"),
	}
	p, warn, err := uc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if warn != "" {
		t.Fatalf("warn = %q", warn)
	}
	if p.SkillHub != "qwen-skills" {
		t.Fatalf("skill_hub = %q", p.SkillHub)
	}
	if len(hubs.created) != 1 || hubs.created[0].Token != "sk-test" {
		t.Fatalf("hub 未按平台配置创建: %+v", hubs.created)
	}
	if p.HasKey != true {
		t.Fatal("返回的 platform 应带 has_key 标记且不带 key")
	}
}

func TestUpdateKeepsKindWhenOmitted(t *testing.T) {
	uc, repo, _, _, _ := newUC()
	_ = repo.Save(context.Background(), qwenPlatform(t))
	repo.items["qwen"].APIKey = "sk-keep"
	desc := "改个描述"
	p, _, err := uc.Update(context.Background(), "qwen", Input{Description: &desc})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if p.Kind != KindQwen {
		t.Fatalf("kind 被重置为 %q", p.Kind)
	}
	if p.Description != desc {
		t.Fatalf("description = %q", p.Description)
	}
	// 空 api_key 表示不改
	if repo.items["qwen"].APIKey != "sk-keep" {
		t.Fatalf("未传 api_key 不应清空，got %q", repo.items["qwen"].APIKey)
	}
	// 自定义 base_url 不能被默认值覆盖
	if repo.items["qwen"].BaseURL != QwenBaseURL {
		t.Fatalf("base_url = %q", repo.items["qwen"].BaseURL)
	}
}

func TestUpdateKeepsCustomBaseURL(t *testing.T) {
	uc, repo, _, _, _ := newUC()
	p := qwenPlatform(t)
	p.BaseURL = "https://mcp.internal.example.com"
	_ = repo.Save(context.Background(), p)

	desc := "内部网关"
	got, _, err := uc.Update(context.Background(), "qwen", Input{Description: &desc})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.BaseURL != "https://mcp.internal.example.com" {
		t.Fatalf("自定义 base_url 被重置为 %q", got.BaseURL)
	}
}

func TestDeletePlatform(t *testing.T) {
	uc, repo, _, _, _ := newUC()
	_ = repo.Save(context.Background(), qwenPlatform(t))
	if err := uc.Delete(context.Background(), "qwen"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := uc.Delete(context.Background(), "qwen"); !errors.Is(err, ErrPlatformNotFound) {
		t.Fatalf("重复删除 err = %v", err)
	}
}

func strp(s string) *string { return &s }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
