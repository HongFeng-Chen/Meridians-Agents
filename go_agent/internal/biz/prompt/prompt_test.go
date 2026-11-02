package prompt

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go_agent/internal/conf"
)

// fakeRepo 内存仓库：验证用例规则，不依赖 MySQL
type fakeRepo struct {
	mu        sync.Mutex
	templates map[string]*Prompt
	versions  map[string][]Version
	scores    []VersionScore // 版本评测得分
	failWith  error          // 模拟存储不可用
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		templates: make(map[string]*Prompt),
		versions:  make(map[string][]Version),
	}
}

func (f *fakeRepo) Create(_ context.Context, p *Prompt, v *Version) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.templates[p.Key]; ok {
		return ErrExists
	}
	cp := *p
	cp.VersionCount = 1
	cp.CreatedAt = time.Now()
	cp.UpdatedAt = cp.CreatedAt
	f.templates[p.Key] = &cp
	vc := *v
	vc.Version = 1
	f.versions[p.Key] = []Version{vc}
	return nil
}

func (f *fakeRepo) Get(_ context.Context, key string) (*Prompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	p, ok := f.templates[key]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *p
	cp.VersionCount = len(f.versions[key])
	return &cp, nil
}

func (f *fakeRepo) List(_ context.Context, filter ListFilter) ([]Prompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Prompt, 0, len(f.templates))
	for _, p := range f.templates {
		if filter.Scene != "" && p.Scene != filter.Scene {
			continue
		}
		if filter.Keyword != "" && !strings.Contains(p.Key, filter.Keyword) && !strings.Contains(p.Name, filter.Keyword) {
			continue
		}
		cp := *p
		cp.VersionCount = len(f.versions[p.Key])
		out = append(out, cp)
	}
	return out, nil
}

func (f *fakeRepo) Update(_ context.Context, p *Prompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.templates[p.Key]
	if !ok {
		return ErrNotFound
	}
	cp := *p
	cp.CreatedAt = cur.CreatedAt
	cp.UpdatedAt = time.Now()
	f.templates[p.Key] = &cp
	return nil
}

func (f *fakeRepo) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.templates[key]; !ok {
		return ErrNotFound
	}
	delete(f.templates, key)
	delete(f.versions, key)
	return nil
}

func (f *fakeRepo) AddVersion(_ context.Context, v *Version) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.templates[v.PromptKey]; !ok {
		return 0, ErrNotFound
	}
	vc := *v
	vc.Version = len(f.versions[v.PromptKey]) + 1
	f.versions[v.PromptKey] = append(f.versions[v.PromptKey], vc)
	return vc.Version, nil
}

func (f *fakeRepo) GetVersion(_ context.Context, key string, version int) (*Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.versions[key] {
		if v.Version == version {
			cp := v
			return &cp, nil
		}
	}
	return nil, ErrVersionNotFound
}

func (f *fakeRepo) ActiveVersion(_ context.Context, key string) (*Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	p, ok := f.templates[key]
	if !ok {
		return nil, ErrNotFound
	}
	for _, v := range f.versions[key] {
		if v.Version == p.ActiveVersion {
			cp := v
			cp.Active = true
			return &cp, nil
		}
	}
	return nil, ErrVersionNotFound
}

func (f *fakeRepo) ListVersions(_ context.Context, key string, limit int) ([]Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.versions[key]
	out := make([]Version, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i])
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) Activate(_ context.Context, key string, version int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.templates[key]
	if !ok {
		return ErrNotFound
	}
	for _, v := range f.versions[key] {
		if v.Version == version {
			p.ActiveVersion = version
			return nil
		}
	}
	return ErrVersionNotFound
}

func (f *fakeRepo) Stats(_ context.Context) (Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var st Stats
	st.Prompts = int64(len(f.templates))
	for _, list := range f.versions {
		st.Versions += int64(len(list))
	}
	return st, nil
}

// fakeCache 内存缓存：记录失效次数
type fakeCache struct {
	mu          sync.Mutex
	data        map[string]Version
	invalidated int
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: make(map[string]Version)}
}

func (c *fakeCache) GetVersion(_ context.Context, key string) (*Version, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	if !ok {
		return nil, false
	}
	cp := v
	cp.Active = true
	return &cp, true
}

func (c *fakeCache) PutVersion(_ context.Context, key string, v *Version, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = *v
}

func (c *fakeCache) Invalidate(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
	c.invalidated++
	return nil
}

func newTestUseCase(t *testing.T, repo Repo, cache Cache) *UseCase {
	t.Helper()
	u, err := New(repo, cache, &conf.Data{Prompt: &conf.Data_Prompt{
		SeedBuiltin: true,
		ListLimit:   200,
		CacheTTL:    nil, // 不启用缓存，保证用例断言确定性
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return u
}

func TestSeedBuiltins(t *testing.T) {
	repo := newFakeRepo()
	u := newTestUseCase(t, repo, newFakeCache())

	for _, b := range Builtins {
		d, err := u.Get(context.Background(), b.Key)
		if err != nil {
			t.Fatalf("内置模板 %s 未写入: %v", b.Key, err)
		}
		if !d.Builtin {
			t.Errorf("%s 应标记为 builtin", b.Key)
		}
		if d.ActiveVersion != 1 || d.Active == nil {
			t.Errorf("%s 生效版本应为 v1，实际 active=%d activeObj=%v", b.Key, d.ActiveVersion, d.Active)
		}
		if d.VersionCount != 1 {
			t.Errorf("%s 版本数 = %d, want 1", b.Key, d.VersionCount)
		}
	}

	// 再次初始化：已有则跳过，不重复写版本
	_ = newTestUseCase(t, repo, newFakeCache())
	d, err := u.Get(context.Background(), KeyRAGSystem)
	if err != nil {
		t.Fatal(err)
	}
	if d.VersionCount != 1 {
		t.Fatalf("重复初始化产生了新版本: %d", d.VersionCount)
	}
}

func TestCreatePublishActivate(t *testing.T) {
	repo := newFakeRepo()
	cache := newFakeCache()
	u := newTestUseCase(t, repo, cache)
	ctx := context.Background()

	if _, err := u.Create(ctx, CreateInput{
		Key: "custom.qa", Name: "自定义问答", Scene: SceneRAG,
		Content: "问题：{{question}}",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 发布即生效（Activate 缺省 true）
	v2, err := u.Publish(ctx, "custom.qa", PublishInput{
		Content: "请回答：{{question}}", Note: "更口语化",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if v2.Version != 2 || !v2.Active {
		t.Fatalf("v2 = %+v", v2)
	}
	if cache.invalidated == 0 {
		t.Fatal("发布后必须失效缓存")
	}

	out, err := u.Render(ctx, "custom.qa", map[string]string{"question": "Q"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "请回答：Q" {
		t.Fatalf("Render = %q, want %q", out, "请回答：Q")
	}

	// 回滚到 v1
	d, err := u.Rollback(ctx, "custom.qa")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if d.ActiveVersion != 1 {
		t.Fatalf("回滚后生效版本 = %d, want 1", d.ActiveVersion)
	}
	out, _ = u.Render(ctx, "custom.qa", map[string]string{"question": "Q"})
	if out != "问题：Q" {
		t.Fatalf("回滚后 Render = %q", out)
	}

	// 已在最早版本，无可回滚
	if _, err := u.Rollback(ctx, "custom.qa"); !errors.Is(err, ErrNoPrevious) {
		t.Fatalf("Rollback 应返回 ErrNoPrevious, got %v", err)
	}
}

func TestPublishDraftNotActive(t *testing.T) {
	u := newTestUseCase(t, newFakeRepo(), newFakeCache())
	ctx := context.Background()

	if _, err := u.Create(ctx, CreateInput{Key: "draft.qa", Content: "A {{q}}"}); err != nil {
		t.Fatal(err)
	}
	no := false
	v, err := u.Publish(ctx, "draft.qa", PublishInput{Content: "B {{q}}", Activate: &no})
	if err != nil {
		t.Fatal(err)
	}
	if v.Active {
		t.Fatal("activate=false 时版本不应生效")
	}
	if out, _ := u.Render(ctx, "draft.qa", map[string]string{"q": "x"}); out != "A x" {
		t.Fatalf("草稿版本不应影响线上: %q", out)
	}
	if _, err := u.Activate(ctx, "draft.qa", 2); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if out, _ := u.Render(ctx, "draft.qa", map[string]string{"q": "x"}); out != "B x" {
		t.Fatalf("激活后应为 v2: %q", out)
	}
}

func TestRenderMissingVars(t *testing.T) {
	u := newTestUseCase(t, newFakeRepo(), newFakeCache())
	_, err := u.Render(context.Background(), KeyRAGUser, map[string]string{"context": "C"})
	var missing *MissingVarsError
	if !errors.As(err, &missing) {
		t.Fatalf("应返回 MissingVarsError, got %v", err)
	}
	if len(missing.Names) != 1 || missing.Names[0] != "question" {
		t.Fatalf("缺失变量 = %v, want [question]", missing.Names)
	}
}

// 存储不可用时应回落内置模板，保证问答链路不断
func TestRenderFallsBackToBuiltin(t *testing.T) {
	repo := newFakeRepo()
	repo.failWith = errors.New("mysql down")
	u, err := New(repo, nil, &conf.Data{Prompt: &conf.Data_Prompt{SeedBuiltin: false}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := u.Render(context.Background(), KeyRAGSystem, nil)
	if err != nil {
		t.Fatalf("应回落内置模板, got err %v", err)
	}
	if !strings.Contains(out, "知识库问答助手") {
		t.Fatalf("回落内容异常: %q", out)
	}
	// 完全未知的 key 才报错
	if _, err := u.Render(context.Background(), "unknown.key", nil); err == nil {
		t.Fatal("未知 key 应报错")
	}
}

func TestCacheServesActiveVersion(t *testing.T) {
	repo := newFakeRepo()
	cache := newFakeCache()
	u := newTestUseCase(t, repo, cache)
	ctx := context.Background()

	if _, err := u.Create(ctx, CreateInput{Key: "cached.qa", Content: "v1 {{q}}"}); err != nil {
		t.Fatal(err)
	}
	// 手动塞入缓存，模拟线上读路径已预热
	cache.PutVersion(ctx, "cached.qa", &Version{PromptKey: "cached.qa", Version: 9, Content: "cached {{q}}"}, time.Minute)
	if out, _ := u.Render(ctx, "cached.qa", map[string]string{"q": "x"}); out != "cached x" {
		t.Fatalf("应命中缓存: %q", out)
	}
	// 激活后缓存失效，读回真实版本
	if _, err := u.Activate(ctx, "cached.qa", 1); err != nil {
		t.Fatal(err)
	}
	if out, _ := u.Render(ctx, "cached.qa", map[string]string{"q": "x"}); out != "v1 x" {
		t.Fatalf("激活后应从仓库读: %q", out)
	}
}

func TestPreviewPartialVars(t *testing.T) {
	u := newTestUseCase(t, newFakeRepo(), newFakeCache())
	res, err := u.Preview(context.Background(), KeyRAGUser, RenderInput{
		Vars: map[string]string{"question": "Q"},
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "context" {
		t.Fatalf("missing = %v, want [context]", res.Missing)
	}
	if len(res.Variables) != 2 {
		t.Fatalf("variables = %v", res.Variables)
	}
	if !strings.Contains(res.Rendered, "Q") {
		t.Fatalf("预览未替换已提供变量: %q", res.Rendered)
	}
}

func TestBuiltinProtectedAndReset(t *testing.T) {
	u := newTestUseCase(t, newFakeRepo(), newFakeCache())
	ctx := context.Background()

	if err := u.Delete(ctx, KeyRAGSystem); !errors.Is(err, ErrBuiltinProtected) {
		t.Fatalf("删除内置模板应被拒绝, got %v", err)
	}

	// 改坏再从内置恢复
	if _, err := u.Publish(ctx, KeyRAGSystem, PublishInput{Content: "坏掉的内容"}); err != nil {
		t.Fatal(err)
	}
	v, err := u.Reset(ctx, KeyRAGSystem, "tester")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !v.Active || v.Version != 3 {
		t.Fatalf("reset 版本 = %+v", v)
	}
	if !strings.Contains(v.Content, "知识库问答助手") {
		t.Fatalf("reset 内容不是内置原文: %q", v.Content)
	}

	// 自定义模板可删除
	if _, err := u.Create(ctx, CreateInput{Key: "tmp.qa", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := u.Delete(ctx, "tmp.qa"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := u.Get(ctx, "tmp.qa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应查不到, got %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	u := newTestUseCase(t, newFakeRepo(), newFakeCache())
	ctx := context.Background()

	if _, err := u.Create(ctx, CreateInput{Key: "1bad", Content: "x"}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("非法 key 应报错, got %v", err)
	}
	if _, err := u.Create(ctx, CreateInput{Key: "ok.key", Content: "   "}); !errors.Is(err, ErrEmptyContent) {
		t.Fatalf("空内容应报错, got %v", err)
	}
	if _, err := u.Create(ctx, CreateInput{Key: "ok.key", Content: "{{v}}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Create(ctx, CreateInput{Key: "ok.key", Content: "{{v}}"}); !errors.Is(err, ErrExists) {
		t.Fatalf("重复 key 应报 ErrExists, got %v", err)
	}
	// 场景缺省为 general，变量自动解析
	d, err := u.Get(ctx, "ok.key")
	if err != nil {
		t.Fatal(err)
	}
	if d.Scene != SceneGeneral {
		t.Errorf("scene = %q, want %q", d.Scene, SceneGeneral)
	}
	if d.Active == nil || len(d.Active.Variables) != 1 {
		t.Errorf("变量未自动解析: %+v", d.Active)
	}
}

func TestUpdateMeta(t *testing.T) {
	u := newTestUseCase(t, newFakeRepo(), newFakeCache())
	ctx := context.Background()
	if _, err := u.Create(ctx, CreateInput{Key: "meta.qa", Name: "旧名", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	name := "新名"
	scene := SceneExtract
	tags := []string{"a", "a", " b "}
	d, err := u.Update(ctx, "meta.qa", UpdateInput{Name: &name, Scene: &scene, Tags: &tags})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if d.Name != "新名" || d.Scene != SceneExtract {
		t.Fatalf("元信息未更新: %+v", d.Prompt)
	}
	if len(d.Tags) != 2 {
		t.Fatalf("标签未去重/裁剪: %v", d.Tags)
	}
}

func TestSaveAgentPrompt(t *testing.T) {
	u := newTestUseCase(t, newFakeRepo(), newFakeCache())

	// 首次：创建 agent.<key>.system 模板
	if err := u.SaveAgentPrompt(context.Background(), "coder", "代码助手", "你是代码助手。"); err != nil {
		t.Fatalf("SaveAgentPrompt 首次失败: %v", err)
	}
	d, err := u.Get(context.Background(), "agent.coder.system")
	if err != nil {
		t.Fatalf("模板未创建: %v", err)
	}
	if d.Scene != "agent" || d.Name != "Agent·代码助手·角色提示词" || d.ActiveVersion != 1 {
		t.Errorf("元数据不符: scene=%s name=%s v=%d", d.Scene, d.Name, d.ActiveVersion)
	}

	// 再次：发布新版本
	if err := u.SaveAgentPrompt(context.Background(), "coder", "代码助手", "你是升级版代码助手。"); err != nil {
		t.Fatalf("SaveAgentPrompt 二次失败: %v", err)
	}
	d2, _ := u.Get(context.Background(), "agent.coder.system")
	if d2.ActiveVersion != 2 {
		t.Errorf("应发布新版本 v2，得 v%d", d2.ActiveVersion)
	}
	v, err := u.Version(context.Background(), "agent.coder.system", 2)
	if err != nil || !strings.Contains(v.Content, "升级版") {
		t.Errorf("新版本内容未生效: %+v err=%v", v, err)
	}

	// 校验：空内容 / 空 key
	if err := u.SaveAgentPrompt(context.Background(), "x", "n", " "); err != ErrEmptyContent {
		t.Errorf("空内容应报 ErrEmptyContent，得 %v", err)
	}
	if err := u.SaveAgentPrompt(context.Background(), " ", "n", "c"); err != ErrEmptyContent {
		t.Errorf("空 key 应报 ErrEmptyContent，得 %v", err)
	}
}

func (f *fakeRepo) InsertVersionScore(ctx context.Context, s VersionScore) error {
	f.scores = append(f.scores, s)
	return nil
}

func (f *fakeRepo) ListVersionScores(ctx context.Context, key string, limit int) ([]VersionScore, error) {
	out := make([]VersionScore, 0)
	for _, s := range f.scores {
		if s.PromptKey == key {
			out = append(out, s)
		}
	}
	return out, nil
}
