// Package loader 把各类文档（PDF / Word / Excel / Markdown / 纯文本）解析为纯文本，
// 供 RAG 与 GraphRAG 两条业务线共用。
//
// 设计要点：
//   - 统一出口 Extract / ExtractReader，按扩展名分发；
//   - 尽量零外部依赖：.docx 用 zip+XML 自行解析，.txt/.md/.csv 直接读；
//   - .pdf 用 ledongthuc/pdf 抽取文本层，.xlsx 用 excelize，.xls 用 extrame/xls；
//   - 旧版二进制 .doc 若本机装有 LibreOffice（soffice）则自动转换，否则给出明确提示。
package loader

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// MaxFileSize 单个文件解析上限（64MB），避免超大文件把内存打满
const MaxFileSize = 64 << 20

// supported 扩展名 -> 说明
var supported = map[string]string{
	".txt":      "纯文本",
	".md":       "Markdown",
	".markdown": "Markdown",
	".csv":      "CSV",
	".tsv":      "TSV",
	".json":     "JSON",
	".log":      "日志",
	".pdf":      "PDF",
	".docx":     "Word",
	".doc":      "Word（旧版）",
	".xlsx":     "Excel",
	".xls":      "Excel（旧版）",
	".xlsm":     "Excel（含宏）",
}

// SupportedExts 返回支持的扩展名（不含点），用于前端 accept 与文档展示
func SupportedExts() []string {
	out := make([]string, 0, len(supported))
	for ext := range supported {
		out = append(out, strings.TrimPrefix(ext, "."))
	}
	return out
}

// Describe 返回某扩展名对应的类型说明
func Describe(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if d, ok := supported[ext]; ok {
		return d
	}
	return "不支持的类型"
}

// IsSupported 判断文件名是否可解析
func IsSupported(name string) bool {
	_, ok := supported[strings.ToLower(filepath.Ext(name))]
	return ok
}

// ExtractFile 解析本地文件为纯文本
func ExtractFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是目录", path)
	}
	if st.Size() > MaxFileSize {
		return "", fmt.Errorf("文件过大（%.1f MB），上限 %d MB", float64(st.Size())/(1<<20), MaxFileSize>>20)
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".log":
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return cleanText(raw), nil
	default:
		txt, err := extractStructured(ext, path)
		if err != nil {
			return "", err
		}
		// 结构化抽取（PDF/Word/Excel）可能产出非法 UTF-8（如中文 PDF 的编码映射问题），
		// Milvus 的 gRPC 会拒绝非 UTF-8 字符串，必须统一清洗
		return normalizeText(sanitizeText(txt)), nil
	}
}

// extractStructured 按扩展名分发结构化文档抽取
func extractStructured(ext, path string) (string, error) {
	switch ext {
	case ".pdf":
		return extractPDF(path)
	case ".docx", ".xlsm":
		return extractDOCXOrXLSX(path, ext)
	case ".doc":
		return extractDOC(path)
	case ".xlsx":
		return extractXLSX(path)
	case ".xls":
		return extractXLS(path)
	default:
		return "", fmt.Errorf("不支持的文件类型 %q（支持：%s）", ext, strings.Join(SupportedExts(), ", "))
	}
}

// ExtractReader 解析上传流（浏览器上传场景）。name 仅用于判断类型。
func ExtractReader(name string, r io.Reader) (string, error) {
	ext := strings.ToLower(filepath.Ext(name))
	if !IsSupported(name) {
		return "", fmt.Errorf("不支持的文件类型 %q（支持：%s）", ext, strings.Join(SupportedExts(), ", "))
	}

	// 纯文本直接内存解析
	if ext == ".txt" || ext == ".md" || ext == ".markdown" ||
		ext == ".csv" || ext == ".tsv" || ext == ".json" || ext == ".log" {
		raw, err := io.ReadAll(io.LimitReader(r, MaxFileSize+1))
		if err != nil {
			return "", err
		}
		if len(raw) > MaxFileSize {
			return "", fmt.Errorf("文件过大，上限 %d MB", MaxFileSize>>20)
		}
		return cleanText(raw), nil
	}

	// 其余格式依赖随机读取（zip / xref），落临时文件后走统一路径
	tmp, err := os.CreateTemp("", "rag-upload-*"+ext)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := io.Copy(tmp, io.LimitReader(r, MaxFileSize+1)); err != nil {
		return "", err
	}
	if fi, err := tmp.Stat(); err == nil && fi.Size() > MaxFileSize {
		return "", fmt.Errorf("文件过大，上限 %d MB", MaxFileSize>>20)
	}
	return ExtractFile(tmp.Name())
}

// extractDOCXOrXLSX 处理 .docx / .xlsm：先看真实魔数，避免“扩展名写错”导致的解析失败
func extractDOCXOrXLSX(path, ext string) (string, error) {
	if isZip(path) {
		if ext == ".xlsm" {
			return extractXLSX(path)
		}
		return extractDOCX(path)
	}
	return "", fmt.Errorf("文件 %s 不是有效的 %s（可能是旧版二进制格式，请另存为 %s）",
		filepath.Base(path), ext, ext)
}

// isZip 检查 zip 魔数（PK\x03\x04）
func isZip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return bytes.Equal(head, []byte{'P', 'K', 0x03, 0x04})
}

// cleanText 纯文本入口：编码探测转码 + 统一格式
func cleanText(raw []byte) string {
	return normalizeText(decodeText(raw))
}

// decodeText 把原始字节转为合法 UTF-8：
//  1. UTF-16（带 BOM，Windows 导出常见）→ utf16 解码；
//  2. 已是合法 UTF-8 → 原样返回；
//  3. 非法 UTF-8 → 按 GB18030（GBK/GB2312 的超集，Windows 中文记事本默认编码）转码；
//  4. 仍失败 → 用 U+FFFD 替换非法字节兜底，保证下游 gRPC 不再拒收。
func decodeText(raw []byte) string {
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		return decodeUTF16(raw[2:], false)
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		return decodeUTF16(raw[2:], true)
	}
	if utf8.Valid(raw) {
		return strings.TrimPrefix(string(raw), "\ufeff")
	}
	if out, err := simplifiedchinese.GB18030.NewDecoder().Bytes(raw); err == nil && utf8.Valid(out) {
		return string(out)
	}
	return strings.ToValidUTF8(string(raw), "\uFFFD")
}

// decodeUTF16 解码 UTF-16 字节序列（含代理对）
func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

// sanitizeText 保证字符串是合法 UTF-8（供 PDF/Word/Excel 等抽取结果使用）。
// 这些抽取结果本质上来自 UTF-8 的 XML/内部字符串，非法字节只是零星残留，
// 因此只做逐字节替换、保留合法部分；不整串做 GB18030 转码（会把合法中文搞乱）。
func sanitizeText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// normalizeText 统一换行、压缩多余空行
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 { // 最多保留一个空行
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// normalizeExt 供内部使用：统一小写、带点
func normalizeExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		ext = strings.ToLower(name)
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
	}
	return ext
}
