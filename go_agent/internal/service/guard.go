package service

// 管理面授权：登录用户 ≠ 管理员。
//
// 背景（P0）：MCP 管理接口原先只依赖认证，不区分角色。只要拿到任意有效令牌
// （甚至认证关闭时的开发态身份），就能创建 stdio MCP 服务 —— 而 stdio 会
// 直接在后端启动子进程，等价于远程命令执行。
//
// 这里提供统一闸门：MCP 配置、工具启停、连接管理等平台级操作必须显式管理员。
// 判定规则见 identity.AdminAllowed：
//   - JWT/静态令牌带 role=admin；
//   - 或认证关闭时的本机回环请求（仅为了本机开发可用）。

import (
	"context"

	"go_agent/internal/pkg/identity"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/log"
)

// requireAdmin 管理操作闸门。拒绝时返回 403，并在日志里留下审计线索。
func requireAdmin(ctx context.Context, action string) error {
	if identity.AdminAllowed(ctx) {
		return nil
	}
	info := identity.From(ctx)
	log.Warn("拒绝管理操作",
		"action", action, "user", info.UserID, "auth", info.Auth,
		"admin", info.Admin, "local", info.Local,
	)
	return kerrors.Forbidden("FORBIDDEN",
		"需要管理员权限："+action+
			"。当前身份 user="+info.UserID+" auth="+info.Auth+
			"；管理面不会把「已登录用户」当作管理员，请使用携带 role=admin 的令牌"+
			"（静态令牌写成 user:tenant:admin），或在认证关闭时从本机回环地址操作")
}
