// Package usage Cost Center（V2 第十二章）：追加式用量记录与聚合。
//
// 设计原则（对应文档 12.2）：
//   - 原始用量与计价分离：tokens 是事实，cost 是按「当时的计价规则」算出来的；
//   - 计价规则带版本：模型调价后不能拿新价格重算历史账单；
//   - 只追加不修改：任何更正都追加一条调整记录，而不是 UPDATE 原行。
//
// 记录维度：tenant / subject / agent / task / provider / model / kind。
// kind 用来区分 LLM token、embedding、rerank、MCP 调用等成本来源。
package usage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/pkg/principal"
)

// 成本来源
const (
	KindLLM       = "llm"
	KindEmbedding = "embedding"
	KindRerank    = "rerank"
	KindMCP       = "mcp"
	KindSkill     = "skill"
	KindWorker    = "worker"
)

var ErrInvalid = errors.New("用量记录参数不合法")

// Record 一条用量记录（追加式）。
type Record struct {
	ID uint64 `json:"id"`

	TenantID  string `json:"tenant_id"`
	SubjectID string `json:"subject_id"`

	AgentID string `json:"agent_id"`
	TaskID  string `json:"task_id"`

	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Kind 成本来源：llm / embedding / rerank / mcp / skill / worker
	Kind string `json:"kind"`

	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`

	// CostAmount 估算成本（用整数微单位避免浮点误差：1 元 = 1_000_000 微元）
	CostMicros int64  `json:"cost_micros"`
	Currency   string `json:"currency"`
	// PriceVersion 计价规则版本（模型调价后必须递增）
	PriceVersion string `json:"price_version"`

	CreatedAt time.Time `json:"created_at"`
}

// Filter 聚合查询条件
type Filter struct {
	TenantID  string
	AgentID   string
	SubjectID string
	// TaskID 单任务成本：与 trace 用同一个 task_id 串联（任务详情页的数据源）
	TaskID string
	Kind   string
	Since  time.Time
	Until  time.Time
	// GroupBy tenant | agent | subject | task | model | kind | day
	GroupBy string
}

// Bucket 聚合结果
type Bucket struct {
	Key string `gorm:"column:bucket_key" json:"key"`

	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	CostMicros   int64 `json:"cost_micros"`
	Calls        int64 `json:"calls"`
}

// Repo 用量仓储。
type Repo interface {
	Add(ctx context.Context, r *Record) error
	Summary(ctx context.Context, f Filter) ([]Bucket, error)
}

// UseCase 成本中心用例。
type UseCase struct {
	repo Repo
}

// New 构建用例。
func New(repo Repo) *UseCase { return &UseCase{repo: repo} }

// Record 记录一条用量。
//
// 身份规则：普通调用方只能记到自己名下；平台管理员可以代办（例如 worker 代记）。
// 租户永远由 Principal 决定，不接受请求体里的 tenant_id。
func (u *UseCase) Record(ctx context.Context, p *principal.Principal, r *Record) (*Record, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: 缺少调用方身份", ErrInvalid)
	}
	if r == nil {
		return nil, fmt.Errorf("%w: 记录为空", ErrInvalid)
	}
	if r.Kind == "" {
		r.Kind = KindLLM
	}
	if !validKinds[r.Kind] {
		return nil, fmt.Errorf("%w: 未知成本来源 %q", ErrInvalid, r.Kind)
	}
	if r.TotalTokens == 0 {
		r.TotalTokens = r.InputTokens + r.OutputTokens
	}
	if r.TotalTokens < 0 || r.CostMicros < 0 {
		return nil, fmt.Errorf("%w: token / 成本不能为负", ErrInvalid)
	}
	if r.TenantID == "" {
		r.TenantID = p.TenantID
	}
	if r.TenantID != p.TenantID && !p.Admin {
		return nil, fmt.Errorf("%w: 不能给其他租户记账", ErrInvalid)
	}
	if r.SubjectID == "" {
		r.SubjectID = p.SubjectID
	}
	if r.SubjectID != p.SubjectID && !p.Admin {
		return nil, fmt.Errorf("%w: 不能给别人记账", ErrInvalid)
	}
	if r.Currency == "" {
		r.Currency = "CNY"
	}
	r.CreatedAt = time.Now()
	if err := u.repo.Add(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// Summary 聚合查询（租户隔离由调用方身份决定）。
func (u *UseCase) Summary(ctx context.Context, p *principal.Principal, f Filter) ([]Bucket, error) {
	if p == nil {
		return nil, ErrInvalid
	}
	if f.TenantID == "" {
		f.TenantID = p.TenantID
	}
	if f.TenantID != p.TenantID && !p.Admin {
		return nil, fmt.Errorf("%w: 不能跨租户查看成本", ErrInvalid)
	}
	switch f.GroupBy {
	case "", "agent", "model", "kind", "day", "tenant", "subject", "task":
	default:
		return nil, fmt.Errorf("%w: 未知分组 %q", ErrInvalid, f.GroupBy)
	}
	return u.repo.Summary(ctx, f)
}

var validKinds = map[string]bool{
	KindLLM: true, KindEmbedding: true, KindRerank: true,
	KindMCP: true, KindSkill: true, KindWorker: true,
}

// KindOf 把外部字符串归一化成成本来源（未知值按 llm 处理）。
func KindOf(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if validKinds[s] {
		return s
	}
	return KindLLM
}
