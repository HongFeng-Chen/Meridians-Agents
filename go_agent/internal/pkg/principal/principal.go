// Package principal 统一调用方身份（V2 External Identity）。
//
// 背景：V1 的 identity.Info 只回答「用户是谁、属于哪个租户」，够用但不够做
// 资源级授权——判断「这个调用方能否读这个知识空间」还需要 issuer、client_id、
// scopes、部门等外部身份信息，而这些信息在不同第三方系统里的 claim 名并不一致。
//
// 本包把它们归一化成 Principal：
//
//	issuer + client_id + tenant_id + subject_id  ← 用户唯一身份（不能只用 subject）
//
// 约定：
//   - 认证（Authenticate）只回答「你是谁」；授权（authorization 包）回答「你能做什么」。
//     第三方令牌里的 roles **不会**自动变成平台 RBAC 角色。
//   - Principal 由网关写入 context，业务层通过 From(ctx) 读取；
//     未注入时返回与 identity 一致的兜底身份，保证单机开发与历史调用不炸。
package principal

import (
	"context"
	"strings"

	"go_agent/internal/pkg/identity"
)

type ctxKey struct{}

// Principal 一次请求的调用方身份。
type Principal struct {
	Issuer   string
	ClientID string

	// SubjectID 外部系统内的用户标识（JWT sub）。
	SubjectID string

	// TenantID 租户隔离键。跨租户访问默认拒绝，只有平台公共资源例外。
	TenantID string

	// Roles 外部令牌声明的角色原文。仅作参考，不能直接当平台权限用。
	Roles []string
	// Scopes OAuth2 scope（平台能力级授权，如 agent:run、kb:read）。
	Scopes []string

	// DepartmentIDs 外部组织部门；知识空间按它做部门继承。
	DepartmentIDs []string

	Audience  []string
	RequestID string

	// AuthType 凭证形态：jwt / jwks / oidc / introspection / apikey / static / dev
	AuthType string

	// Admin 平台管理员标记（来自令牌签名后的 role=platform_admin 或静态令牌）。
	// 它是「身份的一部分」，但资源授权仍以 explicit grant 为准，见 authorization。
	Admin bool
}

// FromIdentity 把 V1 的 identity.Info 归一化成 Principal。
//
// 兼容规则：
//   - Subject 为空回落到 UserID；
//   - Issuer 为空时用 "internal"（平台自签），避免空串参与唯一键；
//   - Auth 为空时用 "dev"。
func FromIdentity(info identity.Info) *Principal {
	p := &Principal{
		Issuer:        strings.TrimSpace(info.Issuer),
		ClientID:      strings.TrimSpace(info.ClientID),
		SubjectID:     firstNonEmpty(info.Subject, info.UserID),
		TenantID:      firstNonEmpty(info.TenantID, identity.DefaultTenant),
		Roles:         info.Roles,
		Scopes:        info.Scopes,
		DepartmentIDs: info.Departments,
		Audience:      info.Audience,
		AuthType:      firstNonEmpty(info.Auth, "dev"),
		Admin:         info.Admin,
	}
	if p.Issuer == "" {
		p.Issuer = "internal"
	}
	if p.SubjectID == "" {
		p.SubjectID = identity.DefaultUser
	}
	return p
}

// IdentityKey 用户唯一身份：issuer + client_id + tenant_id + subject_id。
//
// 不同第三方系统可能存在相同的 subject（比如都用 "admin"），只按 subject
// 合并会把两个人当成一个人——这是外部身份接入最容易踩的坑。
func (p *Principal) IdentityKey() string {
	if p == nil {
		return ""
	}
	return strings.Join([]string{p.Issuer, p.ClientID, p.TenantID, p.SubjectID}, "|")
}

// HasScope 是否具备某个平台能力 scope（大小写不敏感）。
func (p *Principal) HasScope(scope string) bool {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" || p == nil {
		return false
	}
	for _, s := range p.Scopes {
		if strings.EqualFold(strings.TrimSpace(s), scope) {
			return true
		}
	}
	return false
}

// HasRole 是否声明了某个外部角色（大小写不敏感，仅供业务参考）。
func (p *Principal) HasRole(role string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" || p == nil {
		return false
	}
	for _, r := range p.Roles {
		if strings.EqualFold(strings.TrimSpace(r), role) {
			return true
		}
	}
	return false
}

// InDepartment 是否属于某个外部部门。
func (p *Principal) InDepartment(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || p == nil {
		return false
	}
	for _, d := range p.DepartmentIDs {
		if strings.EqualFold(strings.TrimSpace(d), id) {
			return true
		}
	}
	return false
}

// IdentityInfo 反向投影成 V1 的 identity.Info，供尚未改造的中间件/业务复用。
func (p *Principal) IdentityInfo() identity.Info {
	if p == nil {
		return identity.From(context.Background())
	}
	return identity.Info{
		UserID:      p.SubjectID,
		TenantID:    p.TenantID,
		Auth:        p.AuthType,
		Admin:       p.Admin,
		Roles:       p.Roles,
		Issuer:      p.Issuer,
		ClientID:    p.ClientID,
		Subject:     p.SubjectID,
		Scopes:      p.Scopes,
		Departments: p.DepartmentIDs,
		Audience:    p.Audience,
	}
}

// With 把 Principal 写入 context。
func With(ctx context.Context, p *Principal) context.Context {
	if p == nil {
		p = FromIdentity(identity.From(ctx))
	}
	return context.WithValue(ctx, ctxKey{}, p)
}

// From 读取 Principal；未注入时由 identity.Info 投影，保证任何调用点都有值。
func From(ctx context.Context) *Principal {
	if v, ok := ctx.Value(ctxKey{}).(*Principal); ok && v != nil {
		return v
	}
	return FromIdentity(identity.From(ctx))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
