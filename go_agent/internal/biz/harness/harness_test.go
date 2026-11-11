package harness

import (
	"context"
	"go_agent/internal/biz/llm"
	"strings"
	"testing"
)

// fakeChatter 可编程 LLM 桩
type fakeChatter struct{ reply string }

func (f *fakeChatter) Chat(ctx context.Context, system, user string) (string, error) {
	return f.reply, nil
}
func (f *fakeChatter) ChatTemp(ctx context.Context, system, user string, temperature float64) (string, error) {
	return f.reply, nil
}
func (f *fakeChatter) ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error) {
	return f.ChatTemp(ctx, o.System, o.User, o.Temperature)
}

func (f *fakeChatter) Model() string { return "fake-model" }

// fakeRunner 可编程被测对象桩
type fakeRunner struct {
	output string
	info   TargetInfo
}

func (f *fakeRunner) Run(ctx context.Context, target, input string) (string, error) {
	return f.output, nil
}

func (f *fakeRunner) Resolve(ctx context.Context, target string) TargetInfo {
	return f.info
}

func TestScore(t *testing.T) {
	all := []CheckResult{
		{Passed: true, Weight: 1},
		{Passed: true, Weight: 3},
		{Passed: false, Weight: 1},
	}
	if got := Score(all); got != 80 {
		t.Fatalf("Score = %v, want 80", got)
	}
	if got := Score(nil); got != -1 {
		t.Fatalf("Score(nil) = %v, want -1", got)
	}
}

func TestRuleChecks(t *testing.T) {
	u := New(nil, &fakeRunner{}, nil, nil)
	c := Case{Input: "题目"}
	out := `结论如下 {"ok": true}，DONE。`
	cases := []struct {
		chk  Check
		want bool
		note string
	}{
		{Check{Kind: CheckContains, Pattern: "done"}, true, "包含（忽略大小写）"},
		{Check{Kind: CheckNotContains, Pattern: "forbidden"}, true, "未出现禁止词"},
		{Check{Kind: CheckNotContains, Pattern: "结论"}, false, "出现禁止词应失败"},
		{Check{Kind: CheckRegex, Pattern: `\{"ok":\s*true\}`}, true, "正则命中"},
		{Check{Kind: CheckJSONValid}, true, "内嵌 JSON 可解析"},
		{Check{Kind: CheckMinLength, Pattern: "5"}, true, "长度下限"},
		{Check{Kind: CheckMinLength, Pattern: "999"}, false, "长度下限不满足"},
		{Check{Kind: CheckMaxLength, Pattern: "10000"}, true, "长度上限"},
	}
	for _, tc := range cases {
		res := u.judgeOne(context.Background(), RunConfig{}, c, tc.chk, out)
		if res.Passed != tc.want {
			t.Errorf("%s: passed = %v (detail=%s), want %v", tc.note, res.Passed, res.Detail, tc.want)
		}
	}
	// 非法正则：断言失败且 Detail 说明，不 panic
	res := u.judgeOne(context.Background(), RunConfig{}, c, Check{Kind: CheckRegex, Pattern: "("}, out)
	if res.Passed || !strings.Contains(res.Detail, "正则不合法") {
		t.Errorf("非法正则应失败且提示，got %+v", res)
	}
}

func TestLLMJudge(t *testing.T) {
	u := New(nil, &fakeRunner{}, &fakeChatter{reply: `裁判认为：{"pass": true, "score": 88, "reason": "覆盖全部要点"}`}, nil)
	c := Case{Input: "什么是 harness", Expect: "执行框架"}
	res := u.judgeOne(context.Background(), RunConfig{}, c, Check{Kind: CheckLLMJudge, Pattern: "解释定义并举例"}, "harness 是…")
	if !res.Passed || !strings.Contains(res.Detail, "覆盖全部要点") {
		t.Fatalf("llmJudge = %+v, want pass 且带评语", res)
	}

	// Judge 输出不合规 JSON → 断言失败，而非报错
	u2 := New(nil, &fakeRunner{}, &fakeChatter{reply: "抱歉，我无法评判"}, nil)
	res2 := u2.judgeOne(context.Background(), RunConfig{}, c, Check{Kind: CheckLLMJudge, Pattern: "x"}, "y")
	if res2.Passed || !strings.Contains(res2.Detail, "Judge") {
		t.Fatalf("不合规 Judge 输出应失败: %+v", res2)
	}

	// Judge 未接线 → 失败并说明
	u3 := New(nil, &fakeRunner{}, nil, nil)
	res3 := u3.judgeOne(context.Background(), RunConfig{}, c, Check{Kind: CheckLLMJudge, Pattern: "x"}, "y")
	if res3.Passed || !strings.Contains(res3.Detail, "未接线") {
		t.Fatalf("Judge 未接线应失败: %+v", res3)
	}
}

func TestRunCaseNoChecks(t *testing.T) {
	u := New(nil, &fakeRunner{output: "答案"}, nil, nil)
	item := u.runCase(context.Background(), Run{ID: 1, Target: "agent:demo"}, Case{ID: 2, Input: "q"}, 0)
	if item.Error != "" || item.Output != "答案" {
		t.Fatalf("item = %+v", item)
	}
	// 无断言：只采集，Score = -1，输出非空视为通过
	if item.Score != -1 || !item.Passed {
		t.Fatalf("无断言用例应 score=-1 且通过, got %+v", item)
	}
}

func TestValidTarget(t *testing.T) {
	for _, ok := range []string{"agent:research", "model:qwen2.5:72b"} {
		if !validTarget(ok) {
			t.Errorf("%q 应合法", ok)
		}
	}
	for _, bad := range []string{"", "agent:", "model:", "http://x", "agent"} {
		if validTarget(bad) {
			t.Errorf("%q 应不合法", bad)
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	if obj, ok := extractJSONObject(`前置说明 {"a":{"b":1}} 尾部`); !ok || obj != `{"a":{"b":1}}` {
		t.Fatalf("extractJSONObject = %q %v", obj, ok)
	}
	if _, ok := extractJSONObject("没有对象"); ok {
		t.Fatal("无 JSON 应返回 false")
	}
	if _, ok := extractJSONObject(`{"a":"带}花括号"}后缀`); !ok {
		t.Fatal("字符串内的花括号不应破坏解析")
	}
}
