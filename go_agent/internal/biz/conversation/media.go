// media.go 聊天附件（图片 / 视频）落盘与访问地址。
//
// 存储：data/uploads/{yyyymm}/{随机文件名}{ext}，相对进程工作目录；
// 访问：/api/files/{yyyymm}/{文件名}（internal/server/http.go 注册静态前缀）。
// 校验：MIME 白名单（image/* 常用格式、video/mp4|webm|quicktime）、大小上限
// （图片 10MB / 视频 50MB）、文件名不落盘（只用原名做展示与扩展名推断）。
package conversation

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 附件约束
const (
	// MaxAttachments 单条消息附件上限
	MaxAttachments = 9
	maxImageBytes  = 10 << 20 // 10MB
	maxVideoBytes  = 50 << 20 // 50MB
	// mediaURLPrefix 附件访问地址前缀（与 http.go 的静态注册一致）
	mediaURLPrefix = "/api/files/"
	mediaRoot      = "data/uploads"
)

// allowedImageExt / allowedVideoExt 上传白名单（按扩展名推断 MIME，防伪装）
var allowedImageExt = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
}
var allowedVideoExt = map[string]string{
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".m4v": "video/x-m4v",
}

// SaveMedia 校验并把上传文件写入本地存储，返回可直接放进消息的附件。
// name 为客户端原始文件名（只用于扩展名推断与展示，不作为磁盘文件名）。
func (u *UseCase) SaveMedia(name string, r io.Reader) (*Attachment, error) {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(name)))
	mimeType, ok := allowedImageExt[ext]
	limit := int64(maxImageBytes)
	kind := "图片"
	if !ok {
		mimeType, ok = allowedVideoExt[ext]
		limit = int64(maxVideoBytes)
		kind = "视频"
	}
	if !ok {
		return nil, fmt.Errorf("%w: 不支持的附件类型 %q（图片：jpg/png/gif/webp/bmp，视频：mp4/webm/mov）", ErrInvalidConv, ext)
	}

	// 随机文件名：防路径穿越、防重名、防枚举
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, fmt.Errorf("生成文件名失败: %w", err)
	}
	bucket := time.Now().Format("200601")
	fname := time.Now().Format("20060102150405") + "-" + hex.EncodeToString(buf[:]) + ext

	dir := filepath.Join(mediaRoot, bucket)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建附件目录失败: %w", err)
	}
	dstPath := filepath.Join(dir, fname)
	f, err := os.Create(dstPath)
	if err != nil {
		return nil, fmt.Errorf("创建附件文件失败: %w", err)
	}
	defer f.Close()

	// 多读 1 字节判断是否超限
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if err != nil {
		os.Remove(dstPath)
		return nil, fmt.Errorf("写入附件失败: %w", err)
	}
	if n > limit {
		os.Remove(dstPath)
		return nil, fmt.Errorf("%w: %s大小超过上限（%d MB）", ErrInvalidConv, kind, limit>>20)
	}
	if n == 0 {
		os.Remove(dstPath)
		return nil, fmt.Errorf("%w: 附件内容为空", ErrInvalidConv)
	}

	// 展示名：去掉路径部分，只留文件名
	display := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	return &Attachment{
		Name: display,
		URL:  mediaURLPrefix + bucket + "/" + fname,
		MIME: mimeType,
		Size: n,
	}, nil
}

// MimeByExt 根据扩展名返回 MIME（静态访问时兜底用，未知类型 application/octet-stream）
func MimeByExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if m, ok := allowedImageExt[ext]; ok {
		return m
	}
	if m, ok := allowedVideoExt[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	return "application/octet-stream"
}
