package space

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go_agent/internal/pkg/principal"
)

// fakeRepo 内存仓储：只实现用例真正用到的语义，用于验证继承与共享规则。
type fakeRepo struct {
	spaces map[string]*Space
	shares map[uint64]*Share
	seq    uint64
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{spaces: map[string]*Space{}, shares: map[uint64]*Share{}}
}

func (f *fakeRepo) Create(_ context.Context, s *Space) error {
	if _, ok := f.spaces[s.SpaceID]; ok {
		return ErrConflict
	}
	cp := *s
	f.spaces[s.SpaceID] = &cp
	return nil
}

func (f *fakeRepo) Get(_ context.Context, id string) (*Space, error) {
	s, ok := f.spaces[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakeRepo) Update(_ context.Context, s *Space) error {
	if _, ok := f.spaces[s.SpaceID]; !ok {
		return ErrNotFound
	}
	cp := *s
	f.spaces[s.SpaceID] = &cp
	return nil
}

func (f *fakeRepo) Delete(_ context.Context, id string) error {
	delete(f.spaces, id)
	return nil
}

func (f *fakeRepo) ListByTenant(_ context.Context, tenantID string) ([]Space, error) {
	out := []Space{}
	for _, s := range f.spaces {
		if s.TenantID == tenantID {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeRepo) ListChildren(_ context.Context, parent string) ([]Space, error) {
	out := []Space{}
	for _, s := range f.spaces {
		if s.ParentSpaceID == parent {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeRepo) ListDescendants(_ context.Context, id string) ([]Space, error) {
	self, ok := f.spaces[id]
	if !ok {
		return nil, ErrNotFound
	}
	prefix := strings.TrimPrefix(self.Path+"/"+self.SpaceID, "/")
	out := []Space{}
	for _, s := range f.spaces {
		if s.SpaceID == id {
			continue
		}
		if s.Path == prefix || strings.HasPrefix(s.Path, prefix+"/") {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeRepo) UpdatePathPrefix(_ context.Context, oldPrefix, newPrefix string, delta int) error {
	for _, s := range f.spaces {
		if s.Path == oldPrefix || strings.HasPrefix(s.Path, oldPrefix+"/") {
			s.Path = newPrefix + strings.TrimPrefix(s.Path, oldPrefix)
			s.Depth += delta
		}
	}
	return nil
}

func (f *fakeRepo) AddShare(_ context.Context, sh *Share) error {
	f.seq++
	cp := *sh
	cp.ID = f.seq
	f.shares[cp.ID] = &cp
	return nil
}

func (f *fakeRepo) DeleteShare(_ context.Context, id uint64) error {
	delete(f.shares, id)
	return nil
}

func (f *fakeRepo) GetShare(_ context.Context, id uint64) (*Share, error) {
	sh, ok := f.shares[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *sh
	return &cp, nil
}

func (f *fakeRepo) ListShares(_ context.Context, spaceID string) ([]Share, error) {
	out := []Share{}
	for _, sh := range f.shares {
		if sh.SpaceID == spaceID {
			out = append(out, *sh)
		}
	}
	return out, nil
}

func (f *fakeRepo) ListSharesForTenant(_ context.Context, _ string, refs []PrincipalRef) ([]Share, error) {
	out := []Share{}
	for _, sh := range f.shares {
		for _, ref := range refs {
			if sh.PrincipalType == ref.Type && sh.PrincipalID == ref.ID {
				out = append(out, *sh)
			}
		}
	}
	return out, nil
}

func dev(t *testing.T) context.Context {
	t.Helper()
	// 测试里把调用方设为平台管理员：空间创建的管理权校验不属于本文件关注点，
	// 单独由 TestCreateRequiresAdmin 验证。
	return principal.With(context.Background(), adminUser())
}

func adminUser() *principal.Principal {
	return &principal.Principal{Issuer: "test", SubjectID: "alice", TenantID: "public", Admin: true}
}

func alice() *principal.Principal {
	return &principal.Principal{Issuer: "test", SubjectID: "alice", TenantID: "public"}
}

func bob() *principal.Principal {
	return &principal.Principal{Issuer: "test", SubjectID: "bob", TenantID: "public"}
}

// 平台 / 租户空间互不干扰；个人空间默认 isolated，别人看不见。
func TestAllowedSpacesIsolation(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo)
	ctx := dev(t)

	if _, err := uc.Create(ctx, adminUser(), CreateInput{SpaceID: PlatformSpaceID, Name: "平台", SpaceType: TypePlatform, InheritanceMode: InheritInherit}); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.Create(ctx, adminUser(), CreateInput{SpaceID: "tenant:public", Name: "租户", SpaceType: TypeTenant}); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.Create(ctx, adminUser(), CreateInput{SpaceID: "personal:public:alice", Name: "alice", SpaceType: TypePersonal}); err != nil {
		t.Fatal(err)
	}

	ids, err := uc.AllowedSpaceIDs(ctx, alice())
	if err != nil {
		t.Fatal(err)
	}
	if !has(ids, PlatformSpaceID) || !has(ids, "tenant:public") || !has(ids, "personal:public:alice") {
		t.Fatalf("alice 应能看到三级空间: %v", ids)
	}
	bobIDs, _ := uc.AllowedSpaceIDs(ctx, bob())
	if has(bobIDs, "personal:public:alice") {
		t.Fatalf("bob 不应看到 alice 的个人空间: %v", bobIDs)
	}
	if !has(bobIDs, PlatformSpaceID) || !has(bobIDs, "tenant:public") {
		t.Fatalf("bob 应能看到平台与租户空间: %v", bobIDs)
	}
}

// restricted 子空间不继承父空间授权；inherit 子空间继承。
func TestInheritanceModes(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo)
	ctx := dev(t)
	_, _ = uc.Create(ctx, adminUser(), CreateInput{SpaceID: "tenant:acme", TenantID: "acme", Name: "acme", SpaceType: TypeTenant})
	_, _ = uc.Create(ctx, adminUser(), CreateInput{SpaceID: "dept:acme:rd", TenantID: "acme", ParentSpaceID: "tenant:acme", Name: "研发", SpaceType: TypeDepartment, InheritanceMode: InheritInherit})
	_, _ = uc.Create(ctx, adminUser(), CreateInput{SpaceID: "dept:acme:secret", TenantID: "acme", ParentSpaceID: "tenant:acme", Name: "保密", SpaceType: TypeDepartment, InheritanceMode: InheritRestricted})

	carol := &principal.Principal{Issuer: "test", SubjectID: "carol", TenantID: "acme"}
	carolCtx := principal.With(context.Background(), carol)
	ids, err := uc.AllowedSpaceIDs(carolCtx, carol)
	if err != nil {
		t.Fatal(err)
	}
	if !has(ids, "tenant:acme") || !has(ids, "dept:acme:rd") {
		t.Fatalf("租户成员应继承租户空间与 inherit 部门空间: %v", ids)
	}
	if has(ids, "dept:acme:secret") {
		t.Fatalf("restricted 空间不应被继承: %v", ids)
	}
}

// 显式共享给部门后，该部门成员可见。
func TestShareToDepartment(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo)
	ctx := dev(t)
	_, _ = uc.Create(ctx, adminUser(), CreateInput{SpaceID: "tenant:acme", TenantID: "acme", Name: "acme", SpaceType: TypeTenant})
	_, _ = uc.Create(ctx, adminUser(), CreateInput{SpaceID: "dept:acme:secret", TenantID: "acme", ParentSpaceID: "tenant:acme", Name: "保密", SpaceType: TypeDepartment, InheritanceMode: InheritRestricted})
	if _, err := uc.Grant(ctx, alice(), "dept:acme:secret", PrincipalDepartment, "rd", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	carol := &principal.Principal{Issuer: "test", SubjectID: "carol", TenantID: "acme", DepartmentIDs: []string{"rd"}}
	ids, err := uc.AllowedSpaceIDs(principal.With(context.Background(), carol), carol)
	if err != nil {
		t.Fatal(err)
	}
	if !has(ids, "dept:acme:secret") {
		t.Fatalf("部门共享应可见: %v", ids)
	}
}

// 移动空间到自己的子树下必须被拒绝（否则整棵树会脱链成环）。
func TestMoveCycleRejected(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo)
	ctx := dev(t)
	_, _ = uc.Create(ctx, adminUser(), CreateInput{SpaceID: "tenant:acme", TenantID: "acme", Name: "acme", SpaceType: TypeTenant})
	_, _ = uc.Create(ctx, adminUser(), CreateInput{SpaceID: "aa", TenantID: "acme", ParentSpaceID: "tenant:acme", Name: "aa", SpaceType: TypeCustom})
	_, _ = uc.Create(ctx, adminUser(), CreateInput{SpaceID: "bb", TenantID: "acme", ParentSpaceID: "aa", Name: "bb", SpaceType: TypeCustom})

	parent := "bb"
	if _, err := uc.Update(ctx, alice(), "tenant:acme", UpdateInput{ParentSpaceID: &parent}); !errors.Is(err, ErrCycle) {
		t.Fatalf("移动到自身子树应报 ErrCycle，实际 %v", err)
	}
}

// 个人空间只能给自己建：管理员也不能替别人建（owner 校验）。
func TestCreatePersonalForOtherDenied(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo)
	ctx := dev(t)
	p := alice()
	ctx = principal.With(context.Background(), &principal.Principal{
		Issuer: "test", SubjectID: "alice", TenantID: "public",
	})
	if _, err := uc.Create(ctx, p, CreateInput{
		SpaceID: "personal:public:bob", Name: "bob", SpaceType: TypePersonal, OwnerID: "bob",
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("应拒绝替他人创建个人空间，实际 %v", err)
	}
}

func TestScopeToSpaceID(t *testing.T) {
	cases := []struct{ scope, tenant, user, want string }{
		{"system", "acme", "alice", PlatformSpaceID},
		{"tenant", "acme", "alice", "tenant:acme"},
		{"user", "acme", "alice", "personal:acme:alice"},
		{"", "acme", "alice", "personal:acme:alice"},
	}
	for _, c := range cases {
		if got := ScopeToSpaceID(c.scope, c.tenant, c.user); got != c.want {
			t.Errorf("ScopeToSpaceID(%q)=%q，期望 %q", c.scope, got, c.want)
		}
	}
}

func TestEnsureDefaultsIdempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo)
	ctx := dev(t)
	if _, _, _, err := uc.EnsureDefaults(ctx, alice()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := uc.EnsureDefaults(ctx, alice()); err != nil {
		t.Fatalf("重复调用应幂等: %v", err)
	}
	if len(repo.spaces) != 3 {
		t.Fatalf("应有 platform/tenant/personal 三个空间，实际 %d", len(repo.spaces))
	}
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

var _ = time.Now

// 个人空间是隐式单位：即使 knowledge_spaces 里没有这一行，
// 本人的 personal:<tenant>:<user> 空间也必须出现在可见集合里——
// 否则入库先写 space_id、空间行未建时，个人文档会从检索白名单消失。
func TestAllowedSpacesIncludesImplicitPersonal(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUseCase(repo)
	ctx := dev(t)

	// 库里只有平台与租户空间，故意不建任何个人空间行
	if _, err := uc.Create(ctx, adminUser(), CreateInput{SpaceID: PlatformSpaceID, Name: "平台", SpaceType: TypePlatform, InheritanceMode: InheritInherit}); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.Create(ctx, adminUser(), CreateInput{SpaceID: "tenant:public", Name: "租户", SpaceType: TypeTenant}); err != nil {
		t.Fatal(err)
	}

	ids, err := uc.AllowedSpaceIDs(ctx, alice())
	if err != nil {
		t.Fatal(err)
	}
	if !has(ids, "personal:public:alice") {
		t.Fatalf("alice 的隐式个人空间应可见: %v", ids)
	}
	bobIDs, _ := uc.AllowedSpaceIDs(ctx, bob())
	if has(bobIDs, "personal:public:alice") {
		t.Fatalf("bob 不应看到 alice 的隐式个人空间: %v", bobIDs)
	}
}
