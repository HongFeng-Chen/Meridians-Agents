// Package mediastore 生成类媒体（图像 / 视频）落盘与访问地址。
//
// 与聊天附件（biz/conversation）共用 data/uploads 根目录和 /api/files/
// 静态通道，但独立出 gen/ 子目录：生成物由服务端写入，命名完全随机，
// 生命周期与生成记录绑定，互不干扰。
package mediastore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// URLPrefix 生成媒体的访问地址前缀（/api/files/ 静态通道）
const URLPrefix = "/api/files/"

// GenRoot 生成媒体落盘根目录（data/uploads 下的 gen/ 子树）
const GenRoot = "data/uploads/gen"

// extByMIME 常见内容类型 → 扩展名（厂商响应不一定带文件名，按 MIME 推断）
var extByMIME = map[string]string{
	"image/png":                ".png",
	"image/jpeg":               ".jpg",
	"image/webp":               ".webp",
	"image/gif":                ".gif",
	"video/mp4":                ".mp4",
	"video/quicktime":          ".mov",
	"video/webm":               ".webm",
	"application/octet-stream": ".bin",
}

// ExtByMIME 查询 MIME 对应扩展名（未知返回空）
func ExtByMIME(mime string) string {
	if mime == "" {
		return ""
	}
	// 去掉 ;charset=... 参数
	if i := strings.IndexByte(mime, ';'); i > 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return extByMIME[strings.ToLower(mime)]
}

// SaveBytes 把一段生成产物写入本地存储，返回可直接访问的 URL。
// ext 必须带点（如 .png）；内容非空校验兜底。
func SaveBytes(ext string, r io.Reader) (string, error) {
	if ext == "" || ext[0] != '.' {
		return "", fmt.Errorf("生成媒体扩展名不合法: %q", ext)
	}
	var nameBuf [6]byte
	if _, err := rand.Read(nameBuf[:]); err != nil {
		return "", fmt.Errorf("生成文件名失败: %w", err)
	}
	bucket := time.Now().Format("200601")
	fname := time.Now().Format("20060102150405") + "-" + hex.EncodeToString(nameBuf[:]) + ext

	dir := filepath.Join(GenRoot, bucket)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建生成媒体目录失败: %w", err)
	}
	f, err := os.Create(filepath.Join(dir, fname))
	if err != nil {
		return "", fmt.Errorf("创建生成媒体文件失败: %w", err)
	}
	defer f.Close()
	n, err := io.Copy(f, r)
	if err != nil {
		os.Remove(filepath.Join(dir, fname))
		return "", fmt.Errorf("写入生成媒体失败: %w", err)
	}
	if n == 0 {
		os.Remove(filepath.Join(dir, fname))
		return "", fmt.Errorf("生成媒体内容为空")
	}
	return URLPrefix + "gen/" + bucket + "/" + fname, nil
}

// SaveFromURL 下载远端产物并落盘（厂商的临时 URL 通常很快过期，
// 落本地才能长期回看）。返回本地 URL。
func SaveFromURL(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载生成产物: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载生成产物: HTTP %d", resp.StatusCode)
	}
	ext := ExtByMIME(resp.Header.Get("Content-Type"))
	if ext == "" || ext == ".bin" {
		// 退路：按 URL 路径扩展名推断（视频厂商多回 .mp4 结尾的地址）
		ext = strings.ToLower(filepath.Ext(strings.SplitN(url, "?", 2)[0]))
	}
	if ext == "" {
		ext = ".bin"
	}
	return SaveBytes(ext, resp.Body)
}
