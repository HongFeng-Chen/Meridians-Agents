package rag

import (
	"strings"
	"unicode/utf8"
)

// Chunker 文本分块器
type Chunker struct {
	MaxLen  int // 单块最大字符数
	Overlap int // 相邻块重叠字符数
}

// New 创建分块器
func New(maxLen, overlap int) *Chunker {
	if maxLen < 100 {
		maxLen = 100
	}
	if overlap < 0 || overlap >= maxLen {
		overlap = maxLen / 5
	}
	return &Chunker{MaxLen: maxLen, Overlap: overlap}
}

// 按优先级递归切分的分隔符（先段落、后句子）
var separators = []string{"\n\n", "\n", "。", "！", "？", "！", ".", "?", "；", ";", "，", ",", " ", ""}

// Chunk 将长文本切分为若干重叠块（全程按字符计数，杜绝切断多字节字符）
func (c *Chunker) Chunk(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	pieces := c.splitRecursive(text, 0)

	// 合并小片段成块
	var chunks []string
	var buf strings.Builder
	bufRunes := 0 // buf 当前字符数（Builder.Len() 是字节数，中文下是字符数的 ~3 倍，不能混用）
	for _, p := range pieces {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pRunes := utf8.RuneCountInString(p)
		if bufRunes > 0 && bufRunes+pRunes+1 > c.MaxLen {
			chunk := buf.String()
			chunks = append(chunks, chunk)
			// 保留尾部重叠；若尾部加新片段会超出 MaxLen，则放弃重叠
			tail := c.tailOverlap(chunk)
			tRunes := utf8.RuneCountInString(tail)
			buf.Reset()
			if tRunes > 0 && tRunes+pRunes+1 <= c.MaxLen {
				buf.WriteString(tail)
				bufRunes = tRunes
			} else {
				bufRunes = 0
			}
		}
		if bufRunes > 0 {
			buf.WriteString("\n")
			bufRunes++
		}
		buf.WriteString(p)
		bufRunes += pRunes
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		chunks = append(chunks, s)
	}
	return chunks
}

// splitRecursive 递归按分隔符切分，直到所有片段足够小
func (c *Chunker) splitRecursive(s string, level int) []string {
	runes := []rune(s)
	if len(runes) <= c.MaxLen || level >= len(separators) {
		return []string{s}
	}
	sep := separators[level]
	parts := strings.Split(s, sep)
	if len(parts) == 1 {
		return c.splitRecursive(s, level+1)
	}

	var out []string
	var buf strings.Builder
	bufRunes := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pRunes := utf8.RuneCountInString(p)
		if pRunes > c.MaxLen {
			// 递归进入下一级分隔符
			out = append(out, c.splitRecursive(p, level+1)...)
			continue
		}
		if bufRunes > 0 && bufRunes+pRunes+utf8.RuneCountInString(sep) > c.MaxLen {
			out = append(out, buf.String())
			buf.Reset()
			bufRunes = 0
		}
		if bufRunes > 0 && sep != "" {
			buf.WriteString(sep)
			bufRunes += utf8.RuneCountInString(sep)
		}
		buf.WriteString(p)
		bufRunes += pRunes
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// tailOverlap 提取块尾部用于重叠的文本
func (c *Chunker) tailOverlap(chunk string) string {
	runes := []rune(chunk)
	if len(runes) <= c.Overlap {
		return chunk
	}
	// 尽量从句子边界开始。
	// 注意：IndexAny 返回字节下标，必须按 rune 宽度前进，
	// 否则对"。"等多字节字符会切进字符内部，产生非法 UTF-8（gRPC 拒收的根源）
	tail := string(runes[len(runes)-c.Overlap:])
	if idx := strings.IndexAny(tail, "。！？!?.\n"); idx >= 0 {
		if _, size := utf8.DecodeRuneInString(tail[idx:]); idx+size < len(tail) {
			tail = tail[idx+size:]
		}
	}
	return strings.TrimSpace(tail)
}
