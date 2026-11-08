// scope.go 多租户归属校验（服务层统一入口）。
//
// 设计取舍：
//   - 隔离校验放在 **service（API 边界）** 而不是 biz：biz 同时被后台
//     worker / 辩论 goroutine（context.Background）调用，那里没有请求身份，
//     若在 biz 里做强校验会把内部链路误判成越权；
//   - 数据层（repo）只负责「按 user_id 过滤/写入」，不做策略判断；
//   - 认证未启用时 identity 恒为 default，历史数据 user_id 也是 default，
//     因此单机开发态行为与改造前完全一致。
package service

import (
	"context"
	"strings"

	kerrors "github.com/go-kratos/kratos/v3/errors"

	"go_agent/internal/pkg/identity"
)

// scopeUser 当前请求的归属用户（未认证时 = default）。
func scopeUser(ctx context.Context) string { return identity.UserID(ctx) }

// ownsRecord 判断当前请求身份是否有权访问某条归属用户的数据。
// owner 为空视为历史数据（未回填 user_id），保持可见以免升级即丢数据。
func ownsRecord(ctx context.Context, owner string) bool {
	if owner == "" {
		return true
	}
	return owner == scopeUser(ctx)
}

// ensureOwned 归属不符时返回 403；返回 nil 表示允许访问。
func ensureOwned(ctx context.Context, owner, what string) error {
	if ownsRecord(ctx, owner) {
		return nil
	}
	return kerrors.Forbidden("FORBIDDEN", "无权访问他人的"+what+"（当前登录用户与资源归属不一致）")
}

// auditScope 历史/审计类查询的归属范围：
//   - 管理员（含开发态回环）→ 空字符串，表示全平台（审计需要）
//   - 普通用户 → 自己的 user_id（只读自己的记录）
//
// 历史记录里含查询词、工具参数、召回明细、LLM 原文，默认按人过滤是硬要求；
// 「登录了就能看全平台调用历史」是这类系统最常见也最容易被忽略的越权面。
func auditScope(ctx context.Context) string {
	if identity.AdminAllowed(ctx) {
		return ""
	}
	return scopeUser(ctx)
}

// scopedUserID 归属写入键：认证态（JWT / 静态令牌）强制用令牌身份，
// 请求体里的 user_id 一律不认，避免伪造他人身份写数据；
// 开发态（未启用认证）保留请求体参数，方便控制台为指定用户造数据。
func scopedUserID(ctx context.Context, requested string) string {
	info := identity.From(ctx)
	if info.Auth == "jwt" || info.Auth == "static" {
		return info.UserID
	}
	if v := strings.TrimSpace(requested); v != "" {
		return v
	}
	return info.UserID
}
