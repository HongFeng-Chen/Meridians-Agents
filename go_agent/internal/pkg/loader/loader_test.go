package loader

import (
	"archive/zip"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// makeDOCX 生成一个最小可用的 .docx，用于验证 zip+XML 解析
func makeDOCX(t *testing.T, paragraphs []string, table [][]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.docx")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		b.WriteString(`<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`)
	}
	if len(table) > 0 {
		b.WriteString(`<w:tbl>`)
		for _, row := range table {
			b.WriteString(`<w:tr>`)
			for _, cell := range row {
				b.WriteString(`<w:tc><w:p><w:r><w:t>` + cell + `</w:t></w:r></w:p></w:tc>`)
			}
			b.WriteString(`</w:tr>`)
		}
		b.WriteString(`</w:tbl>`)
	}
	b.WriteString(`</w:body></w:document>`)
	if _, err := w.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractDOCX(t *testing.T) {
	path := makeDOCX(t,
		[]string{"第一段：Milvus 3.0 支持 BM25 全文检索。", "第二段：GraphRAG 使用知识图谱增强问答。"},
		[][]string{{"字段", "说明"}, {"dense", "稠密向量"}},
	)

	text, err := ExtractFile(path)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}
	for _, want := range []string{"第一段", "BM25", "第二段", "字段", "dense", "稠密向量"} {
		if !strings.Contains(text, want) {
			t.Errorf("提取文本缺少 %q，实际内容：\n%s", want, text)
		}
	}
}

func TestExtractDOCX_RenamedDOC(t *testing.T) {
	// 改了扩展名的 docx（.doc）应按 docx 解析
	src := makeDOCX(t, []string{"扩展名写错的文档"}, nil)
	dst := filepath.Join(t.TempDir(), "renamed.doc")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := ExtractFile(dst)
	if err != nil {
		t.Fatalf("ExtractFile(.doc as docx): %v", err)
	}
	if !strings.Contains(text, "扩展名写错") {
		t.Errorf("未提取到内容：%s", text)
	}
}

func TestExtractXLSX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.xlsx")
	f := excelize.NewFile()
	defer f.Close()

	sheet := f.GetSheetName(0)
	f.SetSheetName(sheet, "产品")
	_ = f.SetSheetRow("产品", "A1", &[]string{"型号", "价格", "库存"})
	_ = f.SetSheetRow("产品", "A2", &[]string{"X100", "3999", "12"})
	_ = f.SetSheetRow("产品", "A3", &[]string{"X200", "5999", "5"})

	f.NewSheet("备注")
	_ = f.SetSheetRow("备注", "A1", &[]string{"价格含税"})

	if err := f.SaveAs(path); err != nil {
		t.Fatal(err)
	}

	text, err := ExtractFile(path)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}
	for _, want := range []string{"工作表：产品", "型号 | 价格 | 库存", "X100 | 3999 | 12", "价格含税"} {
		if !strings.Contains(text, want) {
			t.Errorf("提取文本缺少 %q，实际内容：\n%s", want, text)
		}
	}
}

func TestExtractPlainText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.md")
	content := "\ufeff# 标题\r\n\r\n\r\n正文一行\r\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := ExtractFile(path)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}
	if strings.HasPrefix(text, "\ufeff") {
		t.Error("BOM 未被去除")
	}
	if strings.Contains(text, "\r") {
		t.Error("换行未统一为 \\n")
	}
	if strings.Contains(text, "\n\n\n") {
		t.Error("多余空行未压缩")
	}
	if !strings.Contains(text, "正文一行") {
		t.Errorf("内容缺失：%s", text)
	}
}

// makePDF 生成单页最小 PDF（Helvetica + 文本层），用于验证 PDF 解析
func makePDF(t *testing.T, lines []string) string {
	t.Helper()
	var content strings.Builder
	content.WriteString("BT /F1 14 Tf 72 760 Td ")
	for i, ln := range lines {
		if i > 0 {
			content.WriteString("0 -22 Td ")
		}
		content.WriteString("(" + ln + ") Tj ")
	}
	content.WriteString("ET")

	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] " +
			"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var buf []byte
	buf = append(buf, "%PDF-1.4\n"...)
	offsets := make([]int, len(objs))
	for i, body := range objs {
		offsets[i] = len(buf)
		buf = append(buf, fmt.Sprintf("%d 0 obj\n%s\nendobj\n", i+1, body)...)
	}
	xref := len(buf)
	buf = append(buf, fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)...)
	for _, off := range offsets {
		buf = append(buf, fmt.Sprintf("%010d 00000 n \n", off)...)
	}
	buf = append(buf, fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objs)+1, xref)...)

	path := filepath.Join(t.TempDir(), "sample.pdf")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractPDF(t *testing.T) {
	path := makePDF(t, []string{
		"Milvus 3.0 BM25 hybrid retrieval",
		"GraphRAG stores entities in Milvus",
	})

	text, err := ExtractFile(path)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}
	if !strings.Contains(text, "Milvus 3.0 BM25 hybrid retrieval") {
		t.Errorf("PDF 文本未按行还原，实际：%q", text)
	}
	if !strings.Contains(text, "GraphRAG stores entities") {
		t.Errorf("PDF 第二行缺失，实际：%q", text)
	}
}

// 乱码率判定：◆ / U+FFFD 占比是「抽取是否可信」的闸门（超过 2% 判失败，换引擎）
func TestGarbledRatio(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"干净文本", "宝钢股份2024年半年度报告，营业收入xxx亿元。", 0},
		{"一半乱码", "◆◆一半◆乱码", 3.0 / 7},
		{"替换符", "正常\ufffd\ufffd\ufffd文字", 3.0 / 7},
		{"空串", "", 1},
	}
	for _, c := range cases {
		if got := garbledRatio(c.in); math.Abs(got-c.want) > 1e-6 {
			t.Fatalf("%s: garbledRatio = %v, want %v", c.name, got, c.want)
		}
	}
	if looksGarbled("宝钢股份2024年半年度报告") {
		t.Fatal("正常文本被判为乱码")
	}
	if !looksGarbled(strings.Repeat("◆", 10) + "正常") {
		t.Fatal("高比例乱码未被识别")
	}
}

// 外部引擎不可用时（未装 python / 显式禁用）必须回落到纯 Go 抽取，不能整体失败
func TestExtractPDFWithoutPython(t *testing.T) {
	t.Setenv("RAG_PDF_DISABLE_PYTHON", "1")
	path := makePDF(t, []string{
		"Milvus 3.0 BM25 hybrid retrieval",
		"GraphRAG stores entities in Milvus",
	})
	text, err := ExtractFile(path)
	if err != nil {
		t.Fatalf("禁用 python 后仍应能解析: %v", err)
	}
	if !strings.Contains(text, "Milvus 3.0 BM25 hybrid retrieval") {
		t.Fatalf("纯 Go 兜底结果异常：%q", text)
	}
}

// 外部引擎可用时优先使用（ascii 样本上应与原生结果一致，保证格式兼容）
func TestExtractPDFPythonEngine(t *testing.T) {
	if findPython() == "" || pythonDisabled.Load() {
		t.Skip("本机没有可用 python")
	}
	path := makePDF(t, []string{"hello python engine", "second line"})
	text, err := ExtractFile(path)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}
	if !strings.Contains(text, "hello python engine") {
		t.Fatalf("外部引擎结果异常：%q", text)
	}
}

func TestExtractUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.xyz")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractFile(path); err == nil {
		t.Fatal("不支持的类型应报错")
	}
	if IsSupported("a.xyz") {
		t.Error("IsSupported 应为 false")
	}
	for _, name := range []string{"a.pdf", "a.DOCX", "a.xlsx", "a.xls", "a.md"} {
		if !IsSupported(name) {
			t.Errorf("%s 应被支持", name)
		}
	}
}

func TestExtractReader(t *testing.T) {
	src := makeDOCX(t, []string{"上传流解析"}, nil)
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	text, err := ExtractReader("上传.docx", f)
	if err != nil {
		t.Fatalf("ExtractReader: %v", err)
	}
	if !strings.Contains(text, "上传流解析") {
		t.Errorf("未提取到内容：%s", text)
	}
}

// GBK 编码文本（Windows 中文记事本默认编码）应自动转码为 UTF-8
func TestCleanTextGBK(t *testing.T) {
	enc := simplifiedchinese.GBK.NewEncoder()
	gbk, err := enc.Bytes([]byte("向量数据库支持混合检索"))
	if err != nil {
		t.Fatal(err)
	}
	got := cleanText(gbk)
	if !utf8.ValidString(got) {
		t.Fatalf("GBK 转码后仍非合法 UTF-8：%q", got)
	}
	if !strings.Contains(got, "向量数据库") {
		t.Errorf("GBK 转码内容错误：%q", got)
	}
}

// UTF-16 LE（带 BOM）应正确解码
func TestCleanTextUTF16LE(t *testing.T) {
	src := "GraphRAG 图谱增强检索"
	u16 := utf16.Encode([]rune(src))
	raw := []byte{0xFF, 0xFE} // LE BOM
	for _, r := range u16 {
		raw = append(raw, byte(r), byte(r>>8))
	}
	got := cleanText(raw)
	if !strings.Contains(got, src) {
		t.Errorf("UTF-16 解码内容错误：%q", got)
	}
}

// 非法 UTF-8 且非 GBK 的二进制残留，兜底后必须是合法 UTF-8（不再触发 Milvus 拒收）
func TestCleanTextInvalidUTF8Fallback(t *testing.T) {
	raw := []byte{'a', 0xff, 0xfe, 'b', 0x80, 'c'}
	got := cleanText(raw)
	if !utf8.ValidString(got) {
		t.Fatalf("兜底清洗后仍非合法 UTF-8：%q", got)
	}
}

// 结构化抽取结果（如 PDF 产出的非法 UTF-8）经 ExtractFile 出口必须被清洗
func TestSanitizeText(t *testing.T) {
	bad := "正常开头\x80\xff非法字节"
	got := sanitizeText(bad)
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeText 后仍非合法 UTF-8：%q", got)
	}
	if !strings.Contains(got, "正常开头") {
		t.Errorf("合法前缀丢失：%q", got)
	}
}
