package prompt

// 提示词工程（Prompt Studio）：LLM 驱动的提示词打分 / 生成 / 优化。
//
// 与模板管理（prompt.go）解耦：Studio 不落库、不碰版本指针，
// 只做「一段提示词 ↔ LLM 结构化评估/改写」的无状态计算。
// LLM 端口绑定 llm.Manager（可指定数据源，失败自动回退 default 链路）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go_agent/internal/biz/llm"
)

// 域错误：mapPromptErr 统一映射为 400 / 502。
var (
	// ErrStudioOutput 大模型未按约定 JSON 输出
	ErrStudioOutput = errors.New("大模型返回内容无法解析，请重试或更换数据源")
)

// LLM 提示词工程依赖的大模型端口（wire 绑定 *llm.Manager）。
type LLM interface {
	// ChatSource 指定数据源对话；source 为空走平台 default 链路，
	// 指定源调用失败时由 llm.Manager 自动回退，任务不中断。
	ChatSource(ctx context.Context, source, system, user string, temperature float64) (string, error)
	// ChatOpts 统一参数通道（JSON 模式 / max_tokens），签名与 llm.Manager 一致
	ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error)
	Model() string
}

// Studio 提示词工程用例。
type Studio struct {
	chat LLM
}

// NewStudio 创建提示词工程用例。
func NewStudio(chat LLM) *Studio {
	return &Studio{chat: chat}
}

// —— 打分 ——

// ScoreDim 单个评分维度。
type ScoreDim struct {
	Name    string `json:"name"`
	Score   int    `json:"score"`
	Comment string `json:"comment"`
}

// ScoreInput 打分入参。
type ScoreInput struct {
	Prompt    string // 待评分提示词
	Task      string // 任务/场景描述（可选）
	LLMSource string // 数据源（可选）
}

// ScoreReport 评分报告。
type ScoreReport struct {
	Total       int        `json:"total"`
	Grade       string     `json:"grade"`
	Dimensions  []ScoreDim `json:"dimensions"`
	Issues      []string   `json:"issues"`
	Suggestions []string   `json:"suggestions"`
	Summary     string     `json:"summary"`
	Model       string     `json:"model"`
}

const studioScoreSystem = `你是资深提示词工程师，负责评审 Agent 的系统提示词。
评审维度固定为五个：清晰度、结构化、角色设定、约束完备、可执行性（各 0-100 分，整数）。
严格按以下 JSON 输出，禁止输出任何其他内容（禁止 markdown 代码块、禁止解释）：
{"total": 总分0-100, "dimensions": [{"name": "清晰度", "score": 0, "comment": "一句话点评"}, ...五个维度], "issues": ["问题1", ...按严重程度排序], "suggestions": ["建议1", ...], "summary": "一句话总评"}
评分要苛刻、具体、可操作；问题与建议必须针对提示词原文的具体内容。`

// Score 对提示词做 LLM 评分。
func (s *Studio) Score(ctx context.Context, in ScoreInput) (*ScoreReport, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, ErrEmptyContent
	}
	var user strings.Builder
	user.WriteString("待评审的提示词：\n<<<PROMPT\n" + in.Prompt + "\nPROMPT>>>\n")
	if t := strings.TrimSpace(in.Task); t != "" {
		user.WriteString("\n该提示词服务的任务/场景：\n" + t + "\n")
	}
	// 打分输出是 JSON：低温 + JSON 模式 + 输出上限，稳定且防啰嗦
	raw, err := s.chat.ChatOpts(ctx, llm.ChatOptions{
		System: studioScoreSystem, User: user.String(),
		Source: in.LLMSource, Temperature: 0.2, MaxTokens: 1200, JSONMode: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStudioOutput, err)
	}
	var rep struct {
		Total       int        `json:"total"`
		Dimensions  []ScoreDim `json:"dimensions"`
		Issues      []string   `json:"issues"`
		Suggestions []string   `json:"suggestions"`
		Summary     string     `json:"summary"`
	}
	if err := studioUnmarshal(raw, &rep); err != nil {
		return nil, err
	}
	out := &ScoreReport{
		Total:       clampScore(rep.Total),
		Grade:       studioGrade(rep.Total),
		Dimensions:  rep.Dimensions,
		Issues:      nonEmpty(rep.Issues),
		Suggestions: nonEmpty(rep.Suggestions),
		Summary:     strings.TrimSpace(rep.Summary),
		Model:       s.chat.Model(),
	}
	for i := range out.Dimensions {
		out.Dimensions[i].Score = clampScore(out.Dimensions[i].Score)
	}
	return out, nil
}

// —— 生成 ——

// GenerateInput 生成入参。
type GenerateInput struct {
	Task      string // 需求描述（必填）
	RoleName  string // 角色名（可选）
	Extra     string // 额外要求（可选）
	LLMSource string // 数据源（可选）
}

// Draft 生成结果。
type Draft struct {
	Prompt     string   `json:"prompt"`
	Name       string   `json:"name"`
	Highlights []string `json:"highlights"`
	Model      string   `json:"model"`
}

const studioGenerateSystem = `你是资深提示词工程师，根据需求描述为 Agent 生成高质量系统提示词。
要求：包含清晰的角色人设、工作方法（分步骤）、输出要求与边界（禁止事项）；使用第二人称「你」；中文；直接可用的成品。
严格按以下 JSON 输出，禁止输出任何其他内容（禁止 markdown 代码块、禁止解释）：
{"prompt": "完整提示词正文", "name": "建议的 Agent 名称（4-8字）", "highlights": ["设计要点1", ...]}`

// Generate 按需求生成提示词草稿。
func (s *Studio) Generate(ctx context.Context, in GenerateInput) (*Draft, error) {
	if strings.TrimSpace(in.Task) == "" {
		return nil, ErrEmptyContent
	}
	var user strings.Builder
	user.WriteString("需求描述：\n" + in.Task + "\n")
	if n := strings.TrimSpace(in.RoleName); n != "" {
		user.WriteString("\n角色名：" + n + "\n")
	}
	if e := strings.TrimSpace(in.Extra); e != "" {
		user.WriteString("\n额外要求：\n" + e + "\n")
	}
	// 生成是创意任务：温度放开到 0.7；输出为 JSON 草稿，开 JSON 模式并限制长度
	raw, err := s.chat.ChatOpts(ctx, llm.ChatOptions{
		System: studioGenerateSystem, User: user.String(),
		Source: in.LLMSource, Temperature: 0.7, MaxTokens: 3000, JSONMode: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStudioOutput, err)
	}
	var d struct {
		Prompt     string   `json:"prompt"`
		Name       string   `json:"name"`
		Highlights []string `json:"highlights"`
	}
	if err := studioUnmarshal(raw, &d); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.Prompt) == "" {
		return nil, ErrStudioOutput
	}
	return &Draft{Prompt: d.Prompt, Name: strings.TrimSpace(d.Name), Highlights: nonEmpty(d.Highlights), Model: s.chat.Model()}, nil
}

// —— 优化 ——

// OptimizeInput 优化入参。
type OptimizeInput struct {
	Prompt    string // 原提示词（必填）
	Task      string // 任务/场景描述（可选）
	Goal      string // 优化目标（可选）
	LLMSource string // 数据源（可选）
}

// OptimizeResult 优化结果。
type OptimizeResult struct {
	Prompt      string   `json:"prompt"`
	Changes     []string `json:"changes"`
	ScoreBefore int      `json:"score_before"`
	ScoreAfter  int      `json:"score_after"`
	Model       string   `json:"model"`
}

const studioOptimizeSystem = `你是资深提示词工程师，负责优化 Agent 的系统提示词。
保持原意与角色不变，针对问题重写：补齐缺失约束、消除歧义、强化输出格式、精简冗余。
严格按以下 JSON 输出，禁止输出任何其他内容（禁止 markdown 代码块、禁止解释）：
{"prompt": "优化后的完整提示词", "changes": ["改动1：为什么改", ...], "score_before": 0-100 原稿评分, "score_after": 0-100 改后评分}`

// Optimize 优化已有提示词。
func (s *Studio) Optimize(ctx context.Context, in OptimizeInput) (*OptimizeResult, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, ErrEmptyContent
	}
	var user strings.Builder
	user.WriteString("原提示词：\n<<<PROMPT\n" + in.Prompt + "\nPROMPT>>>\n")
	if t := strings.TrimSpace(in.Task); t != "" {
		user.WriteString("\n该提示词服务的任务/场景：\n" + t + "\n")
	}
	if g := strings.TrimSpace(in.Goal); g != "" {
		user.WriteString("\n优化目标（优先满足）：" + g + "\n")
	}
	// 优化输出是 JSON（改后文 + 改动说明 + 前后分）：中温 + JSON 模式
	raw, err := s.chat.ChatOpts(ctx, llm.ChatOptions{
		System: studioOptimizeSystem, User: user.String(),
		Source: in.LLMSource, Temperature: 0.4, MaxTokens: 3000, JSONMode: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStudioOutput, err)
	}
	var r struct {
		Prompt      string   `json:"prompt"`
		Changes     []string `json:"changes"`
		ScoreBefore int      `json:"score_before"`
		ScoreAfter  int      `json:"score_after"`
	}
	if err := studioUnmarshal(raw, &r); err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return nil, ErrStudioOutput
	}
	return &OptimizeResult{
		Prompt:      r.Prompt,
		Changes:     nonEmpty(r.Changes),
		ScoreBefore: clampScore(r.ScoreBefore),
		ScoreAfter:  clampScore(r.ScoreAfter),
		Model:       s.chat.Model(),
	}, nil
}

// —— 工具 ——

// studioUnmarshal 从 LLM 回复中提取 JSON 对象并解码。
// 容忍：```json 代码块包裹、JSON 前后的说明文字。
func studioUnmarshal(raw string, v any) error {
	start := strings.Index(raw, "{")
	if start < 0 {
		return ErrStudioOutput
	}
	end := strings.LastIndex(raw, "}")
	if end <= start {
		return ErrStudioOutput
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), v); err != nil {
		return ErrStudioOutput
	}
	return nil
}

// studioGrade 由总分推导等级。
func studioGrade(total int) string {
	switch {
	case total >= 85:
		return "A"
	case total >= 70:
		return "B"
	case total >= 55:
		return "C"
	default:
		return "D"
	}
}

func clampScore(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// nonEmpty 过滤空字符串并去除首尾空白。
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}
