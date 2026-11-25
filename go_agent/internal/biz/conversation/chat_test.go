package conversation

import (
	"testing"

	"go_agent/internal/biz/memory"
	"go_agent/internal/biz/rag"
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

// TestFilterMemories 记忆绝对分数线过滤：低于 0.15 的弱相关（如 0.03 量级噪声）应被丢弃
func TestFilterMemories(t *testing.T) {
	mems := []memory.RecalledMemory{
		{LongTermMemory: &memory.LongTermMemory{ID: "a"}, Score: 0.42},
		{LongTermMemory: &memory.LongTermMemory{ID: "b"}, Score: 0.15},
		{LongTermMemory: &memory.LongTermMemory{ID: "noise"}, Score: 0.033},
		{LongTermMemory: nil, Score: 0.9}, // 无正文体：保留（由 memoryLinesOf 跳过）
	}
	kept := filterMemories(mems, chatMemMinScore)
	if len(kept) != 3 {
		t.Fatalf("应保留 3 条（0.033 视为噪声），实际 %d: %+v", len(kept), kept)
	}
	if kept[0].LongTermMemory.ID != "a" || kept[1].LongTermMemory.ID != "b" {
		t.Fatalf("保留顺序/条目不符: %+v", kept)
	}
	if got := filterMemories(nil, chatMemMinScore); len(got) != 0 {
		t.Fatalf("空入参应返回空切片")
	}
}

// TestFilterAbsScore RRF 绝对分数线过滤（实现在 biz/rag.FilterAbsScore，会话与 RAG 问答共用）
func TestFilterAbsScore(t *testing.T) {
	hits := []rag.Hit{
		{DocID: "top", Score: 0.0328},  // 双通道双第一
		{DocID: "mid", Score: 0.0164},  // 单通道第一，过线
		{DocID: "tail", Score: 0.0137}, // < 0.0145 → 丢
		{DocID: "tail2", Score: 0.01},  // 丢
	}
	kept := rag.FilterAbsScore(hits, chatRagMinRRF)
	if len(kept) != 2 || kept[0].DocID != "top" || kept[1].DocID != "mid" {
		t.Fatalf("应保留 top/mid 两条，实际: %+v", kept)
	}
	if got := rag.FilterAbsScore(nil, chatRagMinRRF); got != nil {
		t.Fatalf("空入参应返回 nil")
	}
	// 全部低于线：无保底，滤空即空（会话链路拿到空切片 = 不拼 RAG 上下文段）
	low := []rag.Hit{{DocID: "a", Score: 0.008}}
	if got := rag.FilterAbsScore(low, chatRagMinRRF); len(got) != 0 {
		t.Fatalf("全部低于线应滤空，实际 %d 条", len(got))
	}
	// min ≤ 0：不过滤
	if got := rag.FilterAbsScore(hits, 0); len(got) != 4 {
		t.Fatalf("min=0 应不过滤，实际 %d", len(got))
	}
}
