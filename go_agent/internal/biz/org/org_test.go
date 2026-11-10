package org

import (
	"context"
	"errors"
	"testing"

	"go_agent/internal/pkg/identity"
)

// memRepo 内存实现：只覆盖用例用到的行为，规则本身在 UseCase 里。
type memRepo struct {
	tenants map[string]*Tenant
	users   map[int64]*User
	nextID  int64
}

func newMemRepo() *memRepo {
	return &memRepo{tenants: map[string]*Tenant{}, users: map[int64]*User{}, nextID: 1}
}

func (m *memRepo) ListTenants(_ context.Context, f TenantFilter) ([]Tenant, error) {
	out := []Tenant{}
	for _, t := range m.tenants {
		if f.Key != "" && t.Key != f.Key {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		out = append(out, *t)
	}
	return out, nil
}

func (m *memRepo) GetTenant(_ context.Context, key string) (*Tenant, error) {
	t, ok := m.tenants[key]
	if !ok {
		return nil, ErrTenantNotFound
	}
	cp := *t
	return &cp, nil
}

func (m *memRepo) CreateTenant(_ context.Context, t *Tenant) error {
	if _, ok := m.tenants[t.Key]; ok {
		return ErrTenantExists
	}
	cp := *t
	m.tenants[t.Key] = &cp
	return nil
}

func (m *memRepo) UpdateTenant(_ context.Context, t *Tenant) error {
	if _, ok := m.tenants[t.Key]; !ok {
		return ErrTenantNotFound
	}
	cp := *t
	m.tenants[t.Key] = &cp
	return nil
}

func (m *memRepo) DeleteTenant(_ context.Context, key string) error {
	if _, ok := m.tenants[key]; !ok {
		return ErrTenantNotFound
	}
	delete(m.tenants, key)
	return nil
}

func (m *memRepo) Stats(_ context.Context, keys []string) (map[string]Stat, error) {
	out := map[string]Stat{}
	for _, k := range keys {
		out[k] = Stat{}
	}
	for _, u := range m.users {
		if s, ok := out[u.TenantID]; ok {
			s.UserCount++
			out[u.TenantID] = s
		}
	}
	return out, nil
}

func (m *memRepo) ListUsers(_ context.Context, f UserFilter) ([]User, int, error) {
	out := []User{}
	for _, u := range m.users {
		if f.TenantID != "" && u.TenantID != f.TenantID {
			continue
		}
		if f.Role != "" && u.Role != f.Role {
			continue
		}
		if f.Status != "" && u.Status != f.Status {
			continue
		}
		out = append(out, *u)
	}
	return out, len(out), nil
}

func (m *memRepo) GetUser(_ context.Context, id int64) (*User, error) {
	u, ok := m.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (m *memRepo) CreateUser(_ context.Context, u *User) error {
	for _, ex := range m.users {
		if ex.TenantID == u.TenantID && ex.Username == u.Username {
			return ErrUserExists
		}
	}
	u.ID = m.nextID
	m.nextID++
	cp := *u
	m.users[cp.ID] = &cp
	return nil
}

func (m *memRepo) UpdateUser(_ context.Context, u *User) error {
	if _, ok := m.users[u.ID]; !ok {
		return ErrUserNotFound
	}
	cp := *u
	m.users[cp.ID] = &cp
	return nil
}

func (m *memRepo) DeleteUser(_ context.Context, id int64) error {
	if _, ok := m.users[id]; !ok {
		return ErrUserNotFound
	}
	delete(m.users, id)
	return nil
}

func (m *memRepo) CountActiveAdmins(_ context.Context, tenantID string, excludeID int64) (int, error) {
	n := 0
	for _, u := range m.users {
		if u.TenantID != tenantID || u.ID == excludeID {
			continue
		}
		if u.Role == "tenant_admin" && u.Status == StatusActive {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) CountUsers(_ context.Context, tenantID string) (int, error) {
	n := 0
	for _, u := range m.users {
		if u.TenantID == tenantID {
			n++
		}
	}
	return n, nil
}

// —— 测试脚手架 ——

func seeded() (*UseCase, *memRepo) {
	r := newMemRepo()
	r.tenants[DefaultTenantKey] = &Tenant{Key: DefaultTenantKey, Name: "默认租户", Status: StatusActive}
	r.tenants["acme"] = &Tenant{Key: "acme", Name: "Acme", Status: StatusActive}
	return New(r), r
}

func adminCtx(user, tenant string) context.Context {
	return identity.With(context.Background(), identity.Info{UserID: user, TenantID: tenant, Auth: "jwt", Admin: true})
}

func tenantAdminCtx(user, tenant string) context.Context {
	return identity.With(context.Background(), identity.Info{
		UserID: user, TenantID: tenant, Auth: "jwt", Roles: []string{"tenant_admin"},
	})
}

func memberCtx(user, tenant string) context.Context {
	return identity.With(context.Background(), identity.Info{UserID: user, TenantID: tenant, Auth: "jwt"})
}

func TestCreateTenantRules(t *testing.T) {
	uc, _ := seeded()
	ctx := adminCtx("root", DefaultTenantKey)

	// 大写会被归一化成小写，避免「同一个租户两种写法」
	got, err := uc.CreateTenant(ctx, TenantInput{Key: " ACME-2 ", Name: ""})
	if err != nil {
		t.Fatalf("创建租户失败: %v", err)
	}
	if got.Key != "acme-2" || got.Name != "acme-2" || got.Status != StatusActive {
		t.Fatalf("归一化结果不符: %+v", got)
	}

	// 内置租户不可创建（已存在，且语义上是保留键）
	if _, err := uc.CreateTenant(ctx, TenantInput{Key: "public"}); !errors.Is(err, ErrTenantExists) {
		t.Fatalf("public 应判定为已占用: %v", err)
	}
	// 非法 key
	for _, bad := range []string{"1abc", "a", "acme space", "acme/2"} {
		if _, err := uc.CreateTenant(ctx, TenantInput{Key: bad}); !errors.Is(err, ErrInvalidTenant) {
			t.Fatalf("%q 应当被拒绝: %v", bad, err)
		}
	}
	// 非平台管理员不能建租户
	if _, err := uc.CreateTenant(memberCtx("bob", "acme"), TenantInput{Key: "beta"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("普通成员不应能建租户: %v", err)
	}
}

func TestDeleteTenantRules(t *testing.T) {
	uc, r := seeded()
	ctx := adminCtx("root", DefaultTenantKey)

	if err := uc.DeleteTenant(ctx, DefaultTenantKey); !errors.Is(err, ErrBuiltinTenant) {
		t.Fatalf("public 不可删除: %v", err)
	}

	// 有成员的租户不可删：成员先迁走，历史任务保留
	r.users[10] = &User{ID: 10, TenantID: "acme", Username: "bob", Role: "user", Status: StatusActive}
	if err := uc.DeleteTenant(ctx, "acme"); !errors.Is(err, ErrTenantHasUsers) {
		t.Fatalf("有成员的租户应拒绝删除: %v", err)
	}
	delete(r.users, 10)
	if err := uc.DeleteTenant(ctx, "acme"); err != nil {
		t.Fatalf("清空成员后应可删除: %v", err)
	}
	if err := uc.DeleteTenant(ctx, "acme"); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("重复删除应返回不存在: %v", err)
	}
}

func TestTenantScopeIsEnforced(t *testing.T) {
	uc, _ := seeded()

	// 非平台管理员：列表只剩自己租户
	list, err := uc.ListTenants(memberCtx("bob", "acme"), TenantFilter{})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(list) != 1 || list[0].Key != "acme" {
		t.Fatalf("应只看到 acme，实际 %+v", list)
	}

	// 平台管理员：能看到全部，且 public 带 builtin 标记
	all, err := uc.ListTenants(adminCtx("root", DefaultTenantKey), TenantFilter{})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("应看到 2 个租户，实际 %d", len(all))
	}
	for _, tn := range all {
		if tn.Key == DefaultTenantKey && !tn.Builtin {
			t.Fatal("public 应标记为内置租户")
		}
	}
}

func TestCreateUserRules(t *testing.T) {
	uc, _ := seeded()
	ctx := memberCtx("alice", "acme")

	u, err := uc.CreateUser(ctx, UserInput{Username: "bob", Role: "developer"})
	if err != nil {
		t.Fatalf("登记成员失败: %v", err)
	}
	if u.TenantID != "acme" || u.Role != "developer" || u.Status != StatusActive || u.Source != SourceManual {
		t.Fatalf("默认值不符: %+v", u)
	}
	if u.DisplayName != "bob" {
		t.Fatalf("显示名应回落到用户名: %+v", u)
	}

	// 同租户重名
	if _, err := uc.CreateUser(ctx, UserInput{Username: "bob"}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("重名应返回 ErrUserExists: %v", err)
	}
	// 跨租户同名允许（不同的人）
	if _, err := uc.CreateUser(adminCtx("root", DefaultTenantKey), UserInput{TenantID: "public", Username: "bob"}); err != nil {
		t.Fatalf("跨租户同名应允许: %v", err)
	}
	// 不能往别人租户里塞人
	if _, err := uc.CreateUser(ctx, UserInput{TenantID: "public", Username: "eve"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("越租户创建应被拒绝: %v", err)
	}
	// 普通成员不能授予 platform_admin
	if _, err := uc.CreateUser(ctx, UserInput{Username: "mallory", Role: "platform_admin"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("非平台管理员不应能授予 platform_admin: %v", err)
	}
	// 租户必须存在
	if _, err := uc.CreateUser(adminCtx("root", DefaultTenantKey), UserInput{TenantID: "ghost", Username: "x1"}); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("未知租户应返回 ErrTenantNotFound: %v", err)
	}
}

func TestUpdateUserProtections(t *testing.T) {
	uc, _ := seeded()
	ctx := tenantAdminCtx("alice", "acme")
	me, _ := uc.CreateUser(ctx, UserInput{Username: "alice", Role: "tenant_admin"})
	other, _ := uc.CreateUser(ctx, UserInput{Username: "bob", Role: "tenant_admin"})

	// 不能停用自己
	disabled := StatusDisabled
	if _, err := uc.UpdateUser(ctx, me.ID, UserPatch{Status: &disabled}); !errors.Is(err, ErrSelfProtect) {
		t.Fatalf("停用自己应被拒绝: %v", err)
	}

	// 不是最后一个管理员时可以降级别人
	roleUser := "user"
	if _, err := uc.UpdateUser(ctx, other.ID, UserPatch{Role: &roleUser}); err != nil {
		t.Fatalf("降级他人失败: %v", err)
	}

	// 只剩自己一个管理员时，不能把自己降级（会无人可管）
	if _, err := uc.UpdateUser(ctx, me.ID, UserPatch{Role: &roleUser}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("最后一个管理员应受保护: %v", err)
	}

	// 平台管理员是恢复通道：租户管理员离职后，平台侧必须能清理最后一人
	if _, err := uc.UpdateUser(adminCtx("root", DefaultTenantKey), me.ID, UserPatch{Role: &roleUser}); err != nil {
		t.Fatalf("平台管理员应可解除最后一个管理员: %v", err)
	}
}

func TestDeleteUserProtections(t *testing.T) {
	uc, r := seeded()
	ctx := tenantAdminCtx("alice", "acme")
	me, _ := uc.CreateUser(ctx, UserInput{Username: "alice", Role: "tenant_admin"})

	// 内置 default 主体不可删
	r.users[99] = &User{ID: 99, TenantID: "acme", Username: DefaultUser, Role: "user", Status: StatusActive}
	if err := uc.DeleteUser(ctx, 99); !errors.Is(err, ErrForbidden) {
		t.Fatalf("default 主体不应被删除: %v", err)
	}

	// 不能删自己
	if err := uc.DeleteUser(ctx, me.ID); !errors.Is(err, ErrSelfProtect) {
		t.Fatalf("删除自己应被拒绝: %v", err)
	}

	// 只有一名管理员时，删掉最后一人也会让租户失去管理者（此处由 self 规则先命中）
	bob, _ := uc.CreateUser(ctx, UserInput{Username: "bob", Role: "user"})
	if err := uc.DeleteUser(ctx, bob.ID); err != nil {
		t.Fatalf("删除普通成员应成功: %v", err)
	}
}

// —— 新增的登录链路方法（memRepo 不参与登录用例的单测，空实现保证接口完整）——

func (r *memRepo) FindUserByUsername(_ context.Context, tenantID, username string) (*User, error) {
	for i := range r.users {
		if r.users[i].TenantID == tenantID && r.users[i].Username == username {
			u := *r.users[i]
			return &u, nil
		}
	}
	return nil, ErrUserNotFound
}

func (r *memRepo) GetUserCredentials(_ context.Context, tenantID, username string) (string, string, error) {
	for i := range r.users {
		if r.users[i].TenantID == tenantID && r.users[i].Username == username {
			return r.users[i].PasswordHash, r.users[i].Status, nil
		}
	}
	return "", "", ErrUserNotFound
}

func (r *memRepo) SetPasswordHash(_ context.Context, id int64, hash string) error {
	for i := range r.users {
		if r.users[i].ID == id {
			r.users[i].PasswordHash = hash
			return nil
		}
	}
	return ErrUserNotFound
}

func (r *memRepo) SyncLDAPUser(_ context.Context, tenantID, username, displayName, email, role string, provision bool) (*User, error) {
	if u, err := r.FindUserByUsername(context.Background(), tenantID, username); err == nil {
		u.DisplayName, u.Email, u.Role = displayName, email, role
		return u, nil
	}
	if !provision {
		return nil, ErrUserNotFound
	}
	var maxID int64
	for id := range r.users {
		if id > maxID {
			maxID = id
		}
	}
	u := &User{ID: maxID + 1, TenantID: tenantID, Username: username, DisplayName: displayName, Email: email, Role: role, Status: StatusActive, Source: SourceLDAP}
	r.users[u.ID] = u
	return u, nil
}
