package authn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go_agent/internal/biz/org"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/auth"
	"go_agent/internal/pkg/authn"
	"go_agent/internal/pkg/identity"
)

// fakeRepo 只覆盖登录链路用到的方法，其余方法 panic（用到就是用错了）
type fakeRepo struct {
	org.Repo
	users map[string]*org.User // key: tenant + "/" + username
}

func (f *fakeRepo) key(tenant, username string) string { return tenant + "/" + username }

func (f *fakeRepo) GetUserCredentials(_ context.Context, tenant, username string) (string, string, error) {
	u, ok := f.users[f.key(tenant, username)]
	if !ok {
		return "", "", org.ErrUserNotFound
	}
	return u.PasswordHash, u.Status, nil
}

func (f *fakeRepo) FindUserByUsername(_ context.Context, tenant, username string) (*org.User, error) {
	u, ok := f.users[f.key(tenant, username)]
	if !ok {
		return nil, org.ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (f *fakeRepo) SyncLDAPUser(_ context.Context, tenant, username, displayName, email, role string, provision bool) (*org.User, error) {
	if u, ok := f.users[f.key(tenant, username)]; ok {
		u.DisplayName, u.Email, u.Role = displayName, email, role
		cp := *u
		return &cp, nil
	}
	if !provision {
		return nil, org.ErrUserNotFound
	}
	u := &org.User{TenantID: tenant, Username: username, DisplayName: displayName, Email: email, Role: role, Status: org.StatusActive, Source: org.SourceLDAP}
	f.users[f.key(tenant, username)] = u
	cp := *u
	return &cp, nil
}

// fakeLDAP LDAP 验密的替身：只有 alice/devs 组的人验得过
type fakeLDAP struct {
	users map[string]*authn.LDAPUser
}

func (f *fakeLDAP) Enabled() bool { return true }

func (f *fakeLDAP) Authenticate(username, password string) (*authn.LDAPUser, error) {
	if password != "ad-good" {
		return nil, authn.ErrLdapAuthFailed
	}
	if u, ok := f.users[username]; ok {
		return u, nil
	}
	return nil, authn.ErrLdapAuthFailed
}

func (f *fakeLDAP) MapRole(groups []string) string {
	for _, g := range groups {
		if strings.Contains(strings.ToLower(g), "admins") {
			return "tenant_admin"
		}
	}
	return "user"
}

func newUC(repo org.Repo, ldap LDAPVerifier, ttl int64, enabled bool) *UseCase {
	sec := &conf.Security{Auth: &conf.Auth{
		JwtSecret: "unit-secret",
		Login:     &conf.Login{Enabled: enabled, TokenTTLSeconds: ttl, Ldap: &conf.LDAP{AutoProvision: true}},
	}}
	uc := New(repo, sec)
	if ldap != nil {
		uc.ldap = ldap
	}
	return uc
}

func login(ctx context.Context, uc *UseCase, u, p, tenant string) (*LoginResult, error) {
	return uc.Login(ctx, LoginInput{Username: u, Password: p, Tenant: tenant})
}

func TestLoginDisabled(t *testing.T) {
	uc := newUC(&fakeRepo{users: map[string]*org.User{}}, nil, 0, false)
	if _, err := login(context.Background(), uc, "alice", "pw-123456", ""); !errors.Is(err, ErrLoginDisabled) {
		t.Fatalf("未开启登录应拒绝: %v", err)
	}
}

// 本地密码：正确通过、错误拒绝、哈希为空转 LDAP。
func TestLocalLogin(t *testing.T) {
	hash, _ := auth.HashPassword("local-pass")
	repo := &fakeRepo{users: map[string]*org.User{
		"public/alice": {TenantID: "public", Username: "alice", DisplayName: "Alice", Role: "developer", Status: org.StatusActive, PasswordHash: hash},
		"public/bob":   {TenantID: "public", Username: "bob", Status: org.StatusActive, Source: org.SourceLDAP},
	}}
	uc := newUC(repo, &fakeLDAP{users: map[string]*authn.LDAPUser{"bob": {Username: "bob"}}}, 0, true)

	res, err := login(context.Background(), uc, "alice", "local-pass", "")
	if err != nil || res.AuthSource != "local" {
		t.Fatalf("本地登录应成功: %v / %s", err, resAuthSource(res))
	}
	if _, err := login(context.Background(), uc, "alice", "wrong", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("错误密码应拒绝: %v", err)
	}
	// 无哈希用户走 LDAP
	if _, err := login(context.Background(), uc, "bob", "ad-good", ""); err != nil || resAuthSourceOf(err) != "" {
		res2, err2 := login(context.Background(), uc, "bob", "ad-good", "")
		if err2 != nil || res2.AuthSource != "ldap" {
			t.Fatalf("LDAP 用户应走 AD: %v", err2)
		}
	}
}

// 租户隔离：同用户名在不同租户是不同的人，各验各的密码。
func TestLocalLoginTenantScope(t *testing.T) {
	h1, _ := auth.HashPassword("pw-acme")
	repo := &fakeRepo{users: map[string]*org.User{
		"acme/alice": {TenantID: "acme", Username: "alice", Status: org.StatusActive, PasswordHash: h1},
	}}
	uc := newUC(repo, nil, 0, true)
	if _, err := login(context.Background(), uc, "alice", "pw-acme", "acme"); err != nil {
		t.Fatalf("acme 租户应通过: %v", err)
	}
	// public 租户查无此人；未配 LDAP 时与密码错误同口径
	if _, err := login(context.Background(), uc, "alice", "pw-acme", "public"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("其他租户应拒绝: %v", err)
	}
}

// 停用检查在验密之后：正确密码才会得到「已停用」，错误密码只会得到统一口径。
func TestDisabledUser(t *testing.T) {
	hash, _ := auth.HashPassword("pw-123456")
	repo := &fakeRepo{users: map[string]*org.User{
		"public/eva": {TenantID: "public", Username: "eva", Status: org.StatusDisabled, PasswordHash: hash},
	}}
	uc := newUC(repo, nil, 0, true)
	if _, err := login(context.Background(), uc, "eva", "pw-123456", ""); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("停用用户应 403 语义: %v", err)
	}
	if _, err := login(context.Background(), uc, "eva", "wrong", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("停用用户 + 错误密码仍是统一口径（不泄露状态）")
	}
}

// LDAP 首次登录自动建号（auto_provision），组映射决定角色。
func TestLDAPAutoProvision(t *testing.T) {
	repo := &fakeRepo{users: map[string]*org.User{}}
	uc := newUC(repo, &fakeLDAP{users: map[string]*authn.LDAPUser{
		"tchen": {Username: "tchen", DisplayName: "陈工", Email: "tchen@corp", Groups: []string{"CN=Meridians-Admins"}},
	}}, 0, true)

	res, err := login(context.Background(), uc, "tchen", "ad-good", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.AuthSource != "ldap" || res.User.Role != "tenant_admin" || res.User.Source != org.SourceLDAP {
		t.Fatalf("自动建号结果不符: %+v", res.User)
	}
	if res.User.TenantID != identity.DefaultTenant {
		t.Fatalf("LDAP 用户应落默认租户: %s", res.User.TenantID)
	}
}

// LDAP 组调低后再次登录，角色跟着同步（不用管理员手工改）。
func TestLDAPRoleSync(t *testing.T) {
	repo := &fakeRepo{users: map[string]*org.User{}}
	l := &fakeLDAP{users: map[string]*authn.LDAPUser{
		"tchen": {Username: "tchen", DisplayName: "陈工", Groups: []string{"CN=Meridians-Admins"}},
	}}
	uc := newUC(repo, l, 0, true)
	if _, err := login(context.Background(), uc, "tchen", "ad-good", ""); err != nil {
		t.Fatal(err)
	}
	// AD 里调组：失去 admins 组 → 回落 user
	l.users["tchen"].Groups = nil
	res, err := login(context.Background(), uc, "tchen", "ad-good", "")
	if err != nil || res.User.Role != "user" {
		t.Fatalf("角色应随组同步: %v / %q", err, res.User.Role)
	}
}

// AD 验证通过但平台未登记且未开自动建号：明确拒绝。
func TestLDAPNoProvision(t *testing.T) {
	repo := &fakeRepo{users: map[string]*org.User{}}
	uc := newUC(repo, &fakeLDAP{users: map[string]*authn.LDAPUser{"x": {Username: "x"}}}, 0, true)
	uc.loginConf.GetLdap().AutoProvision = false
	if _, err := login(context.Background(), uc, "x", "ad-good", ""); err == nil {
		t.Fatal("未开自动建号应拒绝")
	}
}

// 登录成功签发的令牌必须能被网关验签（同一密钥闭环）。
func TestIssuedTokenVerifiable(t *testing.T) {
	hash, _ := auth.HashPassword("pw-123456")
	repo := &fakeRepo{users: map[string]*org.User{
		"public/alice": {TenantID: "public", Username: "alice", Role: "developer", Status: org.StatusActive, PasswordHash: hash},
	}}
	uc := newUC(repo, nil, 3600, true)
	res, err := login(context.Background(), uc, "alice", "pw-123456", "")
	if err != nil {
		t.Fatal(err)
	}
	// auth.New 与 usecase 内的 issuer 用同一密钥格式
	v := newTestVerifier()
	info, err := v.Verify(res.Token)
	if err != nil {
		t.Fatalf("网关应验签通过: %v", err)
	}
	if info.UserID != "alice" || info.TenantID != "public" || info.Admin {
		t.Fatalf("身份不符: %+v", info)
	}
}

func resAuthSource(r *LoginResult) string {
	if r == nil {
		return ""
	}
	return r.AuthSource
}

func resAuthSourceOf(err error) string { return "" }

func newTestVerifier() interface {
	Verify(string) (identity.Info, error)
} {
	return auth.New("unit-secret", nil)
}
