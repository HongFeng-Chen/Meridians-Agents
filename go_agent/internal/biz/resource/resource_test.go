package resource

import (
	"context"
	"errors"
	"testing"
	"time"

	"go_agent/internal/pkg/principal"
)

type fakeRepo struct {
	res    map[string]*Resource
	grants []Grant
	vers   []Version
	seq    uint64
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{res: map[string]*Resource{}}
}

func (f *fakeRepo) Create(_ context.Context, r *Resource) error {
	if _, ok := f.res[r.ResourceID]; ok {
		return ErrConflict
	}
	cp := *r
	f.res[r.ResourceID] = &cp
	return nil
}

func (f *fakeRepo) Update(_ context.Context, r *Resource) error {
	if _, ok := f.res[r.ResourceID]; !ok {
		return ErrNotFound
	}
	cp := *r
	f.res[r.ResourceID] = &cp
	return nil
}

func (f *fakeRepo) Get(_ context.Context, id string) (*Resource, error) {
	r, ok := f.res[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRepo) List(_ context.Context, fl Filter) ([]Resource, error) {
	out := []Resource{}
	for _, r := range f.res {
		if fl.TenantID != "" && r.TenantID != fl.TenantID {
			continue
		}
		if fl.ResourceType != "" && r.ResourceType != fl.ResourceType {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (f *fakeRepo) Delete(_ context.Context, id string) error {
	delete(f.res, id)
	return nil
}

func (f *fakeRepo) AddVersion(_ context.Context, v *Version) error {
	f.vers = append(f.vers, *v)
	return nil
}

func (f *fakeRepo) ListVersions(_ context.Context, id string) ([]Version, error) {
	out := []Version{}
	for _, v := range f.vers {
		if v.ResourceID == id {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeRepo) Grants(_ context.Context, id string) ([]Grant, error) {
	out := []Grant{}
	for _, g := range f.grants {
		if g.ResourceID == id {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeRepo) AddGrant(_ context.Context, g *Grant) error {
	f.seq++
	g.ID = f.seq
	for i := range f.grants {
		if f.grants[i].ResourceID == g.ResourceID && f.grants[i].PrincipalType == g.PrincipalType &&
			f.grants[i].PrincipalID == g.PrincipalID && f.grants[i].Action == g.Action {
			f.grants[i].Effect = g.Effect
			return nil
		}
	}
	f.grants = append(f.grants, *g)
	return nil
}

func (f *fakeRepo) DeleteGrant(_ context.Context, id, pt, pid, action string) error {
	out := f.grants[:0]
	for _, g := range f.grants {
		if g.ResourceID == id && g.PrincipalType == pt && g.PrincipalID == pid && g.Action == action {
			continue
		}
		out = append(out, g)
	}
	f.grants = out
	return nil
}

func admin() *principal.Principal {
	return &principal.Principal{Issuer: "t", SubjectID: "alice", TenantID: "public", Admin: true}
}

func user(sub, tenant string) *principal.Principal {
	return &principal.Principal{Issuer: "t", SubjectID: sub, TenantID: tenant}
}

func ctxWith(p *principal.Principal) context.Context {
	return principal.With(context.Background(), p)
}

func newRes(uc *UseCase, t *testing.T, p *principal.Principal, r *Resource) *Resource {
	t.Helper()
	got, err := uc.Register(ctxWith(p), p, r)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// 归属人拥有全部动作；陌生人默认拒绝（默认拒绝是整张授权表的地基）。
func TestOwnerAndDefaultDeny(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo, nil)
	owner := user("alice", "public")
	r := newRes(uc, t, owner, &Resource{ResourceID: "agent:demo", ResourceType: TypeAgent, TenantID: "public", Name: "demo"})

	for _, action := range []string{ActionRead, ActionUpdate, ActionDelete, ActionExecute} {
		ok, err := uc.Check(context.Background(), owner, r.ResourceID, action)
		if err != nil || !ok {
			t.Fatalf("归属人应有 %s 权限: %v/%v", action, ok, err)
		}
	}
	ok, err := uc.Check(context.Background(), user("bob", "public"), r.ResourceID, ActionRead)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("非归属人默认应拒绝")
	}
}

// 跨租户默认拒绝，即使资源 visibility=tenant。
func TestTenantHardIsolation(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo, nil)
	owner := user("alice", "acme")
	r := newRes(uc, t, owner, &Resource{ResourceID: "prompt:x", ResourceType: TypePrompt, TenantID: "acme", Visibility: VisibilityTenant})
	if r == nil {
		t.Fatal("注册失败")
	}
	// 手动改可见性（Register 内部会保留入参，这里确认语义）
	r.Visibility = VisibilityTenant
	_ = repo.Update(context.Background(), r)

	ok, _ := uc.Check(context.Background(), user("bob", "other"), "prompt:x", ActionRead)
	if ok {
		t.Fatal("跨租户读取应拒绝")
	}
	ok, _ = uc.Check(context.Background(), user("bob", "acme"), "prompt:x", ActionRead)
	if !ok {
		t.Fatal("同租户读取 tenant 可见资源应允许")
	}
}

// 显式 Deny 优先于显式 Allow，也优先于归属人。
func TestExplicitDenyWins(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo, nil)
	alice := user("alice", "public")
	r := newRes(uc, t, alice, &Resource{ResourceID: "skill:s1", ResourceType: TypeSkill, TenantID: "public"})
	if err := uc.Grant(ctxWith(alice), alice, r.ResourceID, PrincipalUser, "alice", []string{ActionExecute}, EffectDeny); err != nil {
		t.Fatal(err)
	}
	ok, err := uc.Check(context.Background(), alice, r.ResourceID, ActionExecute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("显式 Deny 应覆盖归属人")
	}
}

// 显式 Allow 让其他用户可读，但不给写。
func TestExplicitAllow(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo, nil)
	alice := user("alice", "public")
	bob := user("bob", "public")
	r := newRes(uc, t, alice, &Resource{ResourceID: "kb:1", ResourceType: TypeKnowledgeBase, TenantID: "public"})
	if err := uc.Grant(ctxWith(alice), alice, r.ResourceID, PrincipalUser, "bob", []string{ActionRead}, EffectAllow); err != nil {
		t.Fatal(err)
	}
	if ok, _ := uc.Check(context.Background(), bob, r.ResourceID, ActionRead); !ok {
		t.Fatal("显式授权应可读")
	}
	if ok, _ := uc.Check(context.Background(), bob, r.ResourceID, ActionUpdate); ok {
		t.Fatal("未授权的动作应拒绝")
	}
}

// 空间继承：资源挂在空间下，空间可见即可读。
func TestSpaceInheritance(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo, &fakeSpaces{ids: map[string][]string{"carol": {"personal:public:carol"}}})
	r := newRes(uc, t, user("alice", "public"), &Resource{
		ResourceID: "graph:g1", ResourceType: TypeGraphSpace, TenantID: "public", SpaceID: "personal:public:carol",
	})
	carol := user("carol", "public")
	if ok, _ := uc.Check(context.Background(), carol, r.ResourceID, ActionRead); !ok {
		t.Fatal("空间成员应可读")
	}
	if ok, _ := uc.Check(context.Background(), user("dave", "public"), r.ResourceID, ActionRead); ok {
		t.Fatal("非空间成员应拒绝")
	}
}

// 平台公共资源允许跨租户只读，但不允许写。
func TestPlatformPublicRead(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo, nil)
	r := newRes(uc, t, admin(), &Resource{ResourceID: "agent:public", ResourceType: TypeAgent, TenantID: "public", Visibility: VisibilityPublic})
	other := user("bob", "other")
	if ok, _ := uc.Check(context.Background(), other, r.ResourceID, ActionRead); !ok {
		t.Fatal("公共资源应允许跨租户只读")
	}
	if ok, _ := uc.Check(context.Background(), other, r.ResourceID, ActionExecute); ok {
		t.Fatal("公共资源不应允许跨租户执行")
	}
}

// 更新资源追加版本，版本号递增，历史不可覆盖。
func TestUpdateAppendsVersion(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo, nil)
	alice := user("alice", "public")
	r := newRes(uc, t, alice, &Resource{ResourceID: "prompt:p1", ResourceType: TypePrompt, TenantID: "public", Name: "v1"})
	r.Name = "v2"
	got, err := uc.Update(ctxWith(alice), alice, r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 {
		t.Fatalf("版本应递增到 2，实际 %d", got.Version)
	}
	if len(repo.vers) != 2 {
		t.Fatalf("应有两份快照，实际 %d", len(repo.vers))
	}
}

// 不能把租户资源授权给另一个租户。
func TestGrantAcrossTenantRejected(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo, nil)
	alice := user("alice", "acme")
	r := newRes(uc, t, alice, &Resource{ResourceID: "mcp:srv", ResourceType: TypeMCPServer, TenantID: "acme"})
	err := uc.Grant(ctxWith(alice), alice, r.ResourceID, PrincipalTenant, "other", []string{ActionRead}, EffectAllow)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("应拒绝跨租户授权，实际 %v", err)
	}
}

// 未知资源按「无权」处理（不暴露是否存在）。
func TestUnknownResourceDenied(t *testing.T) {
	uc := NewUseCase(newFakeRepo(), nil)
	ok, err := uc.Check(context.Background(), user("alice", "public"), "not-exist", ActionRead)
	if err != nil || ok {
		t.Fatalf("未知资源应返回 false/nil，实际 %v/%v", ok, err)
	}
}

type fakeSpaces struct{ ids map[string][]string }

func (f *fakeSpaces) AllowedSpaceIDs(_ context.Context, p *principal.Principal) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	return f.ids[p.SubjectID], nil
}

var _ = time.Now
