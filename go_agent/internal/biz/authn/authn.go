// Package authn 登录用例：本地密码 + LDAP/AD 统一入口。
//
// 验证顺序刻意设计成「名册优先」：
//  1. users 表里能查到人 → 有本地密码验本地密码；无密码（LDAP 用户）走 AD 验密；
//  2. 查不到人 → 配置了 LDAP 就去 AD 验（验证通过后按 auto_provision 决定建号），
//     没配置就统一报「用户名或密码错误」。
//
// 统一口径的原因：区分「用户不存在 / 密码错误 / 用户被停用」等于给撞库者
// 提供免费的用户名枚举接口。停用用户的拒绝放在验密**之后**——不然停用状态
// 本身就成了探测信号。
//
// 角色同步：LDAP 用户每次登录都按组映射刷新 role（AD 里调组，平台跟着变）；
// 本地用户的角色只由控制台管理，登录不改。
package authn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/audit"
	"go_agent/internal/biz/org"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/auth"
	"go_agent/internal/pkg/authn"
	"go_agent/internal/pkg/authz"
	"go_agent/internal/pkg/identity"
)

// 领域错误：service 层把前两个统一映射成 401（细节只进日志），第三个 403。
var (
	ErrBadCredentials = errors.New("用户名或密码错误")
	ErrUserDisabled   = errors.New("账号已被停用")
	ErrLoginDisabled  = errors.New("用户名密码登录未开启")
	ErrNotConfigured  = errors.New("登录不可用：未配置 JWT 密钥")
)

// LDAPVerifier LDAP 验密能力（pkg/authn.LDAPClient 的窄接口，便于单测替换）
type LDAPVerifier interface {
	Enabled() bool
	Authenticate(username, password string) (*authn.LDAPUser, error)
	MapRole(groups []string) string
}

// UseCase 登录用例
type UseCase struct {
	repo      org.Repo
	issuer    *auth.Issuer
	ldap      LDAPVerifier
	loginConf *conf.Login
	now       func() time.Time
}

// New 构建登录用例。jwtSecret 为空或 login 未启用时端点在 service 层直接拒绝，
// 这里仍完整装配（配置热改后无需重建依赖）。
func New(repo org.Repo, sec *conf.Security) *UseCase {
	a := sec.GetAuth()
	lc := a.GetLogin()
	return &UseCase{
		repo:      repo,
		issuer:    auth.NewIssuer(a.GetJwtSecret(), time.Duration(lc.GetTokenTTLSeconds())*time.Second),
		ldap:      authn.NewLDAPClient(lc.GetLdap()),
		loginConf: lc,
		now:       time.Now,
	}
}

// Enabled 登录端点是否开放（配置 login.enabled）
func (u *UseCase) Enabled() bool {
	return u.loginConf.GetEnabled()
}

// LoginInput 登录入参
type LoginInput struct {
	Username string
	Password string
	// Tenant 本地密码登录的目标租户；LDAP 忽略
	Tenant string
}

// LoginResult 登录结果
type LoginResult struct {
	Token      string
	ExpiresAt  time.Time
	AuthSource string // local | ldap
	User       *org.User
}

// Login 执行登录：验密 → 状态检查 → 建号/同步 → 签发令牌。
func (u *UseCase) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	if !u.Enabled() {
		return nil, ErrLoginDisabled
	}
	if u.issuer == nil {
		return nil, ErrNotConfigured
	}
	username := strings.TrimSpace(in.Username)
	if username == "" || in.Password == "" {
		return nil, ErrBadCredentials
	}
	tenant := strings.TrimSpace(in.Tenant)
	if tenant == "" {
		tenant = identity.DefaultTenant
	}

	source, user, err := u.verify(ctx, tenant, username, in.Password)
	if err != nil {
		return nil, err
	}

	// 停用检查放在验密成功之后：拒绝信息不提前泄露账号状态
	if user.Status == org.StatusDisabled {
		return nil, ErrUserDisabled
	}

	token, exp, err := u.issue(user)
	if err != nil {
		return nil, err
	}

	audit.Emit(ctx, audit.Event{Action: "auth.login", Result: audit.ResultAllowed, Resource: "user", ResourceID: username})
	return &LoginResult{Token: token, ExpiresAt: exp, AuthSource: source, User: user}, nil
}

// verify 验密并保证名册里存在该用户，返回验证方式与档案。
func (u *UseCase) verify(ctx context.Context, tenant, username, password string) (string, *org.User, error) {
	hash, status, credErr := u.repo.GetUserCredentials(ctx, tenant, username)
	switch {
	case credErr == nil:
		// 名册里有人：有本地密码验本地，没有（或本地失败且配了 LDAP）验 AD
		if hash != "" {
			if perr := auth.CheckPassword(hash, password); perr == nil {
				user, gerr := u.repo.FindUserByUsername(ctx, tenant, username)
				if gerr != nil {
					return "", nil, gerr
				}
				return "local", user, nil
			} else if !errors.Is(perr, auth.ErrNoLocalPassword) {
				return "", nil, ErrBadCredentials
			}
		}
		if !u.ldap.Enabled() {
			return "", nil, ErrBadCredentials
		}
	case errors.Is(credErr, org.ErrUserNotFound):
		if !u.ldap.Enabled() {
			// 未配置 LDAP：名册查无此人就是验密失败（口径与密码错误一致）
			return "", nil, ErrBadCredentials
		}
	default:
		return "", nil, credErr
	}

	// LDAP 路径：验密发生在 AD 侧
	lu, err := u.ldap.Authenticate(username, password)
	if err != nil {
		if errors.Is(err, authn.ErrLdapNotConfigured) {
			return "", nil, ErrBadCredentials
		}
		return "", nil, ErrBadCredentials
	}
	role := u.ldap.MapRole(lu.Groups)
	// 已登记的 LDAP 用户若被平台停用，AD 验密通过也拒绝（平台侧停用优先）
	if status == org.StatusDisabled {
		return "", nil, ErrUserDisabled
	}
	user, err := u.repo.SyncLDAPUser(ctx, tenant, lu.Username, lu.DisplayName, lu.Email, role,
		u.loginConf.GetLdap().GetAutoProvision())
	if err != nil {
		if errors.Is(err, org.ErrUserNotFound) {
			// AD 验证通过但平台未登记且未开自动建号
			return "", nil, fmt.Errorf("%w（未在平台登记且未开启自动建号）", ErrBadCredentials)
		}
		return "", nil, err
	}
	return "ldap", user, nil
}

// issue 把名册档案转成身份并签发令牌。
func (u *UseCase) issue(user *org.User) (string, time.Time, error) {
	role := authz.Normalize(user.Role)
	info := identity.Info{
		UserID:   user.Username,
		TenantID: user.TenantID,
		Auth:     "jwt",
		Admin:    role == authz.RolePlatformAdmin,
		Roles:    []string{string(role)},
	}
	token, exp, err := u.issuer.Issue(info)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// Me 当前登录身份（从令牌解出 + 名册补充档案）。
func (u *UseCase) Me(ctx context.Context, info identity.Info) (*org.User, []authz.Role, error) {
	roles := authz.Roles(ctx)
	if info.Auth == "dev" || info.TenantID == "" {
		// 开发态 / 外部令牌：名册里未必有此人，角色直接用令牌解出来的
		return nil, roles, nil
	}
	if user, err := u.repo.FindUserByUsername(ctx, info.TenantID, info.UserID); err == nil {
		return user, roles, nil
	}
	// 静态令牌 / 外部签发的 JWT：名册查不到不报错，返回令牌信息即可
	return nil, roles, nil
}
