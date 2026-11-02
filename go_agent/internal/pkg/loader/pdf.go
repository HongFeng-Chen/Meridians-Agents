package loader

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// pdfPythonTimeout 单次外部抽取的超时（200 页年报约 25~40s，留足余量）
const pdfPythonTimeout = 10 * time.Minute

// garbledThreshold 乱码字符（◆ / U+FFFD / 控制符）占比超过该值即判定抽取失败
const garbledThreshold = 0.02

//go:embed pdfextract.py
var pdfPythonScript string

var (
	scriptOnce sync.Once
	scriptPath string
	scriptErr  error

	// pythonDisabled 一旦确认 python 侧不可用（未安装 / 缺 pdfplumber），不再每篇 PDF 重试
	pythonDisabled atomic.Bool
)

// extractPDF 抽取 PDF 文本层：优先外部引擎（pdfplumber / PyMuPDF），失败或结果乱码时
// 退回纯 Go 实现（ledongthuc/pdf），按乱码率择优返回。
//
// 背景：纯 Go 库对中文财报这类「CID 字体 + 自定义 ToUnicode + 表格混排」的 PDF 会产出
// 倒序、错字和 ◆ 占位符（实测宝钢 2024 半年报乱码率 5.9%，表格行错序），因此默认走
// 外部引擎；未安装时保持原有零依赖行为。
// 扫描件（纯图片）没有文本层，会给出明确提示。
func extractPDF(path string) (string, error) {
	pyText, pyErr := extractPDFWithPython(path)
	if pyErr == nil {
		pyText = normalizeText(cleanPDFText(pyText))
		if !looksGarbled(pyText) {
			return pyText, nil
		}
	}

	nativeText, nativeErr := extractPDFNative(path)
	switch {
	case nativeErr == nil && pyErr == nil:
		// 两个引擎都成功：取乱码率更低的（相同则用外部引擎，行序通常更准）
		if garbledRatio(nativeText) < garbledRatio(pyText) {
			return nativeText, nil
		}
		return pyText, nil
	case nativeErr == nil:
		return nativeText, nil
	case pyErr == nil:
		// 原生失败但外部有结果：宁可给出带少量乱码的文本，也不整体失败
		return pyText, nil
	default:
		return "", fmt.Errorf("PDF 解析失败：%v（外部引擎：%v）", nativeErr, pyErr)
	}
}

// extractPDFNative 纯 Go 抽取（零依赖兜底）
func extractPDFNative(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开 PDF 失败: %w", err)
	}
	defer f.Close()

	var b strings.Builder
	pages := r.NumPage()
	for i := 1; i <= pages; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text := pageText(p)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if pages > 1 {
			fmt.Fprintf(&b, "\n[第 %d 页]\n", i)
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}

	out := cleanText([]byte(b.String()))
	if len([]rune(out)) < 10 {
		return "", errors.New("PDF 中没有可提取的文本（可能是扫描件/图片型 PDF，需先做 OCR）")
	}
	return out, nil
}

// extractPDFWithPython 调用本机 Python + pdfplumber/PyMuPDF 抽取。
// 可用环境变量控制：
//   - RAG_PDF_PYTHON：指定 python 可执行文件（默认自动探测 python / python3 / py）
//   - RAG_PDF_ENGINE：pdfplumber（默认）或 pymupdf
//   - RAG_PDF_DISABLE_PYTHON=1：强制只用纯 Go 实现
func extractPDFWithPython(path string) (string, error) {
	if pythonDisabled.Load() || os.Getenv("RAG_PDF_DISABLE_PYTHON") == "1" {
		return "", errors.New("外部 PDF 引擎已禁用")
	}
	py := findPython()
	if py == "" {
		pythonDisabled.Store(true)
		return "", errors.New("未找到 python，降级为纯 Go 解析")
	}
	script, err := pythonScriptFile()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), pdfPythonTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, py, script, path)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		// 缺 pdfplumber / PyMuPDF 属于「环境没装好」，后续不必再试
		if strings.Contains(msg, "No module named") || strings.Contains(msg, "ModuleNotFoundError") {
			pythonDisabled.Store(true)
		}
		return "", fmt.Errorf("python PDF 抽取失败: %v %s", err, msg)
	}
	out := stdout.String()
	if len([]rune(out)) < 10 {
		return "", errors.New("python PDF 抽取结果为空")
	}
	return out, nil
}

// findPython 探测可用的 python 解释器
func findPython() string {
	if p := strings.TrimSpace(os.Getenv("RAG_PDF_PYTHON")); p != "" {
		return p
	}
	for _, name := range []string{"python", "python3", "py"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// pythonScriptFile 把内嵌脚本落到临时文件（只写一次），避免依赖运行目录
func pythonScriptFile() (string, error) {
	scriptOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "rag-loader")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			scriptErr = err
			return
		}
		scriptPath = filepath.Join(dir, "pdfextract.py")
		if err := os.WriteFile(scriptPath, []byte(pdfPythonScript), 0o644); err != nil {
			scriptErr = err
		}
	})
	return scriptPath, scriptErr
}

// cleanPDFText 去掉 NUL 与控制字符（保留 \n \t），保证能被 Milvus / gRPC 接收
func cleanPDFText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// looksGarbled 判断抽取结果是否明显损坏
func looksGarbled(s string) bool {
	return garbledRatio(s) > garbledThreshold
}

// garbledRatio 乱码率：◆（未映射字形）、U+FFFD、其它控制符占全部字符的比例
func garbledRatio(s string) float64 {
	runes := []rune(s)
	if len(runes) == 0 {
		return 1
	}
	bad := 0
	for _, r := range runes {
		switch {
		case r == '\u25c6', r == utf8.RuneError:
			bad++
		case r < 0x20 && r != '\n' && r != '\t':
			bad++
		case unicode.Is(unicode.Co, r): // 私用区字符，通常也是字体映射失败
			bad++
		}
	}
	return float64(bad) / float64(len(runes))
}

// pageText 按 Y 坐标把文字片段聚成行，避免整页文字粘成一串
func pageText(p pdf.Page) string {
	defer func() { _ = recover() }() // 个别异常页不影响整体解析

	content := p.Content()
	if len(content.Text) == 0 {
		// 取不到坐标信息时退回纯文本抽取
		s, err := p.GetPlainText(nil)
		if err != nil {
			return ""
		}
		return s
	}

	texts := make([]pdf.Text, 0, len(content.Text))
	for _, t := range content.Text {
		// 保留纯空白片段（词间空格），否则单词会被粘连
		if t.S != "" {
			texts = append(texts, t)
		}
	}
	// 自上而下（Y 递减）、同一行内自左向右（X 递增）
	sort.SliceStable(texts, func(i, j int) bool {
		if diff := texts[i].Y - texts[j].Y; diff > 3 || diff < -3 {
			return texts[i].Y > texts[j].Y
		}
		return texts[i].X < texts[j].X
	})

	var b strings.Builder
	curY := texts[0].Y
	prevEnd := math.Inf(-1) // 上一个片段的右边界
	lineStart := true
	for _, t := range texts {
		if curY-t.Y > 3 { // 换行
			b.WriteByte('\n')
			curY = t.Y
			lineStart = true
			prevEnd = math.Inf(-1)
		}
		if !lineStart {
			// PDF 常把每个字符拆成独立片段，按字间距判断是否需要空格
			if t.X-prevEnd > t.FontSize*0.35 {
				b.WriteByte(' ')
			}
		}

		adv := t.W
		if adv <= 0 {
			adv = t.FontSize * 0.5
		}

		if strings.TrimSpace(t.S) == "" {
			// 词间空格：写一个空格后继续
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte(' ')
			}
			prevEnd = t.X + adv
			lineStart = false
			continue
		}

		b.WriteString(strings.TrimSpace(t.S))
		prevEnd = t.X + adv
		lineStart = false
	}
	return b.String()
}
