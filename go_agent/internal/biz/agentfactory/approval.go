package agentfactory

// 人工审批凭据（P1-04）：把「批准这一次调用」和「永久放行这个工具」分开。
//
// 旧实现是 RunState.Approved map[string]bool，键只有能力名：批准过一次
// mcp:db__delete 之后，同一任务里任何参数的 delete 都不再问人。攻击面很直接——
// 第一次用无害参数骗到批准，第二次换成 id=999999 就自动放行。
//
// 现在一条凭据绑定三个维度（其余维度由载体隐含：checkpoint 按 task_id 存，
// 任务自带 user_id / tenant，跨任务读不到彼此的凭据）：
//
//	ability + canonical_args_hash + policy_version
//
// 参数变了、审批策略版本变了，凭据自然失效——不需要额外的撤销逻辑，
// 也不存在「忘了撤销」这条路径。凭据本身带 requester / approver / decision /
// reason / created_at / expires_at，就是一条可审计的授权记录。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/audit"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/pkg/identity"
)

const (
	// approvalPolicyVersion 审批策略版本。改动「什么需要审批」的规则时必须递增：
	// 旧版本下发出的凭据随之失效（宁可多问一次人，也不让旧策略的授权在新策略下继续生效）。
	approvalPolicyVersion = "approval/v2"
	// defaultApprovalTTL 凭据有效期：批准后超过该时长未执行，需要重新审批。
	// 挂起任务不会立刻被 worker 拾起，但也不该把一次批准变成长期通行证。
	defaultApprovalTTL = 24 * time.Hour
	// approvalArgsCap 写进挂起游标的参数快照上限（字节）。超出时只落哈希：
	// 校验不依赖快照（哈希在挂起时算好），快照只为人看，没必要把 checkpoint 撑爆。
	approvalArgsCap = 8192
)

// ApprovalGrant 一条审批凭据：谁、在哪个策略版本下、批准了哪一次具体调用。
type ApprovalGrant struct {
	Ability       string    `json:"ability"`
	ArgsHash      string    `json:"args_hash"`
	PolicyVersion string    `json:"policy_version"`
	Requester     string    `json:"requester"`
	Approver      string    `json:"approver"`
	Decision      string    `json:"decision"`
	Reason        string    `json:"reason,omitempty"`
	HighRisk      bool      `json:"high_risk,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	// SelfApproved 请求人自批（仅开发态 / 非高风险场景），审计里单独可见
	SelfApproved bool `json:"self_approved,omitempty"`
}

// approvalRequest 挂起点：记录「在等人批什么」。写进 checkpoint 的 cursor，
// 批准时从这里取回参数指纹，而不是从 event 或日志里猜。
type approvalRequest struct {
	Kind          string         `json:"kind"` // 固定 "approval"
	Ability       string         `json:"ability"`
	ArgsHash      string         `json:"args_hash"`
	Args          map[string]any `json:"args,omitempty"`
	ArgsOmitted   bool           `json:"args_omitted,omitempty"`
	Reason        string         `json:"reason"`
	HighRisk      bool           `json:"high_risk,omitempty"`
	AgentKey      string         `json:"agent_key,omitempty"`
	PolicyVersion string         `json:"policy_version"`
	CreatedAt     time.Time      `json:"created_at"`
}

// canonicalArgsHash 参数指纹：JSON 序列化后 sha256（encoding/json 对 map 键排序，
// 同一份参数无论字段顺序如何都得到同一个指纹）。
func canonicalArgsHash(args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		// 不可序列化的参数（理论不可达：来自 LLM JSON）：退化成 fmt 输出，
		// 至少保证「不同参数 → 不同指纹」这个性质还在
		b = []byte(fmt.Sprintf("%v", args))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:32]
}

// approvalKey 凭据的存储键：三个维度都进键，任一维度不匹配就是「没批准过」。
func approvalKey(ability, argsHash, policyVersion string) string {
	return ability + "#" + argsHash + "#" + policyVersion
}

// approvedCall 这次具体调用是否已被批准（且凭据未过期、同一策略版本）
func (st *RunState) approvedCall(ability string, args map[string]any) (ApprovalGrant, bool) {
	if st == nil || len(st.Approvals) == 0 {
		return ApprovalGrant{}, false
	}
	g, ok := st.Approvals[approvalKey(ability, canonicalArgsHash(args), approvalPolicyVersion)]
	if !ok {
		return ApprovalGrant{}, false
	}
	if g.PolicyVersion != approvalPolicyVersion {
		return ApprovalGrant{}, false
	}
	if !g.ExpiresAt.IsZero() && time.Now().After(g.ExpiresAt) {
		return ApprovalGrant{}, false
	}
	return g, true
}

// grant 写入一条凭据（覆盖同键旧凭据）
func (st *RunState) grant(g ApprovalGrant) {
	if st.Approvals == nil {
		st.Approvals = map[string]ApprovalGrant{}
	}
	st.Approvals[approvalKey(g.Ability, g.ArgsHash, g.PolicyVersion)] = g
}

// copyApprovals 深拷贝：计划波次的步骤各自拿到独立的状态副本，
// 否则并发步骤共用同一个 map（Go 的并发写 map 会直接 panic）。
func copyApprovals(in map[string]ApprovalGrant) map[string]ApprovalGrant {
	if len(in) == 0 {
		return map[string]ApprovalGrant{}
	}
	out := make(map[string]ApprovalGrant, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// toolHighRisk 静态高危判定：只看能力名，不看参数、不依赖风险模型。
// 风险模型可以误判、可以挂掉，这条规则是独立于它的兜底控制。
func toolHighRisk(ability string) bool {
	if !strings.HasPrefix(ability, CapToolPrefix) {
		return false
	}
	tool := strings.TrimPrefix(ability, CapToolPrefix)
	return mcp.ClassifyToolRisk(mcp.Tool{ToolName: tool}) == mcp.RiskDestructive
}

// humanGate 该 Agent 是否配置了人工闸门（能接住「转人工」的分支）
func humanGate(def *AgentDef) bool {
	if def == nil {
		return false
	}
	return len(def.RequireApproval) > 0 || strings.EqualFold(def.RiskPauseLevel, "high")
}

// approvalReason 这次调用是否需要人工审批，返回触发原因（空 = 不需要）。
//
// 顺序有意义：先看是否已就这次参数批准过，再看静态高危，最后看 Agent 声明。
func approvalReason(def *AgentDef, st *RunState, ability string, args map[string]any) string {
	if def == nil {
		return ""
	}
	if _, ok := st.approvedCall(ability, args); ok {
		return ""
	}
	if toolHighRisk(ability) {
		return "static_risk"
	}
	for _, p := range def.RequireApproval {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(ability, strings.TrimSuffix(p, "*")) {
				return "require_approval"
			}
			continue
		}
		if p == ability {
			return "require_approval"
		}
	}
	return ""
}

// gateApproval 人工审批闸门。
//
// 位置很关键：挂在 InvokeDispatch（授权之后、执行之前）而不是 runToolLoop。
// 三条执行路径（工具循环 / router / function_call 节点）都经 InvokeDispatch，
// 而只有工具循环会走 runToolLoop——把闸门放在工具循环里，router 与
// function_call 调用高危工具时根本不问人。
func (u *UseCase) gateApproval(ctx context.Context, rec *Recorder, ability string, args map[string]any) error {
	if rec == nil || rec.def == nil {
		// 没有 Agent 上下文时 authorizeCapability 已经拒绝，这里不重复判断
		return nil
	}
	reason := approvalReason(rec.def, rec.st, ability, args)
	if reason == "" {
		return nil
	}
	return u.pauseForApproval(ctx, rec, ability, args, reason, toolHighRisk(ability))
}

// pauseForApproval 挂起任务等人批：写游标 + checkpoint + paused 事件 + 审计。
// 返回 ErrApprovalNeeded，上层（executeTaskNow）据此把任务置 paused 并保留 checkpoint。
func (u *UseCase) pauseForApproval(ctx context.Context, rec *Recorder, ability string, args map[string]any, reason string, highRisk bool) error {
	if rec == nil {
		return ErrApprovalNeeded
	}
	if rec.st == nil {
		// router / function_call 等路径没有装配 RunState：就地补一个，
		// 否则挂起时无处记录游标，批准后也就无从续跑。
		rec.st = &RunState{Seen: map[string]string{}, Approvals: map[string]ApprovalGrant{}}
	}
	req := approvalRequest{
		Kind:          "approval",
		Ability:       ability,
		ArgsHash:      canonicalArgsHash(args),
		Reason:        reason,
		HighRisk:      highRisk,
		PolicyVersion: approvalPolicyVersion,
		CreatedAt:     time.Now(),
	}
	if b, err := json.Marshal(args); err == nil && len(b) <= approvalArgsCap {
		req.Args = args
	} else {
		req.ArgsOmitted = true
	}
	rec.st.Cursor = encodeApprovalCursor(req)
	rec.checkpoint(rec.st.Cursor)

	taskID := uint64(0)
	agentKey := ""
	if rec.task != nil {
		taskID = rec.task.ID
	}
	if rec.def != nil {
		agentKey = rec.def.AgentKey
	}
	rec.event(EventPaused, rec.turn, map[string]any{
		"ability": ability, "args": args, "args_hash": req.ArgsHash,
		"reason": reason, "high_risk": highRisk,
		"policy_version": approvalPolicyVersion, "agent": agentKey,
	})
	audit.Info(ctx, "agentfactory.approval.requested", "capability", ability, map[string]any{
		"task_id": taskID, "agent": agentKey, "reason": reason,
		"high_risk": highRisk, "args_hash": req.ArgsHash,
		"policy_version": approvalPolicyVersion,
	})
	return ErrApprovalNeeded
}

// encodeApprovalCursor 序列化挂起游标（失败返回空串：空游标批准时不写凭据，
// 任务会再次挂起重新问人——失败方向偏保守）
func encodeApprovalCursor(r approvalRequest) string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeApprovalCursor 解析挂起游标；非审批游标或旧格式（无 args_hash）返回 false。
func decodeApprovalCursor(cursor string) (approvalRequest, bool) {
	var r approvalRequest
	if strings.TrimSpace(cursor) == "" {
		return r, false
	}
	if err := json.Unmarshal([]byte(cursor), &r); err != nil {
		return r, false
	}
	if r.Kind != "approval" || r.Ability == "" || r.ArgsHash == "" || r.PolicyVersion == "" {
		return r, false
	}
	return r, true
}

// childApprovalError 子任务挂起时给父任务的错误：父任务不能把「等审批」
// 当成普通失败继续跑，也不能假装节点完成——显式失败并指明是哪个子任务在等。
func childApprovalError(child *Task, agentKey string) error {
	id := uint64(0)
	if child != nil {
		id = child.ID
	}
	return fmt.Errorf("%w: 子任务 #%d（%s）正在等待人工审批；下游节点未被激活，"+
		"请到任务中心批准该子任务（它会自行续跑），再重新提交父任务",
		ErrChildApprovalPending, id, agentKey)
}

// checkApprover 审批人资格：
//
//   - 请求人自批：高危操作一律拒绝（四眼原则），非高危允许（单机 / 小团队场景
//     否则无人可批）；开发态（认证关闭 + 本机）放行并打 SelfApproved 标记，
//     与 MCP 工具审批的既有策略保持一致；
//   - 非请求人：必须是管理员——审批是「独立审批人」的职责，不是同租户任意用户的权利。
func (u *UseCase) checkApprover(ctx context.Context, t *Task, req approvalRequest) (selfApproved bool, err error) {
	by := identity.UserID(ctx)
	self := strings.EqualFold(strings.TrimSpace(by), strings.TrimSpace(t.UserID))
	switch {
	case self && req.HighRisk && !identity.AdminAllowed(ctx):
		audit.Denied(ctx, "agentfactory.task.approve", "task", fmt.Sprint(t.ID), "self_approve_high_risk")
		return false, ErrApprovalSelfApprove
	case self:
		return true, nil
	case !identity.AdminAllowed(ctx):
		audit.Denied(ctx, "agentfactory.task.approve", "task", fmt.Sprint(t.ID), "approver_not_admin")
		return false, ErrApprovalForbidden
	}
	return false, nil
}
