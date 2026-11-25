// Package skill 技能管理域：技能清单存 MySQL，支持在线启停、多文件包、
// 从技能市场（skillhub）搜索与下载安装，并可把启用技能落盘供运行时读取。
//
// 设计要点：
//   - 技能以 name 为稳定标识，重复安装视为升级（保留启用状态由入参决定）；
//   - 技能文件路径必须在技能目录内（拒绝绝对路径与 ..）；
//   - skillhub 地址存 MySQL（skill_hubs），conf 只提供默认兜底；
//   - 落盘是可选能力：sync_dir 为空时只存数据库。
package skill

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/pkg/egress"
)

// 来源
const (
	SourceHub     = "hub"
	SourceUpload  = "upload"
	SourceURL     = "url"
	SourceBuiltin = "builtin"
)

// 领域错误
var (
	ErrSkillNotFound = errors.New("技能不存在")
	ErrHubNotFound   = errors.New("技能市场不存在")
	ErrNoHub         = errors.New("没有可用的技能市场，请先在 skill_hubs 配置或设置 data.skill.hubBaseURL")
	ErrInvalidName   = errors.New("技能名称不合法（小写字母或数字开头，可含 . _ -，最长 64 字符）")
	ErrEmptyContent  = errors.New("技能内容不能为空（SKILL.md 内容或 files 至少一项）")
	ErrInvalidFile   = errors.New("技能文件路径不合法或超出大小限制")
	ErrHubResponse   = errors.New("技能市场返回内容不合法")
	ErrInvalidURL    = errors.New("技能包地址不合法（只支持 http/https）")

	// 运行时错误（V2 Skill Runtime）
	ErrSkillDisabled     = errors.New("技能已停用，启用后才能执行")
	ErrNotExecutable     = errors.New("该技能是本地指令包（local），没有远程执行配置")
	ErrNoWorker          = errors.New("没有可用的技能执行实例")
	ErrExecutionNotFound = errors.New("技能执行记录不存在")
	ErrExecutionCanceled = errors.New("技能执行已取消")
	ErrInvalidMode       = errors.New("执行模式不合法（local / remote / async）")
)

// 执行模式：技能「怎么执行」与「是什么」解耦，由 Skill Runtime 统一分发。
//
//	local  —— 进程内：把 SKILL.md 作为指令注入上下文（V1 行为，默认）
//	remote —— 同步远程：调度到某个 Worker 实例，等结果
//	async  —— 异步分布式：先入队拿 execution_id，由派发协程投递给 Worker
const (
	ModeLocal  = "local"
	ModeRemote = "remote"
	ModeAsync  = "async"
)

// 执行状态
const (
	ExecQueued    = "queued"
	ExecRunning   = "running"
	ExecSucceeded = "succeeded"
	ExecFailed    = "failed"
	ExecTimeout   = "timeout"
	ExecCanceled  = "canceled"
)

// Worker 实例状态
const (
	InstanceOnline   = "online"
	InstanceDraining = "draining" // 不再接新任务，等在途任务跑完
	InstanceOffline  = "offline"
)

// 运行时默认值
const (
	DefaultRemoteTimeoutSec = 60
	DefaultAsyncTimeoutSec  = 300
	DefaultMaxRetry         = 1
	DefaultMaxConcurrency   = 4
	DefaultBreakerThreshold = 3 // 连续失败多少次后熔断该实例
	// 执行记录保留时长（超过由 PurgeExecutions 清理）
	DefaultExecutionRetention = 7 * 24 * time.Hour
)

// ExecutionSpec 技能的执行配置（skills.execution_spec 列，JSON）。
//
// 关键设计：LLM 只看到「逻辑技能名 + 参数 Schema」，实例地址、重试、超时
// 都在这里，由运行时解析——换 Worker、扩缩容、灰度升级都不需要动提示词。
type ExecutionSpec struct {
	// Mode local | remote | async，空等价 local
	Mode string `json:"mode,omitempty"`
	// Protocol 远程协议，目前支持 http（Worker 侧统一 /execute 端点）
	Protocol string `json:"protocol,omitempty"`
	// Endpoint 直连地址（无注册中心时的单实例部署）；有实例注册时优先用实例
	Endpoint string `json:"endpoint,omitempty"`
	// Handler Worker 侧的实现名，缺省等于技能名。
	// 平台技能名是「用户看到的能力标识」（可重命名），Handler 是「代码里的函数/路由键」，
	// 两者解耦后，改技能标题/前缀不必改 Worker 代码。
	Handler string `json:"handler,omitempty"`
	// InputSchema 参数 JSON Schema 原文，进 Agent 能力描述，供 LLM 生成参数
	InputSchema string `json:"input_schema,omitempty"`
	// OutputSchema 输出约定（文档性质，便于前端/调用方理解）
	OutputSchema string `json:"output_schema,omitempty"`
	// TimeoutSec 单次调用超时（秒），0 取默认值
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// Retry 失败重试次数（换实例重试），0 取默认值 1；-1 表示不重试
	Retry int `json:"retry,omitempty"`
	// Concurrency 单实例并发上限（调度背压用）
	Concurrency int `json:"concurrency,omitempty"`
	// Idempotent 是否按幂等键去重（同键成功结果直接回放）
	Idempotent bool `json:"idempotent,omitempty"`
	// RequiredScopes 预留的权限范围（如 file:read），由调用方/网关校验
	RequiredScopes []string `json:"required_scopes,omitempty"`
}

// Normalized 补默认值并归一化；mode 为空视为 local。
func (e ExecutionSpec) Normalized() ExecutionSpec {
	out := e
	out.Mode = strings.ToLower(strings.TrimSpace(out.Mode))
	switch out.Mode {
	case "":
		out.Mode = ModeLocal
	case ModeLocal, ModeRemote, ModeAsync:
	default:
		out.Mode = ModeLocal
	}
	out.Protocol = strings.ToLower(strings.TrimSpace(out.Protocol))
	if out.Protocol == "" {
		out.Protocol = "http"
	}
	out.Endpoint = strings.TrimRight(strings.TrimSpace(out.Endpoint), "/")
	out.Handler = strings.TrimSpace(out.Handler)
	if out.TimeoutSec <= 0 {
		if out.Mode == ModeAsync {
			out.TimeoutSec = DefaultAsyncTimeoutSec
		} else {
			out.TimeoutSec = DefaultRemoteTimeoutSec
		}
	}
	if out.Concurrency <= 0 {
		out.Concurrency = DefaultMaxConcurrency
	}
	return out
}

// Valid 执行配置是否合法（安装时校验，避免入库后运行时才炸）
func (e ExecutionSpec) Valid() error {
	m := strings.ToLower(strings.TrimSpace(e.Mode))
	switch m {
	case "", ModeLocal, ModeRemote, ModeAsync:
	default:
		return fmt.Errorf("%w: %s", ErrInvalidMode, e.Mode)
	}
	if m == ModeRemote || m == ModeAsync {
		ep := strings.TrimSpace(e.Endpoint)
		// 允许没有 endpoint：靠实例注册表发现 Worker
		if ep != "" {
			if err := egress.ValidateURL(ep); err != nil {
				return fmt.Errorf("%w: endpoint %q 不可用（%v）", ErrInvalidURL, ep, err)
			}
		}
	}
	if e.TimeoutSec < 0 || e.TimeoutSec > 3600 {
		return fmt.Errorf("%w: timeout_sec 需在 0~3600 之间", ErrInvalidMode)
	}
	return nil
}

// Skill 技能（DO）
type Skill struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`
	SourceURL   string   `json:"source_url"`
	Enabled     bool     `json:"enabled"`
	Checksum    string   `json:"checksum"`
	Content     string   `json:"content,omitempty"` // SKILL.md
	Files       []File   `json:"files,omitempty"`
	FileCount   int      `json:"file_count"`
	Size        int      `json:"size"`
	// Execution 执行配置：决定这个技能是「指令包」还是「远程能力」
	Execution   ExecutionSpec `json:"execution"`
	InstalledAt time.Time     `json:"installed_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// Mode 归一化后的执行模式（local / remote / async）
func (s Skill) Mode() string { return s.Execution.Normalized().Mode }

// Executable 是否是远程/异步技能（需要 Worker 执行）
func (s Skill) Executable() bool {
	m := s.Mode()
	return m == ModeRemote || m == ModeAsync
}

// File 技能附带文件（文本）
type File struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Size    int    `json:"size"`
}

// Hub 技能市场数据源（DO）
type Hub struct {
	Name        string    `json:"name"`
	BaseURL     string    `json:"base_url"`
	Token       string    `json:"-"`
	HasToken    bool      `json:"has_token"`
	Enabled     bool      `json:"enabled"`
	IsDefault   bool      `json:"is_default"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Masked 脱敏副本（token 只显示末 4 位）
func (h Hub) Masked() Hub {
	h.HasToken = strings.TrimSpace(h.Token) != ""
	h.Token = ""
	return h
}

// HubSkill 市场里的技能条目（搜索结果）
type HubSkill struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Downloads   int      `json:"downloads"`
	Installed   bool     `json:"installed"`
	Hub         string   `json:"hub"`
}

// Filter 列表过滤
type Filter struct {
	Keyword  string
	Category string
	Source   string
	Enabled  *bool
	Limit    int
}

// InstallInput 手动安装/上传技能
type InstallInput struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Content     string   `json:"content"`
	Files       []File   `json:"files"`
	Enabled     *bool    `json:"enabled"`
	// Execution 执行配置：留空 = local（只把 SKILL.md 注入上下文）
	Execution *ExecutionSpec `json:"execution"`
}

// RegisterInstanceInput Worker 注册入参（Worker 启动时调用；同 ID 重复注册 = 更新）
type RegisterInstanceInput struct {
	SkillName      string   `json:"skill_name"`
	ID             string   `json:"id"`
	Endpoint       string   `json:"endpoint"`
	Version        string   `json:"version"`
	Region         string   `json:"region"`
	Labels         []string `json:"labels"`
	Weight         int      `json:"weight"`
	MaxConcurrency int      `json:"max_concurrency"`
}

// ImportFromURLInput 从技能包 URL 安装（zip / SKILL.md / JSON 包）
type ImportFromURLInput struct {
	URL     string `json:"url"`
	Name    string `json:"name"` // 包内没有名字时必填
	Version string `json:"version"`
	Token   string `json:"token"` // 私有包：Authorization: Bearer <token>
	Enable  *bool  `json:"enable"`
}

// InstallFromHubInput 从技能市场安装
type InstallFromHubInput struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Hub     string `json:"hub"`
	Enable  *bool  `json:"enable"`
}

// HubInput 新建/修改技能市场
type HubInput struct {
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	Token       string `json:"token"`
	Enabled     *bool  `json:"enabled"`
	IsDefault   bool   `json:"is_default"`
	Description string `json:"description"`
}

// HubPatch 修改技能市场（指针字段为空表示不改）
type HubPatch struct {
	BaseURL     *string `json:"base_url"`
	Token       *string `json:"token"`
	Enabled     *bool   `json:"enabled"`
	IsDefault   *bool   `json:"is_default"`
	Description *string `json:"description"`
}

// SyncResult 落盘结果
type SyncResult struct {
	Dir     string   `json:"dir"`
	Synced  []string `json:"synced"`
	Removed []string `json:"removed"`
}

// Stats 规模统计
type Stats struct {
	Skills  int64 `json:"skills"`
	Enabled int64 `json:"enabled"`
	Hubs    int64 `json:"hubs"`
}

// —— 运行时：Worker 实例与执行记录 ——

// Instance 一个 Skill Worker 实例（skill_instances 表的一行）。
//
// 一个技能可以有多个实例（多副本 / 多地域 / 多资源规格），实例由 Worker 侧
// 启动时注册、按周期心跳；注册中心只存元数据与运行指标，不代理调用。
type Instance struct {
	// ID Worker 自报的唯一标识（如 hostname:port 或容器 ID）
	ID        string   `json:"id"`
	SkillName string   `json:"skill_name"`
	Endpoint  string   `json:"endpoint"`
	Version   string   `json:"version"`
	Region    string   `json:"region"`
	Labels    []string `json:"labels"`
	// Weight 调度权重（越大越优先），默认 1
	Weight int `json:"weight"`
	// MaxConcurrency 该实例允许的在途任务数
	MaxConcurrency int `json:"max_concurrency"`
	// Inflight 当前在途任务数（心跳上报）
	Inflight int `json:"inflight"`
	// Status online / draining / offline
	Status string `json:"status"`
	// LastHeartbeat 最近一次心跳；超过 TTL 由调度器视为离线
	LastHeartbeat time.Time `json:"last_heartbeat"`
	// LatencyMS 最近一次成功调用的耗时（EWMA）
	LatencyMS int64 `json:"latency_ms"`
	// Failures 连续失败次数（成功后清零），达到阈值即熔断
	Failures int `json:"failures"`
	// Successes 累计成功调用数
	Successes int64     `json:"successes"`
	LastError string    `json:"last_error"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Healthy 是否可接受新任务（状态 + 熔断；心跳过期由 Scheduler 用 TTL 判定）
func (i Instance) Healthy(breaker int) bool {
	if i.Status == InstanceOffline || i.Status == InstanceDraining {
		return false
	}
	if breaker > 0 && i.Failures >= breaker {
		return false
	}
	return i.Inflight < maxInt(i.MaxConcurrency, 1)
}

// Capacity 负载水位（0~1，越小越空闲）
func (i Instance) Capacity() float64 {
	return float64(i.Inflight) / float64(maxInt(i.MaxConcurrency, 1))
}

// InstanceHeartbeat Worker 心跳上报
type InstanceHeartbeat struct {
	SkillName string
	ID        string
	Inflight  int
	LatencyMS int64
	Status    string
}

// Execution 一次技能执行记录（skill_executions 表）。
//
// 这是「执行面」的账本：谁在什么任务里、用什么参数、落到哪个 Worker、
// 花了多久、成功还是失败。幂等键也落在这里，重复投递直接回放结果。
type Execution struct {
	ID           string `json:"id"`
	TaskID       string `json:"task_id"`
	SkillName    string `json:"skill_name"`
	SkillVersion string `json:"skill_version"`
	Mode         string `json:"mode"`
	Status       string `json:"status"`
	TenantID     string `json:"tenant_id"`
	UserID       string `json:"user_id"`
	AgentKey     string `json:"agent_key"`
	SessionID    string `json:"session_id"`
	// IdempotencyKey 幂等键（同技能同键成功结果回放）
	IdempotencyKey string `json:"idempotency_key"`
	// Input / Output 均为 JSON 文本（截断存储，避免大结果撑爆表）
	Input     string `json:"input"`
	Output    string `json:"output"`
	Error     string `json:"error"`
	WorkerID  string `json:"worker_id"`
	Attempts  int    `json:"attempts"`
	LatencyMS int64  `json:"latency_ms"`
	TraceID   string `json:"trace_id"`
	// LeaseUntil 派发租约：派发协程崩溃后，过期可被其它进程回收重跑
	LeaseUntil *time.Time `json:"lease_until,omitempty"`
	QueuedAt   *time.Time `json:"queued_at,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Running 是否终态
func (e Execution) Done() bool {
	switch e.Status {
	case ExecSucceeded, ExecFailed, ExecTimeout, ExecCanceled:
		return true
	}
	return false
}

// ExecutionFilter 执行记录查询
type ExecutionFilter struct {
	SkillName string
	Status    string
	TaskID    string
	TenantID  string
	UserID    string
	Limit     int
}

// RuntimeRequest 统一执行入参（Agent / API / Worker 回流都走这一个结构）
type RuntimeRequest struct {
	SkillName string         `json:"skill_name"`
	TaskID    string         `json:"task_id"`
	Input     map[string]any `json:"input"`
	// 身份与上下文：用于多租户隔离与链路追踪，缺省从 context 身份兜底
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	AgentKey  string `json:"agent_key"`
	SessionID string `json:"session_id"`
	Purpose   string `json:"purpose"`
	// Timeout 覆盖技能配置的超时（0 = 用技能配置）
	Timeout time.Duration `json:"-"`
	// IdempotencyKey 调用方指定的幂等键（空则运行时按 task+技能+参数摘要生成）
	IdempotencyKey string `json:"idempotency_key"`
	// ForceAsync 强制异步（仅对 remote 技能有效；async 技能天然异步）
	ForceAsync bool `json:"force_async"`
}

// RuntimeResult 统一执行出参
type RuntimeResult struct {
	ExecutionID string         `json:"execution_id"`
	SkillName   string         `json:"skill_name"`
	Mode        string         `json:"mode"`
	Status      string         `json:"status"`
	Output      map[string]any `json:"output,omitempty"`
	// Text 供 LLM 消费的文本视图（结果 JSON 的压缩形式）
	Text      string `json:"text,omitempty"`
	WorkerID  string `json:"worker_id,omitempty"`
	Attempts  int    `json:"attempts"`
	LatencyMS int64  `json:"latency_ms"`
	// Replayed 命中幂等键，回放的是上次成功结果
	Replayed bool `json:"replayed,omitempty"`
	// Queued 异步模式：已入队，等 execution_id 查询
	Queued bool `json:"queued,omitempty"`
}

// WorkerRequest 发给 Worker 的标准信封（与语言无关，字段对齐 SkillRequest）
type WorkerRequest struct {
	TaskID         string         `json:"task_id"`
	ExecutionID    string         `json:"execution_id"`
	SkillName      string         `json:"skill_name"`
	SkillVersion   string         `json:"skill_version"`
	TenantID       string         `json:"tenant_id"`
	UserID         string         `json:"user_id"`
	AgentKey       string         `json:"agent_key"`
	SessionID      string         `json:"session_id"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	TimeoutMS      int            `json:"timeout_ms,omitempty"`
	TraceID        string         `json:"trace_id,omitempty"`
	Input          map[string]any `json:"input"`
}

// WorkerResponse Worker 的标准回包（SkillResponse）
type WorkerResponse struct {
	TaskID      string         `json:"task_id"`
	ExecutionID string         `json:"execution_id"`
	Success     bool           `json:"success"`
	Output      map[string]any `json:"output,omitempty"`
	Error       string         `json:"error,omitempty"`
	// WorkerID 可选：Worker 自报实例标识，便于核对落点
	WorkerID string `json:"worker_id,omitempty"`
	// LatencyMS 可选：Worker 侧自报耗时
	LatencyMS int64 `json:"latency_ms,omitempty"`
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
