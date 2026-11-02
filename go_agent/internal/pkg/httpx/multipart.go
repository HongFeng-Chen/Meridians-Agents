// Package httpx 为 HTTP 传输层补充 Kratos 未内置的能力。
//
// Kratos 的默认请求解码器只认 application/json、application/x-www-form-urlencoded
// 等已注册 codec，遇到 multipart/form-data 会直接返回 "unregister Content-Type"。
// 而本项目有两处真实的文件上传（/api/extract、/api/upload），所以这里提供：
//
//   - RequestDecoder：替换 http.RequestDecoder，把 multipart 的普通字段按 form
//     codec 绑进 proto 请求体，把文件部分解析成 Upload 挂到请求 context 上；
//   - UploadFrom：业务侧从 context 取出文件名 / 内容类型 / 内容。
//
// 这样文件上传同样是「proto 声明路由 + 生成代码注册」，不需要为它单独开一条
// 手写路由。gRPC 调用方直接填 proto 的 bytes 字段即可（见 ResolveFile）。
package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-kratos/kratos/v3/encoding"
	"github.com/go-kratos/kratos/v3/encoding/form"
	kerrors "github.com/go-kratos/kratos/v3/errors"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"google.golang.org/protobuf/proto"
)

// MaxPartSize 单个文件部分的读取上限（业务层还会按配置再校验一次）。
const MaxPartSize int64 = 128 << 20 // 128 MiB

// Upload 一次 multipart 上传里的文件部分。
type Upload struct {
	// Field multipart 里的 part 名，如 file
	Field string
	// Filename 客户端文件名（含扩展名，解析器按它判断格式）
	Filename string
	// ContentType 浏览器给出的 MIME，可能为空
	ContentType string
	// Data 文件内容
	Data []byte
}

// Size 文件字节数
func (u *Upload) Size() int64 { return int64(len(u.Data)) }

type uploadKey struct{}

// IsMultipart 判断是否 multipart 请求（忽略 boundary 等参数）。
func IsMultipart(r *http.Request) bool {
	if r == nil {
		return false
	}
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "multipart/form-data")
}

// RequestDecoder 作为 http.RequestDecoder 使用：
// multipart 请求自行解析，其余原样交给 Kratos 默认解码器。
func RequestDecoder(r *http.Request, v any) error {
	if !IsMultipart(r) {
		return khttp.DefaultRequestDecoder(r, v)
	}

	mr, err := r.MultipartReader()
	if err != nil {
		return kerrors.BadRequest("MULTIPART", "解析上传内容失败（不是合法的 multipart 请求）: "+err.Error())
	}

	values := make(url.Values)
	var up *Upload
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return kerrors.BadRequest("MULTIPART", "读取上传内容失败: "+err.Error())
		}
		name := part.FormName()
		if name == "" { // 无名字的 part 忽略（如某些客户端追加的空段）
			_ = part.Close()
			continue
		}
		if filename := part.FileName(); filename != "" {
			data, err := io.ReadAll(io.LimitReader(part, MaxPartSize+1))
			_ = part.Close()
			if err != nil {
				return kerrors.BadRequest("MULTIPART", "读取文件内容失败: "+err.Error())
			}
			if int64(len(data)) > MaxPartSize {
				return kerrors.BadRequest("MULTIPART",
					fmt.Sprintf("文件超过 %d MiB 上限", MaxPartSize>>20))
			}
			up = &Upload{
				Field:       name,
				Filename:    filename,
				ContentType: part.Header.Get("Content-Type"),
				Data:        data,
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, 1<<20))
		_ = part.Close()
		if err != nil {
			return kerrors.BadRequest("MULTIPART", "读取表单字段失败: "+err.Error())
		}
		values.Add(name, string(data))
	}

	// 绑定目标有两种形态（由 proto 的 google.api.http body 决定）：
	//   body: "file"  → 目标只是那个 bytes 字段（*[]byte），请求体就是文件；
	//   body: "*"     → 目标是整个消息，表单字段按 form codec 绑进去。
	// 其余字段（mode/doc_id/source）由生成代码的 BindQuery 从 query 取。
	bindTarget(v, values, up)

	if up != nil {
		// 挂到请求 context 上：kratos 的 http.Context.Value 会透传到 handler 的 ctx
		*r = *r.WithContext(context.WithValue(r.Context(), uploadKey{}, up))
	}
	return nil
}

// bindTarget 按目标类型分别处理，避免把 []byte 当消息解码（会静默丢字段）。
func bindTarget(v any, values url.Values, up *Upload) {
	if data, ok := v.(*[]byte); ok {
		if up != nil {
			*data = up.Data
		}
		return
	}
	msg, ok := v.(proto.Message)
	if !ok || len(values) == 0 {
		return
	}
	// codec 的注册名是 form.Name（x-www-form-urlencoded），不是 "form"
	codec := encoding.GetCodec(form.Name)
	if codec == nil {
		return
	}
	_ = codec.Unmarshal([]byte(values.Encode()), msg)
}

// UploadFrom 取出本次请求解析出的文件部分。
func UploadFrom(ctx context.Context) (*Upload, bool) {
	if ctx == nil {
		return nil, false
	}
	up, ok := ctx.Value(uploadKey{}).(*Upload)
	return up, ok && up != nil && len(up.Data) > 0
}

// ResolveFile 统一 HTTP 与 gRPC 两种调用方式的文件入参：
//   - HTTP（multipart）：用解码器挂到 ctx 上的 Upload，文件名取客户端原名；
//   - gRPC：调用方把内容放在 data（proto 的 bytes 字段），文件名放在 name。
//
// 两者都没有时返回 400，避免下游拿到空内容才报错。
func ResolveFile(ctx context.Context, data []byte, name string) (*Upload, error) {
	if up, ok := UploadFrom(ctx); ok {
		return up, nil
	}
	if len(data) > 0 {
		if strings.TrimSpace(name) == "" {
			name = "upload.bin"
		}
		return &Upload{Field: "file", Filename: name, Data: data}, nil
	}
	return nil, kerrors.BadRequest("MULTIPART", "缺少 file 字段（HTTP 走 multipart 的 file 部分，gRPC 填 file 字节）")
}
