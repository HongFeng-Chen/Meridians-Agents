package harness

import (
	"errors"
	"time"
)

// 运行状态
const (
	RunPending   = "pending"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCanceled  = "canceled"
)

// 断言规则类型
const (
	CheckContains    = "contains"     // 输出包含指定文本（忽略大小写）
	CheckNotContains = "not_contains" // 输出不包含指定文本
	CheckRegex       = "regex"        // 输出匹配正则
	CheckJSONValid   = "json_valid"   // 输出可解析为 JSON（结构化输出校验）
	CheckMinLength   = "min_length"   // 输出字符数下限（Pattern 为数字）
	CheckMaxLength   = "max_length"   // 输出字符数上限
	CheckLLMJudge    = "llm_judge"    // LLM-as-Judge 语义判分（Pattern 为评分要点）
)

// 被测对象前缀
const (
	TargetAgentPrefix = "agent:" // agent:<agent_key>，经 agentfactory 执行
	TargetModelPrefix = "model:" // model:<模型名>，经 llm.Manager 直连
)

// 领域错误
var (
	ErrSetNotFound   = errors.New("用例集不存在")
	ErrCaseNotFound  = errors.New("评测用例不存在")
	ErrRunNotFound   = errors.New("评测运行不存在")
	ErrEmptySet      = errors.New("用例集名称不能为空")
	ErrEmptyCase     = errors.New("用例输入不能为空")
	ErrInvalidCheck  = errors.New("断言规则不合法")
	ErrInvalidTarget = errors.New("被测对象不合法（agent:<key> 或 model:<name>）")
	ErrRunActive     = errors.New("评测运行仍在进行中，不能删除或重复启动")
	ErrNoCases       = errors.New("用例集为空，请先添加用例")
	ErrNoRunner      = errors.New("被测对象执行器未接线")
)

// Check 一条断言规则
type Check struct {
	Kind    string  `json:"kind"`    // 七类之一
	Pattern string  `json:"pattern"` // 文本 / 正则 / 长度数字 / 评分要点
	Weight  float64 `json:"weight"`  // 权重，0 按 1 计
}

// CheckResult 单条断言的判定结果
type CheckResult struct {
	Kind    string  `json:"kind"`
	Pattern string  `json:"pattern"`
	Passed  bool    `json:"passed"`
	Weight  float64 `json:"weight"`
	Detail  string  `json:"detail"` // 失败原因 / LLM Judge 评语
}

// Case 评测用例：一份「输入 + 期望 + 判分规则」的三元组
type Case struct {
	ID        uint64    `json:"id"`
	SetID     uint64    `json:"set_id"`
	Name      string    `json:"name"`    // 用例名（列表展示）
	Input     string    `json:"input"`   // 喂给被测对象的输入
	Context   string    `json:"context"` // 可选前置上下文（拼接在输入前）
	Expect    string    `json:"expect"`  // 参考答案（llm_judge 与人工比对用）
	Checks    []Check   `json:"checks"`  // 断言规则（空 = 只采集不判分）
	Tags      []string  `json:"tags"`    // 标签（按维度聚合报告）
	Enabled   bool      `json:"enabled"` // 停用用例不参与运行
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Set 用例集：一个被测对象的一组用例（评测的调度单位）
type Set struct {
	ID          uint64    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Target      string    `json:"target"` // 默认被测对象，启动运行时可覆盖
	Tags        []string  `json:"tags"`
	Enabled     bool      `json:"enabled"`
	CaseCount   int       `json:"case_count"` // 派生字段：启用用例数
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RunConfig 运行配置
type RunConfig struct {
	Target            string `json:"target"`               // 覆盖 Set.Target；空用用例集默认值
	JudgeModel        string `json:"judge_model"`          // LLM Judge 使用的模型；空走默认源
	Concurrency       int    `json:"concurrency"`          // 并发用例数（默认 2，防止打爆数据源）
	PerCaseTimeoutSec int    `json:"per_case_timeout_sec"` // 单例超时（默认 120s）
	// 提示词版本评测标注（可选）：运行完成后把得分回写 prompt_version_scores，
	// 闭环「提示词优化 → 评测验证」
	PromptKey     string `json:"prompt_key,omitempty"`
	PromptVersion int    `json:"prompt_version,omitempty"`
	StopOnCancel  bool   `json:"-"` // 内部标记
}

// Run 一次评测运行（进度实时落库，供前端轮询）
type Run struct {
	ID         uint64    `json:"id"`
	SetID      uint64    `json:"set_id"`
	SetName    string    `json:"set_name"`
	Target     string    `json:"target"`
	Status     string    `json:"status"`
	Total      int       `json:"total"` // 用例总数
	Done       int       `json:"done"`  // 已完成数
	Passed     int       `json:"passed"`
	Failed     int       `json:"failed"`
	Errored    int       `json:"errored"` // 执行报错的用例数（不计入判分分母）
	Score      float64   `json:"score"`   // 加权平均分 0-100
	CostMS     int64     `json:"cost_ms"` // 总耗时
	Config     RunConfig `json:"config"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// RunItem 单用例运行结果
type RunItem struct {
	ID               uint64        `json:"id"`
	RunID            uint64        `json:"run_id"`
	CaseID           uint64        `json:"case_id"`
	CaseName         string        `json:"case_name"`
	Output           string        `json:"output"` // 被测对象原始输出
	Checks           []CheckResult `json:"checks"` // 逐条断言结果
	Passed           bool          `json:"passed"` // 全部（权重>0 的）断言通过
	Score            float64       `json:"score"`  // 0-100
	LatencyMS        int64         `json:"latency_ms"`
	PromptTokens     int64         `json:"prompt_tokens"` // 来自 llm 调用记录（执行 + judge）
	CompletionTokens int64         `json:"completion_tokens"`
	Error            string        `json:"error,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
}

// SetFilter / RunFilter 列表过滤
type (
	SetFilter struct {
		Keyword string
		Limit   int
	}
	RunFilter struct {
		SetID  uint64
		Status string
		Limit  int
	}
)

// Validate 校验断言规则
func (c Check) Validate() error {
	switch c.Kind {
	case CheckContains, CheckNotContains, CheckRegex, CheckLLMJudge:
		if c.Pattern == "" {
			return ErrInvalidCheck
		}
	case CheckJSONValid:
	case CheckMinLength, CheckMaxLength:
		if c.Pattern == "" {
			return ErrInvalidCheck
		}
	default:
		return ErrInvalidCheck
	}
	return nil
}

// Score 计算一组断言结果的加权得分（0-100）。无断言视为不判分，返回 -1。
func Score(checks []CheckResult) float64 {
	total, passed := 0.0, 0.0
	for _, c := range checks {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		total += w
		if c.Passed {
			passed += w
		}
	}
	if total == 0 {
		return -1
	}
	return passed / total * 100
}
