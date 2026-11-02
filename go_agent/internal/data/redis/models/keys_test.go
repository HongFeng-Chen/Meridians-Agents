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
