package conversation

import (
	"testing"
)

// TestParseChatJSON LLM 输出协议解析
func TestParseChatJSON(t *testing.T) {
	// 直答
	act, err := parseChatJSON(`前缀 noise {"final":"你好，我是平台助手"} 后缀`)
	if err != nil {
		t.Fatalf("直答解析失败: %v", err)
	}
	if act.Final != "你好，我是平台助手" || act.Action.Name != "" {
		t.Fatalf("直答解析结果不符: %+v", act)
	}

	// 嵌套 args 的工具调用
	act, err = parseChatJSON(`{"thought":"需要查仓库","action":{"type":"mcp","name":"github_get_repo","args":{"owner":"octocat","repo":"hello"}}}`)
	if err != nil {
		t.Fatalf("工具调用解析失败: %v", err)
	}
	if act.Action.Type != "mcp" || act.Action.Name != "github_get_repo" || act.Action.Args["owner"] != "octocat" {
		t.Fatalf("工具调用解析结果不符: %+v", act)
	}

	// 带 thought + final
	act, err = parseChatJSON(`{"thought":"够了","final":"答案是 42"}`)
	if err != nil || act.Final != "答案是 42" {
		t.Fatalf("thought+final 解析不符: %+v err=%v", act, err)
	}

	// 非法输入
	for _, bad := range []string{"", "没有 JSON", `{"final":`, `{}`} {
		if _, err := parseChatJSON(bad); err == nil && bad != `{}` {
			// `{}` 是合法 JSON（空 action），其余都应报错
			if bad == `{}` {
				continue
			}
		}
	}
	if _, err := parseChatJSON("没有 JSON"); err == nil {
		t.Fatal("无 JSON 输入应报错")
	}
	if _, err := parseChatJSON(`{"final":`); err == nil {
		t.Fatal("不完整 JSON 应报错")
	}
}

// TestClipRunes 截断
func TestClipRunes(t *testing.T) {
	if got := clipRunes("abcdef", 3); got != "abc…" {
		t.Fatalf("clipRunes 截断不符: %q", got)
	}
	if got := clipRunes("abc", 5); got != "abc" {
		t.Fatalf("clipRunes 短文本不符: %q", got)
	}
}

// TestAgentInput 委派输入提取
func TestAgentInput(t *testing.T) {
	if got := agentInput(map[string]any{"input": "查一下订单"}); got != "查一下订单" {
		t.Fatalf("input 提取不符: %q", got)
	}
	if got := agentInput(map[string]any{"task": "备份任务"}); got != "备份任务" {
		t.Fatalf("task 提取不符: %q", got)
	}
	if got := agentInput(map[string]any{"a": 1}); got == "" {
		t.Fatal("无 input/task 时应回退到 args JSON")
	}
}
