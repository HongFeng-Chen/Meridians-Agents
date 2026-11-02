package prompt

// Studio 单测：fake LLM 桩验证打分/生成/优化的 JSON 解析、
// 容错（代码块包裹 / 前后说明文字）与入参校验。

import (
	"context"
	"go_agent/internal/biz/llm"
	"strings"
	"testing"
)

type fakeStudioLLM struct {
	reply string
	err   error

	lastSource string
	lastTemp   float64
}

func (f *fakeStudioLLM) ChatSource(ctx context.Context, source, system, user string, temperature float64) (string, error) {
	f.lastSource = source
	f.lastTemp = temperature
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func (f *fakeStudioLLM) ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error) {
	return f.ChatSource(ctx, o.Source, o.System, o.User, o.Temperature)
}

func (f *fakeStudioLLM) Model() string { return "fake-model" }

func TestStudioScore(t *testing.T) {
	llm := &fakeStudioLLM{reply: "评分如下：\n```json\n" + `{
		"total": 88,
		"dimensions": [
			{"name": "清晰度", "score": 90, "comment": "表述清楚"},
			{"name": "结构化", "score": 86, "comment": "分段合理"}
		],
		"issues": ["缺少输出格式约束"],
		"suggestions": ["补充 JSON 输出示例", "  "],
		"summary": "整体良好，需补输出约束"
	}` + "\n```"}
	s := NewStudio(llm)

	rep, err := s.Score(context.Background(), ScoreInput{Prompt: "你是调研助手。", Task: "行业调研"})
	if err != nil {
		t.Fatalf("Score 失败: %v", err)
	}
	if rep.Total != 88 || rep.Grade != "A" {
		t.Errorf("total/grade = %d/%s, 期望 88/A", rep.Total, rep.Grade)
	}
	if len(rep.Dimensions) != 2 || rep.Dimensions[0].Name != "清晰度" {
		t.Errorf("dimensions 解析错误: %+v", rep.Dimensions)
	}
	// 空建议应被过滤
	if len(rep.Suggestions) != 1 {
		t.Errorf("suggestions = %v, 期望过滤空串后剩 1 条", rep.Suggestions)
	}
	if rep.Model != "fake-model" {
		t.Errorf("model = %q", rep.Model)
	}
	if llm.lastTemp != 0.2 {
		t.Errorf("打分温度 = %v, 期望 0.2", llm.lastTemp)
	}

	// 数据源透传
	_, _ = s.Score(context.Background(), ScoreInput{Prompt: "x", LLMSource: "qwen"})
	if llm.lastSource != "qwen" {
		t.Errorf("source = %q, 期望 qwen", llm.lastSource)
	}

	// 等级边界
	if got := studioGrade(85); got != "A" {
		t.Errorf("85 应为 A，得 %s", got)
	}
	if got := studioGrade(54); got != "D" {
		t.Errorf("54 应为 D，得 %s", got)
	}
}

func TestStudioScoreValidation(t *testing.T) {
	s := NewStudio(&fakeStudioLLM{})
	if _, err := s.Score(context.Background(), ScoreInput{Prompt: "  "}); err != ErrEmptyContent {
		t.Errorf("空提示词应报 ErrEmptyContent，得 %v", err)
	}
	// LLM 输出不是 JSON
	s2 := NewStudio(&fakeStudioLLM{reply: "我觉得写得不错，八十分吧"})
	if _, err := s2.Score(context.Background(), ScoreInput{Prompt: "x"}); err != ErrStudioOutput {
		t.Errorf("非 JSON 输出应报 ErrStudioOutput，得 %v", err)
	}
}

func TestStudioGenerate(t *testing.T) {
	llm := &fakeStudioLLM{reply: `{"prompt": "你是数据分析助手。", "name": "数据分析助手", "highlights": ["含分步方法", "含输出约束"]}`}
	s := NewStudio(llm)

	d, err := s.Generate(context.Background(), GenerateInput{Task: "做销售数据分析", RoleName: "数据分析师"})
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if !strings.Contains(d.Prompt, "数据分析") || d.Name != "数据分析助手" || len(d.Highlights) != 2 {
		t.Errorf("draft 解析错误: %+v", d)
	}
	if llm.lastTemp != 0.7 {
		t.Errorf("生成温度 = %v, 期望 0.7", llm.lastTemp)
	}

	if _, err := s.Generate(context.Background(), GenerateInput{Task: ""}); err != ErrEmptyContent {
		t.Errorf("空 task 应报 ErrEmptyContent，得 %v", err)
	}
	// LLM 返回空 prompt 视为解析失败
	s2 := NewStudio(&fakeStudioLLM{reply: `{"prompt": "", "name": "x"}`})
	if _, err := s2.Generate(context.Background(), GenerateInput{Task: "y"}); err != ErrStudioOutput {
		t.Errorf("空 prompt 应报 ErrStudioOutput，得 %v", err)
	}
}

func TestStudioOptimize(t *testing.T) {
	llm := &fakeStudioLLM{reply: `优化完成。结果：{"prompt": "优化后的提示词", "changes": ["补充输出格式"], "score_before": 62, "score_after": 90}`}
	s := NewStudio(llm)

	r, err := s.Optimize(context.Background(), OptimizeInput{Prompt: "原始提示词", Goal: "输出更稳定"})
	if err != nil {
		t.Fatalf("Optimize 失败: %v", err)
	}
	if r.Prompt != "优化后的提示词" || r.ScoreBefore != 62 || r.ScoreAfter != 90 || len(r.Changes) != 1 {
		t.Errorf("result 解析错误: %+v", r)
	}
	if llm.lastTemp != 0.4 {
		t.Errorf("优化温度 = %v, 期望 0.4", llm.lastTemp)
	}

	// 分数越界钳制
	s2 := NewStudio(&fakeStudioLLM{reply: `{"prompt": "p", "changes": [], "score_before": 120, "score_after": -5}`})
	r2, err := s2.Optimize(context.Background(), OptimizeInput{Prompt: "x"})
	if err != nil {
		t.Fatalf("Optimize 失败: %v", err)
	}
	if r2.ScoreBefore != 100 || r2.ScoreAfter != 0 {
		t.Errorf("分数未钳制: %d/%d", r2.ScoreBefore, r2.ScoreAfter)
	}
}
