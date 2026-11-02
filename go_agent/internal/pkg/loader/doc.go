package loader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// extractDOC 处理旧版二进制 .doc（Word 97-2003）。
//
// Go 生态没有成熟的 .doc 纯 Go 解析库，这里按三条路径处理：
//  1. 若其实是改了扩展名的 docx（zip 魔数），直接按 docx 解析；
//  2. 若本机可用 LibreOffice（soffice），headless 转成 txt 后读取；
//  3. 否则返回明确提示，引导用户另存为 .docx。
func extractDOC(path string) (string, error) {
	if isZip(path) {
		return extractDOCX(path)
	}

	bin := findSoffice()
	if bin == "" {
		return "", errors.New(
			"旧版 .doc 为二进制格式，暂无法直接解析：请用 Word/WPS 另存为 .docx，" +
				"或安装 LibreOffice 后重启服务（将自动调用 soffice 转换）")
	}

	out, err := convertWithSoffice(bin, path, "txt")
	if err != nil {
		return "", fmt.Errorf("调用 LibreOffice 转换 .doc 失败: %w", err)
	}
	defer os.RemoveAll(filepath.Dir(out))

	raw, err := os.ReadFile(out)
	if err != nil {
		return "", err
	}
	text := cleanText(raw)
	if strings.TrimSpace(text) == "" {
		return "", errors.New("Word 文档中没有可提取的文本")
	}
	return text, nil
}

// findSoffice 在 PATH 与常见安装目录中查找 soffice 可执行文件
func findSoffice() string {
	candidates := []string{"soffice", "soffice.exe", "libreoffice"}
	if p, err := exec.LookPath("soffice"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		candidates = append(candidates,
			`C:\Program Files\LibreOffice\program\soffice.exe`,
			`C:\Program Files (x86)\LibreOffice\program\soffice.exe`,
		)
	} else {
		candidates = append(candidates, "/usr/bin/soffice", "/usr/local/bin/soffice",
			"/Applications/LibreOffice.app/Contents/MacOS/soffice")
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// convertWithSoffice 把文档转换为指定格式，返回产物路径（调用方负责清理临时目录）
func convertWithSoffice(bin, src, to string) (string, error) {
	dir, err := os.MkdirTemp("", "rag-soffice-*")
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin,
		"--headless", "--norestore", "--convert-to", to+":Text", "--outdir", dir, src)
	if output, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(output)))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	for _, e := range entries {
		if strings.EqualFold(filepath.Ext(e.Name()), "."+to) {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	os.RemoveAll(dir)
	return "", errors.New("soffice 未产出预期文件")
}
