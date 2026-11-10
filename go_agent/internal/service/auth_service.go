// auth_service.go 登录与策略管理的传输层。
//
// 错误映射的安全考量：登录失败一律 401（用户不存在 / 密码错误 / LDAP 不可用
// 同一个提示），细节留在服务端日志——401 vs 404 的差异就是撞库者的用户名枚举信号。
package service

import (
	"context"
	"strings"
	"time"

	authv1 "go_agent/api/auth/v1"
	"go_agent/internal/biz/audit"
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

// PolicyService Casbin 策略管理服务
type PolicyService struct {
	authv1.UnimplementedPolicyServiceServer
	policies PolicyStore
}

var _ authv1.PolicyServiceServer = (*PolicyService)(nil)

// PolicyStore 策略存储能力（repo.CasbinEngine 的窄接口）
type PolicyStore interface {
	ListPolicies() [][]string
	AddPolicies(rules [][]string) error
	RemovePolicies(rules [][]string) error
	ListBindings() [][]string
	AddBindings(rules [][]string) error
	RemoveBindings(rules [][]string) error
}

// NewPolicyService 构建策略管理服务
func NewPolicyService(store PolicyStore) *PolicyService {
	return &PolicyService{policies: store}
}

// requirePolicyAdmin 策略管理一律 platform:config。
func requirePolicyAdmin(ctx context.Context, action string) error {
	return requirePerm(ctx, authz.PermPlatformConfig, action, "policy", "")
}

// ListPolicies 列出全部 p 规则
func (s *PolicyService) ListPolicies(ctx context.Context, _ *authv1.EmptyReply) (*authv1.ListPoliciesReply, error) {
	if err := requirePolicyAdmin(ctx, "policy.list"); err != nil {
		return nil, err
	}
	out := &authv1.ListPoliciesReply{}
	for _, row := range s.policies.ListPolicies() {
		out.Policies = append(out.Policies, rowToPolicy(row))
	}
	return out, nil
}

// AddPolicy 追加 p 规则
func (s *PolicyService) AddPolicy(ctx context.Context, req *authv1.AddPolicyRequest) (*authv1.EmptyReply, error) {
	if err := requirePolicyAdmin(ctx, "policy.add"); err != nil {
		return nil, err
	}
	rules, err := policyRows(req.GetRules())
	if err != nil {
		return nil, err
	}
	if err := s.policies.AddPolicies(rules); err != nil {
		return nil, internalErr(err)
	}
	audit.Emit(ctx, audit.Event{Action: "policy.add", Result: audit.ResultAllowed, Resource: "policy"})
	return &authv1.EmptyReply{}, nil
}

// RemovePolicy 删除 p 规则
func (s *PolicyService) RemovePolicy(ctx context.Context, req *authv1.RemovePolicyRequest) (*authv1.EmptyReply, error) {
	if err := requirePolicyAdmin(ctx, "policy.remove"); err != nil {
		return nil, err
	}
	rules, err := policyRows(req.GetRules())
	if err != nil {
		return nil, err
	}
	if err := s.policies.RemovePolicies(rules); err != nil {
		return nil, internalErr(err)
	}
	audit.Emit(ctx, audit.Event{Action: "policy.remove", Result: audit.ResultAllowed, Resource: "policy"})
	return &authv1.EmptyReply{}, nil
}

// ListBindings 列出全部 g 规则
func (s *PolicyService) ListBindings(ctx context.Context, _ *authv1.EmptyReply) (*authv1.ListBindingsReply, error) {
	if err := requirePolicyAdmin(ctx, "binding.list"); err != nil {
		return nil, err
	}
	out := &authv1.ListBindingsReply{}
	for _, row := range s.policies.ListBindings() {
		if len(row) != 3 {
			continue
		}
		out.Bindings = append(out.Bindings, &authv1.RoleBinding{
			Username: row[0],
			Role:     row[1],
			Dom:      row[2],
		})
	}
	return out, nil
}

// AddBinding 绑定用户角色
func (s *PolicyService) AddBinding(ctx context.Context, req *authv1.AddBindingRequest) (*authv1.EmptyReply, error) {
	if err := requirePolicyAdmin(ctx, "binding.add"); err != nil {
		return nil, err
	}
	rules, err := bindingRows(req.GetBindings())
	if err != nil {
		return nil, err
	}
	if err := s.policies.AddBindings(rules); err != nil {
		return nil, internalErr(err)
	}
	audit.Emit(ctx, audit.Event{Action: "binding.add", Result: audit.ResultAllowed, Resource: "policy"})
	return &authv1.EmptyReply{}, nil
}

// RemoveBinding 解绑用户角色
func (s *PolicyService) RemoveBinding(ctx context.Context, req *authv1.RemoveBindingRequest) (*authv1.EmptyReply, error) {
	if err := requirePolicyAdmin(ctx, "binding.remove"); err != nil {
		return nil, err
	}
	rules, err := bindingRows(req.GetBindings())
	if err != nil {
		return nil, err
	}
	if err := s.policies.RemoveBindings(rules); err != nil {
		return nil, internalErr(err)
	}
	audit.Emit(ctx, audit.Event{Action: "binding.remove", Result: audit.ResultAllowed, Resource: "policy"})
	return &authv1.EmptyReply{}, nil
}

// policyRows 校验并转换 p 规则；角色统一 role: 前缀，权限点必须规范。
func policyRows(rules []*authv1.PolicyRule) ([][]string, error) {
	out := make([][]string, 0, len(rules))
	for _, r := range rules {
		sub := normalizeSub(r.GetSub())
		perm := strings.TrimSpace(r.GetPerm())
		if sub == "" || perm == "" {
			return nil, badRequest("策略的 sub 与 perm 不能为空")
		}
		if perm != "*" {
			if !authz.KnownPerm(authz.Perm(perm)) {
				return nil, badRequest("未知权限点 " + perm + "（可选 platform:config / tenant:manage / agent:develop / task:operate / task:approve / audit:read）")
			}
		}
		out = append(out, []string{sub, defaultDom(r.GetDom()), perm})
	}
	return out, nil
}

// bindingRows 校验并转换 g 规则；角色必须是规范角色。
func bindingRows(bindings []*authv1.RoleBinding) ([][]string, error) {
	out := make([][]string, 0, len(bindings))
	for _, b := range bindings {
		username := strings.TrimSpace(b.GetUsername())
		role := strings.TrimSpace(b.GetRole())
		if username == "" || role == "" {
			return nil, badRequest("绑定的 username 与 role 不能为空")
		}
		norm := string(authz.Normalize(role))
		if !authz.KnownRole(authz.Role(norm)) {
			return nil, badRequest("未知角色 " + role)
		}
		out = append(out, []string{username, "role:" + norm, defaultDom(b.GetDom())})
	}
	return out, nil
}

// rowToPolicy 回读的 p 规则转 proto
func rowToPolicy(row []string) *authv1.PolicyRule {
	if len(row) != 3 {
		return nil
	}
	return &authv1.PolicyRule{Sub: row[0], Dom: row[1], Perm: row[2]}
}

// normalizeSub sub 允许用户名或 role:<角色>，裸角色名自动补前缀
func normalizeSub(raw string) string {
	sub := strings.TrimSpace(raw)
	if sub == "" || strings.HasPrefix(sub, "role:") {
		return sub
	}
	if authz.KnownRole(authz.Role(sub)) {
		return "role:" + sub
	}
	return sub
}

func defaultDom(dom string) string {
	if d := strings.TrimSpace(dom); d != "" {
		return d
	}
	return "*"
}
