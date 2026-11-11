// auth_service.go 登录与身份的传输层。
//
// 错误映射的安全考量：登录失败一律 401（用户不存在 / 密码错误 / LDAP 不可用
// 同一个提示），细节留在服务端日志——401 vs 404 的差异就是撞库者的用户名枚举信号。
package service

import (
	"context"
	"time"

	authv1 "go_agent/api/auth/v1"
	authnbiz "go_agent/internal/biz/authn"
	"go_agent/internal/pkg/authz"
	"go_agent/internal/pkg/identity"

	kerrors "github.com/go-kratos/kratos/v3/errors"
)

// AuthService 登录服务
type AuthService struct {
	authv1.UnimplementedAuthServiceServer
	uc *authnbiz.UseCase
}

var _ authv1.AuthServiceServer = (*AuthService)(nil)

// NewAuthService 构建登录服务
func NewAuthService(uc *authnbiz.UseCase) *AuthService {
	return &AuthService{uc: uc}
}

// Login 用户名密码登录
func (s *AuthService) Login(ctx context.Context, req *authv1.LoginRequest) (*authv1.LoginReply, error) {
	if !s.uc.Enabled() {
		// 404 而不是 401：端点都没开，语义是「没有这个功能」
		return nil, kerrors.NotFound("AUTH_LOGIN_DISABLED", "用户名密码登录未开启")
	}
	res, err := s.uc.Login(ctx, authnbiz.LoginInput{
		Username: req.GetUsername(),
		Password: req.GetPassword(),
		Tenant:   req.GetTenant(),
	})
	if err != nil {
		return nil, loginErr(err)
	}
	return &authv1.LoginReply{
		Token:      res.Token,
		ExpiresAt:  res.ExpiresAt.Format(time.RFC3339),
		AuthSource: res.AuthSource,
	}, nil
}

// Me 当前身份
func (s *AuthService) Me(ctx context.Context, _ *authv1.EmptyReply) (*authv1.MeReply, error) {
	info := identity.From(ctx)
	out := &authv1.MeReply{
		UserId:   info.UserID,
		TenantId: info.TenantID,
	}
	for _, r := range authz.Roles(ctx) {
		out.Roles = append(out.Roles, string(r))
	}
	if user, _, err := s.uc.Me(ctx, info); err == nil && user != nil {
		out.DisplayName = user.DisplayName
		out.Email = user.Email
		out.Source = user.Source
	}
	return out, nil
}

// loginErr 登录错误 → HTTP：统一 401，不给撞库者任何枚举信号。
func loginErr(err error) error {
	switch err {
	case authnbiz.ErrUserDisabled:
		return kerrors.Forbidden("AUTH_DISABLED", err.Error())
	case authnbiz.ErrLoginDisabled:
		return kerrors.NotFound("AUTH_LOGIN_DISABLED", err.Error())
	case authnbiz.ErrNotConfigured:
		return kerrors.InternalServer("AUTH_NOT_CONFIGURED", err.Error())
	default:
		if kerrors.IsBadRequest(err) || authnbiz.ErrBadCredentials == err {
			return kerrors.Unauthorized("AUTH_BAD_CREDENTIALS", "用户名或密码错误")
		}
		// LDAP 不可达 / 建号失败等内部错误也按 401 出：对外保持同一口径，
		// 真实原因由 biz 层的日志与审计记录
		return kerrors.Unauthorized("AUTH_BAD_CREDENTIALS", "用户名或密码错误")
	}
}
