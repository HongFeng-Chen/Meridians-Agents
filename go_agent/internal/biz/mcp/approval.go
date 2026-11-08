package mcp

// 工具调用审批：把「一次高危调用」变成「一次显式的人工决定」。
//
// 为什么不能只靠角色：MCP 管理面里 /api/mcp/tools/{id}/call 是给人调试用的，
// 它调的是真实业务接口——`delete_repository` 不因为调用者是管理员就变得安全。
// 管理员的身份解决的是「你有没有权限改配置」，审批解决的是「这次操作你是否
// 明确同意」。两者是不同维度，所以这里单独做一次性、绑定参数、绑定人的令牌。
//
// 流程（两段式，不需要前端配合也能用 curl 跑通）：
//
//	1. 直接调用工具 → 若风险等级达到阈值，返回 428 + 审批单 ID（不执行工具）；
//	2. 管理员 POST /api/mcp/tool-approvals/{id}/approve 批准；
//	3. 调用方带 X-MCP-Approval-Token: <id> 重放同一次调用 → 校验并一次性消费。
//
// 令牌约束：绑定 (请求人, 租户, 工具, 参数指纹)，有效期默认 5 分钟，只能用一次，
// 批准人不能是请求人自己（四眼原则）。参数被改过则指纹不匹配，必须重新申请。
//
// 存储：进程内 map。单实例部署够用；多副本部署需要换成 Redis（接口已抽出
// ApprovalStore，替换实现即可）。这是有意的取舍——先把语义做对，再谈分布式。
// 审批单不进 audit 之外的地方，也不落 MySQL，避免半成品状态污染业务表。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 审批策略与错误
var (
	// ErrApprovalRequired 调用被挂起，需要审批
	ErrApprovalRequired = errors.New("该工具调用需要人工审批")
	// ErrApprovalInvalid 令牌无效 / 过期 / 已被使用 / 与本次调用不匹配
	ErrApprovalInvalid = errors.New("审批令牌无效")
	// ErrApprovalDenied 审批被驳回
	ErrApprovalDenied = errors.New("审批已被驳回")
	// ErrApprovalSelfApprove 不能批准自己发起的调用
	ErrApprovalSelfApprove = errors.New("不能批准自己发起的调用（四眼原则）")
)

// ApprovalStatus 审批单状态
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
	ApprovalConsumed ApprovalStatus = "consumed"
	ApprovalExpired  ApprovalStatus = "expired"
)

// ToolApproval 一张审批单
type ToolApproval struct {
	ID          string         `json:"id"`
	ToolID      uint64         `json:"tool_id"`
	ToolName    string         `json:"tool_name"`
	ServerID    uint64         `json:"mcp_server_id"`
	Risk        RiskLevel      `json:"risk"`
	UserID      string         `json:"user_id"`
	TenantID    string         `json:"tenant_id"`
	ArgsDigest  string         `json:"args_digest"`
	ArgsBrief   string         `json:"args_brief,omitempty"`
	Status      ApprovalStatus `json:"status"`
	RequestedAt time.Time      `json:"requested_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
	DecidedAt   time.Time      `json:"decided_at,omitempty"`
	DecidedBy   string         `json:"decided_by,omitempty"`
	Comment     string         `json:"comment,omitempty"`
}

// ApprovalStore 审批单存储（进程内实现见 memoryApprovalStore）
type ApprovalStore interface {
	Create(a *ToolApproval) error
	Get(id string) (*ToolApproval, bool)
	List(status ApprovalStatus, limit int) []*ToolApproval
	Update(id string, fn func(*ToolApproval) error) (*ToolApproval, error)
	// Sweep 把过期未决的单子标记为 expired 并清理过旧的已终结单子
	Sweep(now time.Time, keep time.Duration)
}

// —— 策略 ——

// approvalPolicy 审批策略
type approvalPolicy struct {
	// Threshold 达到该等级即需要审批；空表示不启用审批（none）
	Threshold RiskLevel
	// TTL 审批单有效期
	TTL time.Duration
}

var (
	approvalPolicyOnce sync.Once
	approvalPolicyVal  approvalPolicy
)

// CurrentApprovalPolicy 读取审批策略（环境变量，进程启动读一次）
//
//	MCP_TOOL_APPROVAL_LEVEL=destructive   # none | write | destructive | all（默认 destructive）
//	MCP_TOOL_APPROVAL_TTL=300             # 审批单有效期（秒，默认 300）
func CurrentApprovalPolicy() approvalPolicy {
	approvalPolicyOnce.Do(func() {
		p := approvalPolicy{Threshold: RiskDestructive, TTL: 5 * time.Minute}
		switch strings.ToLower(strings.TrimSpace(os.Getenv("MCP_TOOL_APPROVAL_LEVEL"))) {
		case "none", "off", "false", "0":
			p.Threshold = ""
		case "write":
			p.Threshold = RiskWrite
		case "all", "read":
			p.Threshold = RiskRead
		case "destructive", "default", "":
			p.Threshold = RiskDestructive
		default:
			p.Threshold = RiskDestructive
		}
		if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("MCP_TOOL_APPROVAL_TTL"))); err == nil && n > 0 {
			p.TTL = time.Duration(n) * time.Second
		}
		approvalPolicyVal = p
	})
	return approvalPolicyVal
}

// setApprovalPolicy 单测用：覆盖策略
func setApprovalPolicy(p approvalPolicy) { approvalPolicyVal = p; approvalPolicyOnce.Do(func() {}) }

// ApprovalRequired 该工具当前是否需要审批
func ApprovalRequired(t Tool) bool {
	p := CurrentApprovalPolicy()
	if p.Threshold == "" {
		return false
	}
	return ClassifyToolRisk(t).Rank() >= p.Threshold.Rank()
}

// —— 存储实现 ——

type memoryApprovalStore struct {
	mu    sync.Mutex
	items map[string]*ToolApproval
}

var (
	approvalStoreOnce sync.Once
	approvalStoreVal  ApprovalStore
)

// DefaultApprovalStore 进程级审批单存储
func DefaultApprovalStore() ApprovalStore {
	approvalStoreOnce.Do(func() {
		approvalStoreVal = &memoryApprovalStore{items: make(map[string]*ToolApproval)}
	})
	return approvalStoreVal
}

// SetApprovalStore 替换存储实现（Redis 版本 / 单测注入）
func SetApprovalStore(s ApprovalStore) { approvalStoreVal = s; approvalStoreOnce.Do(func() {}) }

func (m *memoryApprovalStore) Create(a *ToolApproval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[a.ID]; ok {
		return fmt.Errorf("审批单 %s 已存在", a.ID)
	}
	m.items[a.ID] = a
	return nil
}

func (m *memoryApprovalStore) Get(id string) (*ToolApproval, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.items[id]
	if !ok {
		return nil, false
	}
	// 读时顺手把过期未决的单子标记为 expired：不依赖外部调度器，
	// 单实例进程里这就是最可靠的位置（多副本部署需换成 Redis + 定期任务）。
	if a.Status == ApprovalPending && time.Now().After(a.ExpiresAt) {
		a.Status = ApprovalExpired
	}
	cp := *a
	return &cp, true
}

func (m *memoryApprovalStore) List(status ApprovalStatus, limit int) []*ToolApproval {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	now := time.Now()
	out := make([]*ToolApproval, 0, len(m.items))
	for _, a := range m.items {
		if a.Status == ApprovalPending && now.After(a.ExpiresAt) {
			a.Status = ApprovalExpired
		}
		if status != "" && a.Status != status {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.After(out[j].RequestedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m *memoryApprovalStore) Update(id string, fn func(*ToolApproval) error) (*ToolApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.items[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s 不存在", ErrApprovalInvalid, id)
	}
	if err := fn(a); err != nil {
		return nil, err
	}
	cp := *a
	return &cp, nil
}

func (m *memoryApprovalStore) Sweep(now time.Time, keep time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, a := range m.items {
		if a.Status == ApprovalPending && now.After(a.ExpiresAt) {
			a.Status = ApprovalExpired
			continue
		}
		if a.Status != ApprovalPending && now.Sub(a.RequestedAt) > keep {
			delete(m.items, id)
		}
	}
}

// —— 对外用例 ——

// RequestToolApproval 为一次调用开审批单。
// args 会算成指纹：审批的是「这一次具体调用」，改了参数就得重新申请。
func RequestToolApproval(t Tool, args map[string]any, userID, tenantID string) (*ToolApproval, error) {
	p := CurrentApprovalPolicy()
	now := time.Now()
	a := &ToolApproval{
		ID:          newApprovalID(),
		ToolID:      t.ID,
		ToolName:    t.UniqueName,
		ServerID:    t.ServerID,
		Risk:        ClassifyToolRisk(t),
		UserID:      userID,
		TenantID:    tenantID,
		ArgsDigest:  argsDigest(args),
		ArgsBrief:   argsBrief(args),
		Status:      ApprovalPending,
		RequestedAt: now,
		ExpiresAt:   now.Add(p.TTL),
	}
	if err := DefaultApprovalStore().Create(a); err != nil {
		return nil, err
	}
	return a, nil
}

// ConsumeToolApproval 校验并消费审批令牌（一次性）。
func ConsumeToolApproval(id, userID, tenantID string, t Tool, args map[string]any) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: 缺少审批令牌", ErrApprovalInvalid)
	}
	digest := argsDigest(args)
	_, err := DefaultApprovalStore().Update(id, func(a *ToolApproval) error {
		switch a.Status {
		case ApprovalDenied:
			return ErrApprovalDenied
		case ApprovalConsumed:
			return fmt.Errorf("%w: 该审批已被使用（一次性）", ErrApprovalInvalid)
		case ApprovalExpired:
			return fmt.Errorf("%w: 审批单已过期", ErrApprovalInvalid)
		case ApprovalPending:
			return fmt.Errorf("%w: 审批尚未通过", ErrApprovalInvalid)
		case ApprovalApproved:
			// 继续校验绑定关系
		default:
			return fmt.Errorf("%w: 未知状态 %s", ErrApprovalInvalid, a.Status)
		}
		if time.Now().After(a.ExpiresAt) {
			a.Status = ApprovalExpired
			return fmt.Errorf("%w: 审批单已过期", ErrApprovalInvalid)
		}
		if a.UserID != userID || a.TenantID != tenantID {
			return fmt.Errorf("%w: 审批单属于其他调用方", ErrApprovalInvalid)
		}
		if a.ToolName != t.UniqueName {
			return fmt.Errorf("%w: 审批单针对的是工具 %s", ErrApprovalInvalid, a.ToolName)
		}
		if a.ArgsDigest != digest {
			return fmt.Errorf("%w: 参数与审批时不一致，请重新申请", ErrApprovalInvalid)
		}
		a.Status = ApprovalConsumed
		return nil
	})
	return err
}

// DecideToolApproval 批准或驳回（approver 必须是管理员，由调用方保证；
// 这里只保证「不能自己批自己」）。
func DecideToolApproval(id, approver string, approve bool, comment string) (*ToolApproval, error) {
	return DefaultApprovalStore().Update(id, func(a *ToolApproval) error {
		if a.Status != ApprovalPending {
			return fmt.Errorf("%w: 当前状态 %s 不能审批", ErrApprovalInvalid, a.Status)
		}
		if time.Now().After(a.ExpiresAt) {
			a.Status = ApprovalExpired
			return fmt.Errorf("%w: 审批单已过期", ErrApprovalInvalid)
		}
		if strings.TrimSpace(approver) == "" {
			return fmt.Errorf("%w: 审批人不能为空", ErrApprovalInvalid)
		}
		if strings.EqualFold(strings.TrimSpace(approver), a.UserID) {
			return ErrApprovalSelfApprove
		}
		if approve {
			a.Status = ApprovalApproved
		} else {
			a.Status = ApprovalDenied
		}
		a.DecidedAt = time.Now()
		a.DecidedBy = approver
		a.Comment = strings.TrimSpace(comment)
		return nil
	})
}

// ListToolApprovals 审批单列表（按申请时间倒序）
func ListToolApprovals(status, limit string) []*ToolApproval {
	n, _ := strconv.Atoi(strings.TrimSpace(limit))
	return DefaultApprovalStore().List(ApprovalStatus(strings.ToLower(strings.TrimSpace(status))), n)
}

// SweepToolApprovals 清理过期单子（随调度器周期性调用）
func SweepToolApprovals() {
	DefaultApprovalStore().Sweep(time.Now(), time.Hour)
}

// —— 工具函数 ——

// newApprovalID 生成不可猜测的审批单号（128 bit 随机）
func newApprovalID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 兜底：极少数环境拿不到熵，用时间戳保证可用性（安全性依赖 TTL + 绑定校验）
		return "ap" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "ap" + hex.EncodeToString(b)
}

// argsDigest 参数指纹：对 key 排序后序列化，保证同样语义得到同样指纹
func argsDigest(args map[string]any) string {
	if len(args) == 0 {
		return "empty"
	}
	raw, err := json.Marshal(canonicalValue(args))
	if err != nil {
		return "unhashable"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// canonicalValue 递归排序 map key（encoding/json 本身对 map 有序，这里只处理嵌套 any）
func canonicalValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = canonicalValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = canonicalValue(t[i])
		}
		return out
	default:
		return v
	}
}

// argsBrief 参数摘要（审批列表给人看，截断到 300 字符）
func argsBrief(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	s := string(raw)
	if len([]rune(s)) > 300 {
		s = string([]rune(s)[:300]) + "…"
	}
	return s
}
