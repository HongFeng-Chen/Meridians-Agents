package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRepo 内存仓库
type fakeRepo struct {
	mu    sync.Mutex
	items map[string]*Skill
	hubs  map[string]*Hub
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{items: map[string]*Skill{}, hubs: map[string]*Hub{}}
}

func (r *fakeRepo) List(ctx context.Context, f Filter) ([]Skill, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Skill, 0, len(r.items))
	for _, s := range r.items {
		if f.Enabled != nil && s.Enabled != *f.Enabled {
			continue
		}
		if f.Keyword != "" && !strings.Contains(s.Name, f.Keyword) && !strings.Contains(s.Title, f.Keyword) {
			continue
		}
		cp := *s
		cp.Content = ""
		cp.Files = nil
		out = append(out, cp)
	}
	return out, nil
}

func (r *fakeRepo) Get(ctx context.Context, name string) (*Skill, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.items[name]
	if !ok {
		return nil, ErrSkillNotFound
	}
	cp := *s
	return &cp, nil
}

func (r *fakeRepo) Save(ctx context.Context, s *Skill) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *s
	r.items[s.Name] = &cp
	return nil
}

func (r *fakeRepo) Delete(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[name]; !ok {
		return ErrSkillNotFound
	}
	delete(r.items, name)
	return nil
}

func (r *fakeRepo) SetEnabled(ctx context.Context, name string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.items[name]
	if !ok {
		return ErrSkillNotFound
	}
	s.Enabled = enabled
	return nil
}

func (r *fakeRepo) Stats(ctx context.Context) (Stats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Stats{Skills: int64(len(r.items)), Hubs: int64(len(r.hubs))}
	for _, s := range r.items {
		if s.Enabled {
			st.Enabled++
		}
	}
	return st, nil
}

func (r *fakeRepo) ListHubs(ctx context.Context) ([]Hub, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Hub, 0, len(r.hubs))
	for _, h := range r.hubs {
		out = append(out, *h)
	}
	return out, nil
}

func (r *fakeRepo) GetHub(ctx context.Context, name string) (*Hub, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hubs[name]
	if !ok {
		return nil, ErrHubNotFound
	}
	cp := *h
	return &cp, nil
}

func (r *fakeRepo) SaveHub(ctx context.Context, h *Hub) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *h
	r.hubs[h.Name] = &cp
	return nil
}

func (r *fakeRepo) DeleteHub(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.hubs[name]; !ok {
		return ErrHubNotFound
	}
	delete(r.hubs, name)
	return nil
}

func (r *fakeRepo) SetDefaultHub(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.hubs[name]; !ok {
		return ErrHubNotFound
	}
	for _, h := range r.hubs {
		h.IsDefault = h.Name == name
	}
	return nil
}

// fakeHub 技能市场
type fakeHub struct {
	search []HubSkill
	pkg    *Skill
	err    error

	urlPkg *Skill
	urlErr error
}

func (f *fakeHub) Search(ctx context.Context, hub Hub, keyword string, limit int) ([]HubSkill, error) {
	return f.search, f.err
}

func (f *fakeHub) Fetch(ctx context.Context, hub Hub, name, version string) (*Skill, error) {
	if f.err != nil {
		return nil, f.err
	}
	cp := *f.pkg
	return &cp, nil
}

func (f *fakeHub) FetchURL(ctx context.Context, rawURL, token string) (*Skill, error) {
	if f.urlErr != nil {
		return nil, f.urlErr
	}
	if f.urlPkg == nil {
		return nil, ErrHubResponse
	}
	cp := *f.urlPkg
	return &cp, nil
}

func newUC(t *testing.T, dir string, hub *fakeHub) (*UseCase, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	if hub == nil {
		hub = &fakeHub{}
	}
	return &UseCase{repo: repo, hub: hub, syncDir: dir, maxFile: 1 << 20, hubLimit: 100}, repo
}

func TestInstallValidation(t *testing.T) {
	dir := t.TempDir()
	uc, _ := newUC(t, dir, nil)
	ctx := context.Background()

	if _, err := uc.Install(ctx, InstallInput{Name: "Bad Name", Content: "x"}); err != ErrInvalidName {
		t.Fatalf("非法名称应拒绝, got %v", err)
	}
	if _, err := uc.Install(ctx, InstallInput{Name: "ok", Content: "  "}); err != ErrEmptyContent {
		t.Fatalf("空内容应拒绝, got %v", err)
	}
	if _, err := uc.Install(ctx, InstallInput{Name: "ok", Files: []File{{Path: "../etc/passwd", Content: "x"}}}); err == nil {
		t.Fatal("路径穿越应拒绝")
	}
	if _, err := uc.Install(ctx, InstallInput{Name: "ok", Files: []File{{Path: "/abs.txt", Content: "x"}}}); err == nil {
		t.Fatal("绝对路径应拒绝")
	}
	uc.maxFile = 4
	if _, err := uc.Install(ctx, InstallInput{Name: "ok", Content: "12345"}); err == nil {
		t.Fatal("超限文件应拒绝")
	}
}

func TestInstallWritesDiskAndEnableDisable(t *testing.T) {
	dir := t.TempDir()
	uc, _ := newUC(t, dir, nil)
	ctx := context.Background()

	s, err := uc.Install(ctx, InstallInput{
		Name:        "pdf-tool",
		Version:     "1.2.0",
		Description: "PDF 处理",
		Content:     "# PDF Tool\n使用说明",
		Files:       []File{{Path: "scripts/run.py", Content: "print(1)"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.FileCount != 1 || s.Checksum == "" {
		t.Fatalf("安装结果异常: %+v", s)
	}
	skillMD := filepath.Join(dir, "pdf-tool", "SKILL.md")
	if b, err := os.ReadFile(skillMD); err != nil || !strings.Contains(string(b), "PDF Tool") {
		t.Fatalf("SKILL.md 未落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pdf-tool", "manifest.json")); err != nil {
		t.Fatalf("manifest.json 未落盘: %v", err)
	}

	if _, err := uc.Disable(ctx, "pdf-tool"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pdf-tool")); !os.IsNotExist(err) {
		t.Fatal("停用后目录应被删除")
	}

	if _, err := uc.Enable(ctx, "pdf-tool"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(skillMD); err != nil {
		t.Fatalf("启用后应重新落盘: %v", err)
	}

	if err := uc.Uninstall(ctx, "pdf-tool"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pdf-tool")); !os.IsNotExist(err) {
		t.Fatal("卸载后目录应被删除")
	}
}

func TestSyncCleansOrphans(t *testing.T) {
	dir := t.TempDir()
	uc, repo := newUC(t, dir, nil)
	ctx := context.Background()

	if _, err := uc.Install(ctx, InstallInput{Name: "a", Content: "# A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.Install(ctx, InstallInput{Name: "b", Content: "# B"}); err != nil {
		t.Fatal(err)
	}
	// 手工造一个孤儿目录 + 把 b 停用（不删目录，模拟历史残留）
	if err := os.MkdirAll(filepath.Join(dir, "gone"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = repo.SetEnabled(ctx, "b", false)

	res, err := uc.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Synced) != 1 || res.Synced[0] != "a" {
		t.Fatalf("同步结果异常: %+v", res)
	}
	for _, name := range []string{"b", "gone"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("孤儿目录 %s 应被清理", name)
		}
	}
}

func TestInstallFromHub(t *testing.T) {
	dir := t.TempDir()
	hub := &fakeHub{
		search: []HubSkill{{Name: "hub-skill", Version: "2.0.0", Title: "市场技能"}},
		pkg: &Skill{
			Name: "hub-skill", Version: "2.0.0", Title: "市场技能",
			Content: "# Hub Skill", Files: []File{{Path: "lib/util.py", Content: "x=1"}},
		},
	}
	uc, repo := newUC(t, dir, hub)
	ctx := context.Background()
	if err := repo.SaveHub(ctx, &Hub{Name: "official", BaseURL: "https://hub.example.com", Enabled: true, IsDefault: true}); err != nil {
		t.Fatal(err)
	}

	s, err := uc.InstallFromHub(ctx, InstallFromHubInput{Name: "hub-skill"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceHub || s.SourceURL != "https://hub.example.com" {
		t.Fatalf("来源未记录: %+v", s)
	}
	if len(s.Files) != 1 || s.Files[0].Path != "lib/util.py" {
		t.Fatalf("文件未入库: %+v", s.Files)
	}
	if _, err := os.Stat(filepath.Join(dir, "hub-skill", "lib", "util.py")); err != nil {
		t.Fatalf("文件未落盘: %v", err)
	}

	list, err := uc.SearchHub(ctx, "hub", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Installed || list[0].Hub != "official" {
		t.Fatalf("搜索结果标记异常: %+v", list)
	}
}

func TestNoHub(t *testing.T) {
	uc, _ := newUC(t, t.TempDir(), &fakeHub{})
	if _, err := uc.SearchHub(context.Background(), "x", "", 0); err != ErrNoHub {
		t.Fatalf("无市场应返回 ErrNoHub, got %v", err)
	}
}

func TestImportFromURL(t *testing.T) {
	dir := t.TempDir()
	hub := &fakeHub{urlPkg: &Skill{
		Name: "qwen-skill", Version: "1.0.0", Title: "千问技能",
		Content: "# Qwen Skill", Files: []File{{Path: "SKILL.md", Content: "# Qwen Skill"}},
	}}
	uc, _ := newUC(t, dir, hub)
	s, err := uc.ImportFromURL(context.Background(), ImportFromURLInput{
		URL: "https://example.com/qwen-skill.zip",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "qwen-skill" || s.Source != SourceURL || s.SourceURL != "https://example.com/qwen-skill.zip" {
		t.Fatalf("来源未记录: %+v", s)
	}
}

func TestImportFromURLNeedsNameForPlainText(t *testing.T) {
	hub := &fakeHub{urlPkg: &Skill{Content: "# 纯文本技能"}}
	uc, _ := newUC(t, t.TempDir(), hub)
	if _, err := uc.ImportFromURL(context.Background(), ImportFromURLInput{URL: "https://example.com/SKILL.md"}); err == nil {
		t.Fatal("没有名字时应要求显式指定 name")
	}
	s, err := uc.ImportFromURL(context.Background(), ImportFromURLInput{
		URL: "https://example.com/SKILL.md", Name: "my-skill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "my-skill" || s.Content != "# 纯文本技能" {
		t.Fatalf("单文件技能入库异常: %+v", s)
	}
}

func TestImportFromURLRejectsBadScheme(t *testing.T) {
	uc, _ := newUC(t, t.TempDir(), &fakeHub{})
	for _, u := range []string{"", "ftp://x/y.zip", "file:///etc/passwd"} {
		if _, err := uc.ImportFromURL(context.Background(), ImportFromURLInput{URL: u, Name: "x"}); err == nil {
			t.Fatalf("%q 应被拒绝", u)
		}
	}
}
