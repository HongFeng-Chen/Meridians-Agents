package service

// 管理面授权：登录用户 ≠ 管理员。
//
// 背景（P0）：MCP 管理接口原先只依赖认证，不区分角色。只要拿到任意有效令牌
// （甚至认证关闭时的开发态身份），就能创建 stdio MCP 服务 —— 而 stdio 会
// 直接在后端启动子进程，等价于远程命令执行。
//
// 这里提供统一闸门：MCP 配置、工具启停、连接管理、技能安装、Agent 定义删除等
// 平台级操作必须显式管理员。判定规则见 identity.AdminAllowed：
//   - JWT/静态令牌带 role=admin；
//   - 认证关闭时的本机回环请求（仅为了本机开发可用）；
//   - 认证关闭且显式设置 AUTH_DEV_TRUST_ALL=true（演示环境逃生阀，启动有 WARN）。
//
// 每一次判定（无论放行还是拒绝）都落一条审计事件：放行也要留痕，
// 否则事后只能证明「有人干过」，无法回答「谁被允许干过」。

import (
	"context"

	"go_agent/internal/biz/audit"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/pkg/identity"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/log"
)

// requireAdmin 管理操作闸门。拒绝时返回 403，并在日志与审计表里留下线索。
func requireAdmin(ctx context.Context, action string) error {
	return requireAdminFor(ctx, action, "", "")
}

// requireAdminFor 带资源标识的管理操作闸门（用于需要追到具体对象的变更）。
func requireAdminFor(ctx context.Context, action, resource, resourceID string) error {
	if identity.AdminAllowed(ctx) {
		audit.Emit(ctx, audit.Event{
			Action:     action,
			Result:     audit.ResultAllowed,
			Resource:   resource,
			ResourceID: resourceID,
		})
		return nil
	}
	info := identity.From(ctx)
	log.Warn("拒绝管理操作",
		"action", action, "user", info.UserID, "auth", info.Auth,
		"admin", info.Admin, "local", info.Local,
	)
	audit.Emit(ctx, audit.Event{
		Action:     action,
		Result:     audit.ResultDenied,
		Resource:   resource,
		ResourceID: resourceID,
		Detail: map[string]any{
			"user":  info.UserID,
			"auth":  info.Auth,
			"admin": info.Admin,
			"local": info.Local,
		},
	})
	return kerrors.Forbidden("FORBIDDEN",
		"需要管理员权限："+action+
			"。当前身份 user="+info.UserID+" auth="+info.Auth+
			"；管理面不会把「已登录用户」当作管理员，请使用携带 role=admin 的令牌"+
			"（静态令牌写成 user:tenant:admin），或在认证关闭时从本机回环地址操作")
}

// RequireAdmin 供 server 层自定义路由复用的管理闸门（审计查询等运维接口）
func RequireAdmin(ctx context.Context, action string) error { return requireAdmin(ctx, action) }

// auditEvent 记录一条管理动作（放行侧的业务明细，比如批了哪张审批单）
func auditEvent(ctx context.Context, resource, action string, detail map[string]any) {
	audit.Emit(ctx, audit.Event{
		Action:   action,
		Result:   audit.ResultAllowed,
		Resource: resource,
		Detail:   detail,
	})
}

// auditToolCall 记录工具调用的风险决策。
//
// 这是「数据面」的审计：工具可能删资源、写库，光记一条 op_histories
// （用过了什么）不够，要能回答「这次高危调用是谁批的、有没有批」。
func auditToolCall(ctx context.Context, t mcp.Tool, risk mcp.RiskLevel, result, approvalID, note string) {
	detail := map[string]any{
		"tool":   t.UniqueName,
		"risk":   string(risk),
		"server": t.ServerID,
	}
	if approvalID != "" {
		detail["approval_id"] = approvalID
	}
	if note != "" {
		detail["note"] = note
	}
	audit.Emit(ctx, audit.Event{
		Action:     "mcp.tool.call",
		Result:     result,
		Resource:   "tool",
		ResourceID: t.UniqueName,
		Risk:       string(risk),
		Detail:     detail,
	})
}
