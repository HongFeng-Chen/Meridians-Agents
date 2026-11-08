package service

// MCP 工具调用的风险闸门与审批面。
//
// 分工：
//   - biz/mcp：风险分级（risk.go）与审批单（approval.go），纯内存语义；
//   - 这里：把「HTTP 请求 → 调用」的边界接上——读审批令牌、开单、拒绝并给出
//     可操作的提示；审批接口本身也要管理员，且不能自己批自己。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/pkg/identity"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
)

// ApprovalTokenHeader 审批令牌请求头。
//
// 用请求头而不是改 proto：调用方（含前端 / 脚本）不需要重新生成协议，
// curl 加一个 -H 就能完成「申请 → 批准 → 重放」的闭环。
const ApprovalTokenHeader = "X-MCP-Approval-Token"

// guardToolCall 调用前的风险闸门。
//
// 返回值：
//   - nil：放行（低风险，或令牌校验通过并已消费）；
//   - 428：需要审批，错误信息里带审批单号（调用方拿它去走批准流程）；
//   - 403：令牌无效 / 过期 / 已用过 / 参数变了 / 已被驳回。
func (s *MCPService) guardToolCall(ctx context.Context, toolID uint64, uniqueName string, args map[string]any) (*mcp.Tool, error) {
	t, err := s.uc.ResolveTool(ctx, toolID, uniqueName)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	if !mcp.ApprovalRequired(*t) {
		return t, nil
	}

	user, tenant := identity.UserID(ctx), identity.TenantID(ctx)
	token := approvalTokenFrom(ctx)
	risk := mcp.ClassifyToolRisk(*t)

	if token == "" {
		a, err := mcp.RequestToolApproval(*t, args, user, tenant)
		if err != nil {
			return nil, mapMCPErr(err)
		}
		auditToolCall(ctx, *t, risk, "approval_requested", a.ID, "")
		return nil, kerrors.New(428, "APPROVAL_REQUIRED", fmt.Sprintf(
			"工具 %s 的风险等级为 %s，需要人工审批。已生成审批单 %s（%s 内有效）："+
				"请管理员调用 POST /api/mcp/tool-approvals/%s/approve 批准，"+
				"然后带请求头 %s: %s 重放本次调用（参数不可修改，令牌一次性）",
			t.UniqueName, risk, a.ID, mcp.CurrentApprovalPolicy().TTL.Round(1e9),
			a.ID, ApprovalTokenHeader, a.ID,
		))
	}

	if err := mcp.ConsumeToolApproval(token, user, tenant, *t, args); err != nil {
		auditToolCall(ctx, *t, risk, "approval_rejected", token, err.Error())
		return nil, kerrors.New(403, "APPROVAL_INVALID", err.Error())
	}
	auditToolCall(ctx, *t, risk, "approval_consumed", token, "")
	return t, nil
}

// approvalTokenFrom 从 HTTP 请求头取审批令牌；gRPC 调用时为空（走 428）。
func approvalTokenFrom(ctx context.Context) string {
	req, ok := khttp.RequestFromServerContext(ctx)
	if !ok || req == nil {
		return ""
	}
	if v := strings.TrimSpace(req.Header.Get(ApprovalTokenHeader)); v != "" {
		return v
	}
	// 兼容一个更短的名字，方便脚本里手写
	return strings.TrimSpace(req.Header.Get("X-Approval-Token"))
}

// 领域错误 → 传输错误
func mapApprovalErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mcp.ErrApprovalInvalid):
		return kerrors.New(400, "APPROVAL_INVALID", err.Error())
	case errors.Is(err, mcp.ErrApprovalDenied):
		return kerrors.New(403, "APPROVAL_DENIED", err.Error())
	case errors.Is(err, mcp.ErrApprovalSelfApprove):
		return kerrors.New(403, "APPROVAL_SELF_APPROVE", err.Error())
	default:
		return kerrors.New(500, "APPROVAL_FAILED", err.Error())
	}
}

// —— 审批面（供 /api/mcp/tool-approvals 路由调用）——

// ListToolApprovals 审批单列表。待办列表是运维视图：需要管理员。
func (s *MCPService) ListToolApprovals(ctx context.Context, status, limit string) ([]*mcp.ToolApproval, error) {
	if err := requireAdmin(ctx, "mcp.tool.approval.list"); err != nil {
		return nil, err
	}
	return mcp.ListToolApprovals(status, limit), nil
}

// DecideToolApproval 批准 / 驳回。approver 取当前登录身份，
// 「不能自己批自己」由 biz 层保证（四眼原则）。
func (s *MCPService) DecideToolApproval(ctx context.Context, id, comment string, approve bool) (*mcp.ToolApproval, error) {
	action := "mcp.tool.approval.deny"
	if approve {
		action = "mcp.tool.approval.approve"
	}
	if err := requireAdmin(ctx, action); err != nil {
		return nil, err
	}
	a, err := mcp.DecideToolApproval(id, identity.UserID(ctx), approve, comment)
	if err != nil {
		return nil, mapApprovalErr(err)
	}
	auditEvent(ctx, "mcp.tool.approval", action, map[string]any{
		"approval_id": a.ID,
		"tool":        a.ToolName,
		"risk":        string(a.Risk),
		"requester":   a.UserID,
		"decision":    string(a.Status),
		"comment":     a.Comment,
	})
	return a, nil
}
