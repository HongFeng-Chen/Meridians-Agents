// Package llm 大模型接入域：数据源（Provider）配置存 MySQL，支持多数据源
// 并存、按场景路由、失败自动降级，以及对数据源的在线增删改查与连通性测试。
//
// 设计要点：
//   - 配置以 MySQL 为准（llm_providers），conf 里的 llm 块只做「兜底默认」；
//   - 业务代码（RAG / GraphRAG / Memory）只依赖 Chatter 接口，不感知数据源；
//   - 解析结果在内存中做短 TTL 缓存，变更接口立即失效本实例，多实例靠 TTL 收敛。
package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 数据源类型：仅用于展示与前端预设，实际调用统一走 OpenAI 兼容协议。
const (
	KindOpenAI      = "openai"
	KindDeepSeek    = "deepseek"
	KindOllama      = "ollama"
	KindQwen        = "qwen"
	KindSiliconFlow = "siliconflow"
	KindMoonshot    = "moonshot"
	KindCustom      = "custom"
)

// 场景：一个数据源可声明自己服务的场景，解析时优先按场景命中。
const (
	SceneGeneral = "general"
	SceneRAG     = "rag"
	SceneGraph   = "graph"
	SceneExtract = "extract"
	SceneMemory  = "memory"
)

// 领域错误
var (
	ErrProviderNotFound = errors.New("LLM 数据源不存在")
	ErrProviderExists   = errors.New("LLM 数据源名称已存在")
	ErrInvalidProvider  = errors.New("LLM 数据源配置不合法")
	ErrNoProvider       = errors.New("没有可用的 LLM 数据源（请在 MySQL 配置或将 llm 块写入 config.yaml）")
	ErrLastProvider     = errors.New("不能删除唯一启用的数据源，请先新增其它数据源")
)

// Provider LLM 数据源（DO）。APIKey 为明文，仅内部使用；
// 对外输出必须经 Masked() 脱敏。
type Provider struct {
	ID           uint64   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"base_url"`
	APIKey       string   `json:"-"`
	HasKey       bool     `json:"has_key"`
	Models       []string `json:"models"`
	DefaultModel string   `json:"default_model"`
	// ImageModels / VideoModels 该数据源支持的图像/视频生成模型名（媒体生成用，
	// 与对话模型 models 互相独立；空表示该数据源不参与媒体生成路由）。
	ImageModels []string  `json:"image_models"`
	VideoModels []string  `json:"video_models"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	TimeoutSec  int       `json:"timeout_seconds"`
	Enabled     bool      `json:"enabled"`
	IsDefault   bool      `json:"is_default"`
	Priority    int       `json:"priority"`
	Scenes      []string  `json:"scenes"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Masked 返回脱敏副本（供管理接口输出）
func (p Provider) Masked() Provider {
	p.HasKey = p.APIKey != ""
	p.APIKey = MaskKey(p.APIKey)
	return p
}

// MaskKey 密钥脱敏：只保留末 4 位
func MaskKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 6 {
		return "****"
	}
	return "****" + k[len(k)-4:]
}

// Model 返回实际使用的模型名（默认模型优先，其次首个模型）
func (p Provider) Model() string {
	if strings.TrimSpace(p.DefaultModel) != "" {
		return p.DefaultModel
	}
	if len(p.Models) > 0 {
		return p.Models[0]
	}
	return ""
}

// ServesScene 判断数据源是否声明支持该场景（未声明任何场景视为通用）
func (p Provider) ServesScene(scene string) bool {
	if scene == "" || scene == SceneGeneral || len(p.Scenes) == 0 {
		return true
	}
	for _, s := range p.Scenes {
		if strings.EqualFold(s, scene) {
			return true
		}
	}
	return false
}

// CreateProviderInput 新建数据源
type CreateProviderInput struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"base_url"`
	APIKey       string   `json:"api_key"`
	Models       []string `json:"models"`
	DefaultModel string   `json:"default_model"`
	ImageModels  []string `json:"image_models"`
	VideoModels  []string `json:"video_models"`
	Temperature  float64  `json:"temperature"`
	MaxTokens    int      `json:"max_tokens"`
	TimeoutSec   int      `json:"timeout_seconds"`
	Enabled      *bool    `json:"enabled"`
	IsDefault    bool     `json:"is_default"`
	Priority     int      `json:"priority"`
	Scenes       []string `json:"scenes"`
	Description  string   `json:"description"`
}

// UpdateProviderInput 修改数据源（指针字段为空表示不改）
type UpdateProviderInput struct {
	Kind         *string   `json:"kind"`
	BaseURL      *string   `json:"base_url"`
	APIKey       *string   `json:"api_key"`
	Models       *[]string `json:"models"`
	DefaultModel *string   `json:"default_model"`
	ImageModels  *[]string `json:"image_models"`
	VideoModels  *[]string `json:"video_models"`
	Temperature  *float64  `json:"temperature"`
	MaxTokens    *int      `json:"max_tokens"`
	TimeoutSec   *int      `json:"timeout_seconds"`
	Enabled      *bool     `json:"enabled"`
	IsDefault    *bool     `json:"is_default"`
	Priority     *int      `json:"priority"`
	Scenes       *[]string `json:"scenes"`
	Description  *string   `json:"description"`
}

// TestResult 连通性测试结果
type TestResult struct {
	OK        bool   `json:"ok"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	LatencyMS int64  `json:"latency_ms"`
	Reply     string `json:"reply"`
	Error     string `json:"error,omitempty"`
}

// Stats 数据源规模
type Stats struct {
	Providers int64  `json:"providers"`
	Enabled   int64  `json:"enabled"`
	Default   string `json:"default"`
}

// ChatInput 手动对话（管理端调试用）
type ChatInput struct {
	Provider    string  `json:"provider"`
	Scene       string  `json:"scene"`
	System      string  `json:"system"`
	User        string  `json:"user"`
	Temperature float64 `json:"temperature"`
}

// Usage 一次调用的 token 用量（OpenAI 兼容协议响应里的 usage 字段）
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// —— LLM 调用历史 ——

type ctxMetaKey struct{}

// CallMeta 调用元数据：业务方通过 WithMeta 塞进 context，
// Manager 记录调用时取出，把每次 LLM 调用关联到业务实体（如任务）。
type CallMeta struct {
	TaskID   uint64 // 关联任务（agentfactory）
	AgentKey string // 关联 Agent
	Purpose  string // 调用用途，如 agentfactory.react / agentfactory.factory / llm.test
	// RefKind / RefID 通用归属：没有任务行的调用（评测 model: 目标、单轮直连）
	// 靠它把成本归到「哪一次业务动作」上，取值与 usage_records.ref_* 同构。
	RefKind string
	RefID   string
}

// WithMeta 把调用元数据放进 context。
//
// 按字段合并而不是整体替换：上游挂在 context 上的归属（例如评测运行把
// harness_run:<id> 挂上去）不能被下游的任务执行覆盖掉——一旦覆盖，
// 一次评测的总成本就少了被测 Agent 那部分。零值字段表示「不改这一项」。
func WithMeta(ctx context.Context, meta CallMeta) context.Context {
	cur := MetaFrom(ctx)
	if meta.TaskID != 0 {
		cur.TaskID = meta.TaskID
	}
	if meta.AgentKey != "" {
		cur.AgentKey = meta.AgentKey
	}
	if meta.Purpose != "" {
		cur.Purpose = meta.Purpose
	}
	if meta.RefKind != "" {
		cur.RefKind = meta.RefKind
	}
	if meta.RefID != "" {
		cur.RefID = meta.RefID
	}
	return context.WithValue(ctx, ctxMetaKey{}, cur)
}

// MetaFrom 取调用元数据（未设置返回零值）
func MetaFrom(ctx context.Context) CallMeta {
	if meta, ok := ctx.Value(ctxMetaKey{}).(CallMeta); ok {
		return meta
	}
	return CallMeta{}
}

// CallRecord 一次 LLM 调用记录（一份数据源尝试一条）
type CallRecord struct {
	ID          uint64    `json:"id"`
	TenantID    string    `json:"tenant_id"` // 归属（写入时从 ctx 身份取）
	UserID      string    `json:"user_id"`
	Provider    string    `json:"provider"`     // 数据源名（fallback 表示配置兜底）
	Model       string    `json:"model"`        // 实际使用的模型
	Scene       string    `json:"scene"`        // 场景路由（general/rag/memory/...）
	Purpose     string    `json:"purpose"`      // 业务用途（context 元数据）
	TaskID      uint64    `json:"task_id"`      // 关联任务（0 表示与任务无关）
	AgentKey    string    `json:"agent_key"`    // 关联 Agent
	Temperature float64   `json:"temperature"`  // 本次温度
	DurationMS  int64     `json:"duration_ms"`  // 调用耗时
	OK          bool      `json:"ok"`           // 是否成功
	ErrorMsg    string    `json:"error_msg"`    // 失败原因（截断 300）
	PromptChars int       `json:"prompt_chars"` // 输入字符数
	ReplyChars  int       `json:"reply_chars"`  // 输出字符数
	PromptText  string    `json:"prompt_text"`  // 输入内容（system+user 拼接，超上限才截断存储）
	ReplyText   string    `json:"reply_text"`   // 输出内容（超上限才截断存储）
	Usage                 // token 用量
	CreatedAt   time.Time `json:"created_at"`
}

const (
	// maxPromptText / maxReplyText 调用详情落库截断上限（字符）。
	// 列类型为 mediumtext（16MB），20 万字符（中文约 600KB/行）足够存全量，
	// 仅作为防御性兜底：正常场景（含长系统提示词 + 召回上下文）都不会触达。
	maxPromptText = 200000
	maxReplyText  = 200000
)

// truncateText 截断字符串到 n 个字符（按 rune，避免中文按字节切出半字符），
// 超出部分追加省略标记
func truncateText(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + fmt.Sprintf("\n…（已截断，原文共 %d 字符）", len(r))
}

// PromptFull 调用输入全文（system+user 拼接，未截断前）
func PromptFull(system, user string) string {
	if strings.TrimSpace(system) == "" {
		return user
	}
	if strings.TrimSpace(user) == "" {
		return system
	}
	return "[system]\n" + system + "\n\n[user]\n" + user
}

// CallFilter 调用记录过滤
type CallFilter struct {
	// UserID 非空时只返回该用户的调用记录（服务层按登录身份设置；管理员传空 = 全平台）
	UserID   string
	TenantID string
	TaskID   uint64
	Model    string
	Models   []string // 多模型过滤（与 Model 取并集）
	Provider string
	Purpose  string
	OK       *bool
	Since    time.Time // 时间下界（含），零值 = 不限
	Until    time.Time // 时间上界（含），零值 = 不限
	Limit    int
	Offset   int
}

// ModelUsage 按模型聚合的用量
type ModelUsage struct {
	Model            string `json:"model"`
	Calls            int64  `json:"calls"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// CallStats 调用记录统计
type CallStats struct {
	Calls            int64        `json:"calls"`
	PromptTokens     int64        `json:"prompt_tokens"`
	CompletionTokens int64        `json:"completion_tokens"`
	TotalTokens      int64        `json:"total_tokens"`
	Failures         int64        `json:"failures"`
	ByModel          []ModelUsage `json:"by_model"`
}

// ChatOptions 一次 LLM 调用的完整参数（Manager.ChatOpts 通道）。
// Temperature < 0 表示用数据源默认温度；Source 与 Scene 二选一，Source 优先。
type ChatOptions struct {
	System string
	User   string
	Source string // 指定数据源名（未命中或失败回退默认链路）
	Scene  string // 场景路由（rag/graph/extract/memory/general）

	Temperature float64
	MaxTokens   int  // 补全长度上限（0 = 数据源默认）
	JSONMode    bool // response_format json_object，不支持自动降级
}

// UsageRow token 用量下钻的一行聚合（按 total_tokens 降序）
type UsageRow struct {
	Key              string `json:"key"`
	Calls            int64  `json:"calls"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	Failures         int64  `json:"failures"`
}

// 下钻维度白名单
var BreakdownDims = map[string]bool{
	"agent_key": true, "scene": true, "purpose": true, "model": true,
}
