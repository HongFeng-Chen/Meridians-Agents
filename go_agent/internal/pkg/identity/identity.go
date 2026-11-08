// Package identity 请求身份上下文：把「谁在调用、属于哪个租户」从网关层
// 透传到 biz 层，避免业务代码写死用户标识（历史问题：记忆召回固定用 "default"，
// 所有用户的长期记忆混在同一个命名空间）。
//
// 约定：
//   - 网关（HTTP / gRPC 中间件）解析凭证后调用 With 写入 context；
//   - 业务层只读 UserID / TenantID，不感知凭证形态（JWT / 开发态 / 未来 OIDC）；
//   - 未注入时返回兜底值而非空串，保证单机开发与历史数据（user_id="default"）兼容。
package identity

import (
	"context"
	"os"
	"strings"
)

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
	// Auth 凭证来源：jwt / static / dev / none（便于审计与排错）
	Auth string
	// Admin 是否为管理员（来自 JWT 的 role/roles claim 或静态令牌的第三段）。
	// 注意：这是「权限」而不是「身份」——登录用户默认不是管理员。
	Admin bool
	// Local 请求是否来自回环地址。仅在认证关闭的开发态下用于放行本机管理操作，
	// 防止「AUTH_ENABLED=false + 0.0.0.0 监听」把管理面裸奔到公网。
	Local bool
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

// AdminAllowed 是否允许执行平台管理操作（MCP 服务配置、Skill 安装等）。
//
// 规则刻意保守：
//   - 有效凭证 + role=admin → 允许；
//   - 认证关闭的开发态（Auth=dev）且请求来自回环地址 → 允许，保证本机开发可用；
//   - 认证关闭 + 显式开启 AUTH_DEV_TRUST_ALL=true → 允许（见下）；
//   - 其余一律拒绝。特别是「认证关闭 + 远程请求」必须拒绝：
//     否则任何能访问端口的人都能创建 stdio MCP 服务，等价于远程命令执行。
//
// AUTH_DEV_TRUST_ALL 是给**演示环境**留的显式逃生阀：单机演示时浏览器从另一台
// 机器访问，回环判定不成立，管理操作会全 403。这个开关默认关闭，打开时启动日志
// 会打 WARN——它把管理面重新变成了匿名可达，只在「确定不对外」的环境里用。
func AdminAllowed(ctx context.Context) bool {
	info := From(ctx)
	if info.Admin {
		return true
	}
	if info.Auth != "dev" {
		return false
	}
	return info.Local || devTrustAll
}

// DevTrustAll 是否开启了开发态「无条件信任」逃生阀（供启动告警使用）
func DevTrustAll() bool { return devTrustAll }

// devTrustAll AUTH_DEV_TRUST_ALL：认证关闭时把所有请求视为管理员。
// 与 MCP_STDIO_* / EGRESS_* 一致，走环境变量，进程启动读一次。
var devTrustAll = func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AUTH_DEV_TRUST_ALL"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}()

// IsAdmin 仅看显式管理员标记（不包含开发态回环放行），用于审计与日志。
func IsAdmin(ctx context.Context) bool { return From(ctx).Admin }
