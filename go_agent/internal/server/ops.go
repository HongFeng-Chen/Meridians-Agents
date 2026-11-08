package server

// 审计与工具审批的 HTTP 入口。
//
// 为什么不用 proto 生成路由：
//   - 这两个接口是「运维面」的：审批高危工具调用、查审计事件。调用方是脚本、
//     kubectl exec 里的 curl 或管理台，不需要进入对外的 OpenAPI 契约；
//   - 走 srv.Route 可以避免改 proto → 重新生成 → 前端适配的长链路。
//
// 注意：srv.Route 注册的 handler **不会**自动经过 http.Middleware（Kratos 的
// 中间件是在生成的 handler 内部通过 ctx.Middleware 套上去的）。所以这里手动
// 包一层 ctx.Middleware，保证身份注入与限流照样生效——否则这两个接口会变成
// 「无身份」的匿名入口。

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"go_agent/internal/biz/audit"
	"go_agent/internal/service"

	khttp "github.com/go-kratos/kratos/v3/transport/http"
)

const (
	toolApprovalPrefix = "/api/mcp/tool-approvals"
	auditPath          = "/api/admin/audits"
)

// registerOpsRoutes 挂上运维面路由
func registerOpsRoutes(srv *khttp.Server, mcpSvc *service.MCPService, auditUC *audit.UseCase) {
	if srv == nil {
		return
	}
	r := srv.Route("/")

	// —— 工具调用审批 ——

	// 待审批列表：GET /api/mcp/tool-approvals?status=pending&limit=50
	r.GET(toolApprovalPrefix, func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			list, err := mcpSvc.ListToolApprovals(c, ctx.Query().Get("status"), ctx.Query().Get("limit"))
			if err != nil {
				return err
			}
			return ctx.JSON(200, map[string]any{"approvals": list, "total": len(list)})
		})
	})

	// 批准 / 驳回：POST /api/mcp/tool-approvals/{id}/approve|deny
	r.POST(toolApprovalPrefix+"/{id}/approve", approvalDecisionHandler(mcpSvc, true))
	r.POST(toolApprovalPrefix+"/{id}/deny", approvalDecisionHandler(mcpSvc, false))

	// 单条详情：GET /api/mcp/tool-approvals/{id}
	r.GET(toolApprovalPrefix+"/{id}", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			id := approvalIDFrom(ctx.Request().URL.Path)
			list, err := mcpSvc.ListToolApprovals(c, "", "500")
			if err != nil {
				return err
			}
			for _, a := range list {
				if a.ID == id {
					return ctx.JSON(200, a)
				}
			}
			return ctx.JSON(404, map[string]any{"code": 404, "reason": "NOT_FOUND", "message": "审批单不存在或已过期"})
		})
	})

	// —— 审计查询 ——

	// GET /api/admin/audits?action=&result=&resource=&user=&since=&until=&limit=
	if auditUC != nil {
		r.GET(auditPath, func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				q := ctx.Query()
				f := audit.Filter{
					Action:   strings.TrimSpace(q.Get("action")),
					Result:   strings.TrimSpace(q.Get("result")),
					Resource: strings.TrimSpace(q.Get("resource")),
					UserID:   strings.TrimSpace(q.Get("user")),
				}
				f.Since = parseTimeParam(q.Get("since"))
				f.Until = parseTimeParam(q.Get("until"))
				if n, err := strconv.Atoi(q.Get("limit")); err == nil {
					f.Limit = n
				}
				// 审计是全平台数据，只有管理员能看
				if err := service.RequireAdmin(c, "audit.list"); err != nil {
					return err
				}
				list, err := auditUC.List(c, f)
				if err != nil {
					return err
				}
				return ctx.JSON(200, map[string]any{"events": list, "total": len(list)})
			})
		})
	}
}

// approvalDecisionHandler 把「读 body + 调服务」包成 Route handler
func approvalDecisionHandler(svc *service.MCPService, approve bool) func(khttp.Context) error {
	return func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			id := approvalIDFrom(ctx.Request().URL.Path)
			if id == "" {
				return ctx.JSON(400, map[string]any{"code": 400, "reason": "BAD_REQUEST", "message": "缺少审批单号"})
			}
			var body struct {
				Comment string `json:"comment"`
			}
			if ctx.Request().Body != nil {
				_ = json.NewDecoder(ctx.Request().Body).Decode(&body) // 允许空 body
			}
			a, err := svc.DecideToolApproval(c, id, body.Comment, approve)
			if err != nil {
				return err
			}
			return ctx.JSON(200, a)
		})
	}
}

// withMiddleware 手动套上服务中间件（认证 → 限流 → 校验），保证自定义路由
// 与 proto 生成的路由走同一套安全边界。
func withMiddleware(ctx khttp.Context, fn func(context.Context) error) error {
	h := func(c context.Context, _ any) (any, error) {
		return nil, fn(c)
	}
	_, err := ctx.Middleware(h)(ctx, ctx.Request())
	return err
}

// approvalIDFrom 从 /api/mcp/tool-approvals/{id}[/approve] 里取 id
func approvalIDFrom(path string) string {
	rest := strings.TrimPrefix(path, toolApprovalPrefix+"/")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return strings.TrimSpace(id)
}

// parseTimeParam 支持 RFC3339 与相对时间（如 24h、7d）
func parseTimeParam(raw string) time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d)
	}
	// 7d 这类写法 time.ParseDuration 不支持
	if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && strings.HasSuffix(s, "d") {
		return time.Now().AddDate(0, 0, -n)
	}
	return time.Time{}
}
