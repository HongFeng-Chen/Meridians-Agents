package harness

// judge.go 判分体系：规则断言（确定性、零成本）+ LLM-as-Judge（语义开放题）。
//
// 判分原则：
//   - 每条断言独立判定、独立计权重，用例得分 = Σ通过权重 / Σ总权重 × 100；
//   - 规则断言失败要给出具体原因（缺失文本 / 不匹配位置 / 长度差），便于定位；
//   - LLM Judge 输出强制 JSON 协议（与执行环的输出协议治理同一思路），
//     解析失败视为断言失败而不是用例失败 —— 评测器自己不能成为被测对象的故障源。

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"go_agent/internal/biz/llm"
	"strings"
)

// judge 逐条判定用例的全部断言
func (u *UseCase) judge(ctx context.Context, cfg RunConfig, c Case, output string) []CheckResult {
	results := make([]CheckResult, 0, len(c.Checks))
	for _, chk := range c.Checks {
		results = append(results, u.judgeOne(ctx, cfg, c, chk, output))
	}
	return results
}

func (u *UseCase) judgeOne(ctx context.Context, cfg RunConfig, c Case, chk Check, output string) CheckResult {
	w := chk.Weight
	if w <= 0 {
		w = 1
	}
	res := CheckResult{Kind: chk.Kind, Pattern: chk.Pattern, Weight: w}

	switch chk.Kind {
	case CheckContains:
		res.Passed = strings.Contains(strings.ToLower(output), strings.ToLower(chk.Pattern))
		if !res.Passed {
			res.Detail = fmt.Sprintf("输出未包含 %q", clipRunes(chk.Pattern, 80))
		}
	case CheckNotContains:
		res.Passed = !strings.Contains(strings.ToLower(output), strings.ToLower(chk.Pattern))
		if !res.Passed {
			res.Detail = fmt.Sprintf("输出出现了禁止内容 %q", clipRunes(chk.Pattern, 80))
		}
	case CheckRegex:
		re, err := regexp.Compile(chk.Pattern)
		if err != nil {
			res.Detail = "正则不合法: " + err.Error()
			return res
		}
		res.Passed = re.MatchString(output)
		if !res.Passed {
			res.Detail = fmt.Sprintf("输出未匹配正则 %q", clipRunes(chk.Pattern, 80))
		}
	case CheckJSONValid:
		var v any
		if obj, ok := extractJSONObject(output); ok {
			res.Passed = json.Unmarshal([]byte(obj), &v) == nil
		}
		if !res.Passed {
			res.Detail = "输出不是完整可解析的 JSON"
		}
	case CheckMinLength:
		n, ok := parseLen(chk.Pattern)
		if !ok {
			res.Detail = "长度阈值不合法: " + chk.Pattern
			return res
		}
		res.Passed = len([]rune(output)) >= n
		if !res.Passed {
			res.Detail = fmt.Sprintf("输出 %d 字符，未达到下限 %d", len([]rune(output)), n)
		}
	case CheckMaxLength:
		n, ok := parseLen(chk.Pattern)
		if !ok {
			res.Detail = "长度阈值不合法: " + chk.Pattern
			return res
		}
		res.Passed = len([]rune(output)) <= n
		if !res.Passed {
			res.Detail = fmt.Sprintf("输出 %d 字符，超出上限 %d", len([]rune(output)), n)
		}
	case CheckLLMJudge:
		if u.chat == nil {
			res.Detail = "LLM Judge 未接线，无法语义判分"
			return res
		}
		passed, reason := u.llmJudge(ctx, cfg, c, chk, output)
		res.Passed = passed
		res.Detail = reason
	default:
		res.Detail = "未知断言类型 " + chk.Kind
	}
	return res
}

// llmJudge LLM-as-Judge：参考 expect 与评分要点 pattern 给输出判分。
// 输出协议：{"pass": true/false, "score": 0-100, "reason": "评语"}
func (u *UseCase) llmJudge(ctx context.Context, cfg RunConfig, c Case, chk Check, output string) (bool, string) {
	system := `你是严格的评测裁判（LLM-as-Judge）。根据「评分要点」判断被测输出是否合格。
评分要点可以包含多个子要求，全部满足才算 pass。允许表达方式不同，但事实与要点必须覆盖。
只输出一个 JSON 对象，不要输出其他文字：
{"pass": true, "score": 90, "reason": "一句话判定依据"}
规则：
1. pass 为最终判定，score 为 0-100 质量分
2. reason 用中文，不超过 100 字
3. 输出为空、跑题、编造事实一律 pass=false`
	user := fmt.Sprintf("## 题目输入\n%s\n\n## 参考答案\n%s\n\n## 评分要点\n%s\n\n## 被测输出\n%s",
		clipRunes(c.Input, 2000), orNone(c.Expect), chk.Pattern, clipRunes(output, 6000))

	judgeCtx := ctx
	if cfg.JudgeModel != "" {
		// 指定 Judge 模型：与执行环 chatWith 同一约定（提示词头部声明期望模型）
		user = "[期望模型: " + cfg.JudgeModel + "]\n" + user
	}
	reply, err := u.chat.ChatOpts(judgeCtx, llm.ChatOptions{
		System:      system,
		User:        user,
		Temperature: 0,    // 评审要稳定
		MaxTokens:   800,  // 评语很短，上限防啰嗦
		JSONMode:    true, // 判定输出是 JSON，不支持自动降级
	})
	if err != nil {
		return false, "Judge 调用失败: " + err.Error()
	}
	obj, ok := extractJSONObject(reply)
	if !ok {
		return false, "Judge 输出无 JSON: " + clipRunes(reply, 120)
	}
	var verdict struct {
		Pass   bool   `json:"pass"`
		Score  int    `json:"score"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(obj), &verdict); err != nil {
		return false, "Judge JSON 解析失败: " + clipRunes(reply, 120)
	}
	reason := verdict.Reason
	if reason == "" {
		reason = fmt.Sprintf("Judge 判定 pass=%v score=%d", verdict.Pass, verdict.Score)
	}
	return verdict.Pass, reason
}

// —— 小工具 ——

// extractJSONObject 截取第一个括号平衡的 JSON 对象（与 agentfactory 同算法，
// 独立实现避免跨包依赖内部函数）
func extractJSONObject(text string) (string, bool) {
	start := strings.Index(text, "{")
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if esc {
			esc = false
			continue
		}
		switch {
		case inStr && ch == '\\':
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return text[start : i+1], true
			}
		}
	}
	return "", false
}

func parseLen(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
	}
	return n, true
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（无）"
	}
	return s
}
