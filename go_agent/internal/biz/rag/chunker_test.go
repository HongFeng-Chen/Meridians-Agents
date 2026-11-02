package rag

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkBasic(t *testing.T) {
	c := New(100, 20)
	text := strings.Repeat("这是一段测试文本，用于验证分块功能。", 30)
	chunks := c.Chunk(text)
	if len(chunks) == 0 {
		t.Fatal("expected chunks, got 0")
	}
	for _, ch := range chunks {
		if len([]rune(ch)) > 100 {
			t.Errorf("chunk too long: %d", len([]rune(ch)))
		}
		// 回归：不得产生非法 UTF-8（此前 tailOverlap 按字节切多字节字符导致）
		if !utf8.ValidString(ch) {
			t.Errorf("chunk contains invalid UTF-8: %q", ch[:min(40, len(ch))])
		}
	}
}

// 中文句号等多字节字符不得被从中间切断
func TestChunkMultiByteBoundary(t *testing.T) {
	c := New(50, 10)
	text := strings.Repeat("甲乙丙丁戊己庚辛。", 20)
	chunks := c.Chunk(text)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, ch := range chunks {
		if !utf8.ValidString(ch) {
			t.Errorf("chunk %d contains invalid UTF-8 (rune split): %q", i, ch[:min(40, len(ch))])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestChunkEmpty(t *testing.T) {
	c := New(100, 20)
	if got := c.Chunk("   "); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestChunkOverlap(t *testing.T) {
	c := New(50, 10)
	text := strings.Repeat("段落内容甲乙丙丁。\n", 40)
	chunks := c.Chunk(text)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	// 相邻块之间应有内容衔接（存在部分字符重叠）
	joined := strings.Join(chunks, "\n")
	if len(joined) < len(text)/2 {
		t.Error("chunks lost too much content")
	}
}
