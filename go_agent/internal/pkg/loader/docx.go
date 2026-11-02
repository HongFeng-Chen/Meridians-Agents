package loader

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// extractDOCX 解析 .docx：docx 本质是 zip 包，正文在 word/document.xml。
// 直接流式扫描 XML，抽取 <w:t> 文本、段落换行、表格单元格分隔，无需第三方库。
func extractDOCX(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("打开 Word 文档失败: %w", err)
	}
	defer zr.Close()

	var doc *zip.File
	for _, f := range zr.File {
		switch f.Name {
		case "word/document.xml":
			doc = f
		}
	}
	if doc == nil {
		return "", errors.New("不是有效的 .docx 文件：缺少 word/document.xml")
	}

	rc, err := doc.Open()
	if err != nil {
		return "", fmt.Errorf("读取 word/document.xml 失败: %w", err)
	}
	defer rc.Close()

	text, err := parseWordDocument(rc)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("Word 文档中没有可提取的文本（可能只包含图片）")
	}
	return text, nil
}

// parseWordDocument 流式解析 word/document.xml
func parseWordDocument(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var b strings.Builder

	var inText, inInstr bool
	tableDepth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("解析 Word XML 失败: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t": // <w:t> 文本
				inText = true
			case "instrText", "delText": // 域代码 / 修订删除，忽略
				inInstr = true
			case "tab":
				b.WriteByte('\t')
			case "tbl":
				tableDepth++
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "instrText", "delText":
				inInstr = false
			case "p": // 段落：表格内用空格接续，正文用换行
				if tableDepth > 0 {
					b.WriteByte(' ')
				} else {
					b.WriteByte('\n')
				}
			case "tc": // 单元格之间用 | 分隔
				b.WriteString("| ")
			case "tr": // 表格一行结束
				b.WriteByte('\n')
			case "tbl":
				if tableDepth > 0 {
					tableDepth--
				}
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText && !inInstr {
				b.Write(t)
			}
		}
	}

	// 清理表格行尾残留的分隔符与多余空格
	lines := strings.Split(b.String(), "\n")
	for i, ln := range lines {
		ln = strings.TrimRight(ln, " \t|")
		ln = strings.ReplaceAll(ln, "|  ", "| ")
		lines[i] = strings.TrimRight(ln, " \t")
	}
	return cleanText([]byte(strings.Join(lines, "\n"))), nil
}
