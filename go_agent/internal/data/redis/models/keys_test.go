package models

import (
	"strings"
	"testing"
)

func TestShortSessionKey(t *testing.T) {
	if got := ShortSessionKey("s-1"); got != "mem:short:s-1" {
		t.Errorf("ShortSessionKey = %q", got)
	}
	if got := ShortSessionScanPattern(); got != "mem:short:*" {
		t.Errorf("ShortSessionScanPattern = %q", got)
	}
	// 会话键必须被 SCAN 模式覆盖，否则管理端的枚举/统计会漏
	if !strings.HasPrefix(ShortSessionKey("s-1"), strings.TrimSuffix(ShortSessionScanPattern(), "*")) {
		t.Error("会话键不在 SCAN 模式覆盖范围内")
	}
}

func TestPromptVersionKey(t *testing.T) {
	if got := PromptVersionKey("rag.system"); got != "prompt:ver:rag.system" {
		t.Errorf("PromptVersionKey = %q", got)
	}
}

func TestAnswerKeys(t *testing.T) {
	if got := AnswerScanPattern(); got != "rag:ans:*" {
		t.Errorf("AnswerScanPattern = %q", got)
	}
	// 提示词缓存与问答缓存必须是不同前缀：RAG 失效不应连带清掉提示词缓存
	if strings.HasPrefix(PromptVersionPrefix, AnswerPrefix) || strings.HasPrefix(AnswerPrefix, PromptVersionPrefix) {
		t.Error("rag 与 prompt 的键前缀发生了重叠")
	}
	// 入库锁与问答缓存也必须分开
	if strings.HasPrefix(IngestLockPrefix, AnswerPrefix) {
		t.Error("入库锁前缀不应落在问答缓存前缀下")
	}
}

func TestMaxSessionMessages(t *testing.T) {
	if MaxSessionMessages <= 0 {
		t.Fatalf("MaxSessionMessages = %d，必须为正数", MaxSessionMessages)
	}
}

// TestScopedSessionKey 短期记忆键必须带租户 + 用户作用域：
// 同一个 session_id 在不同身份下必须是不同的键（否则知道 ID 就能读别人的对话）。
func TestScopedSessionKey(t *testing.T) {
	alice := ScopedSessionKey("t1", "alice", "s-1")
	bob := ScopedSessionKey("t1", "bob", "s-1")
	otherTenant := ScopedSessionKey("t2", "alice", "s-1")

	if alice == bob || alice == otherTenant {
		t.Fatalf("不同身份不能共用会话键: alice=%q bob=%q otherTenant=%q", alice, bob, otherTenant)
	}
	if got, want := alice, "mem:short:t1:alice:s-1"; got != want {
		t.Errorf("ScopedSessionKey = %q, want %q", got, want)
	}
	// 仍要落在 SCAN 模式内，管理端枚举才看得到
	if !strings.HasPrefix(alice, strings.TrimSuffix(ShortSessionScanPattern(), "*")) {
		t.Error("带作用域的会话键不在 SCAN 模式覆盖范围内")
	}
	// 空归属不能塌成 mem:short:::s-1（会与另一个空归属用户撞键）
	empty := ScopedSessionKey("", "", "s-1")
	if strings.Contains(empty, ":::") {
		t.Errorf("空归属应使用占位符，实际 %q", empty)
	}
	if empty == ScopedSessionKey("", "alice", "s-1") {
		t.Error("空归属与具名用户不能共用会话键")
	}
}
