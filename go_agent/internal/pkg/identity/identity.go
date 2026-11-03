// Package identity 请求身份上下文：把「谁在调用、属于哪个租户」从网关层
// 透传到 biz 层，避免业务代码写死用户标识（历史问题：记忆召回固定用 "default"，
// 所有用户的长期记忆混在同一个命名空间）。
//
// 约定：
//   - 网关（HTTP / gRPC 中间件）解析凭证后调用 With 写入 context；
//   - 业务层只读 UserID / TenantID，不感知凭证形态（JWT / 开发态 / 未来 OIDC）；
//   - 未注入时返回兜底值而非空串，保证单机开发与历史数据（user_id="default"）兼容。
package identity

import "context"

// 兜底身份：未启用认证（本地开发、内网单机）时使用。
const (
	DefaultUser   = "default"
	DefaultTenant = "public"
)

type ctxKey struct{}

// Info 一次请求的身份信息。
type Info struct {
	UserID   string
	TenantID string
	// Auth 凭证来源：jwt / dev（便于审计与排错）
	Auth string
}

// With 把身份写入 context。空值自动回落到默认身份。
func With(ctx context.Context, info Info) context.Context {
	if info.UserID == "" {
		info.UserID = DefaultUser
	}
	if info.TenantID == "" {
		info.TenantID = DefaultTenant
	}
	return context.WithValue(ctx, ctxKey{}, info)
}

// From 读取身份；未注入时返回默认身份。
func From(ctx context.Context) Info {
	if v, ok := ctx.Value(ctxKey{}).(Info); ok {
		if v.UserID == "" {
			v.UserID = DefaultUser
		}
		if v.TenantID == "" {
			v.TenantID = DefaultTenant
		}
		return v
	}
	return Info{UserID: DefaultUser, TenantID: DefaultTenant, Auth: "none"}
}

// UserID 当前用户标识（记忆、会话、配额的作用域键）。
func UserID(ctx context.Context) string { return From(ctx).UserID }

// TenantID 当前租户标识（限流、预算、账单的作用域键）。
func TenantID(ctx context.Context) string { return From(ctx).TenantID }
