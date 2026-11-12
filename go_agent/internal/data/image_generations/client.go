// client.go 图片生成客户端入口（OpenAI 兼容 /images 协议族）。
//
// 路由：openai 的图生图走 multipart /images/edits（见 edits.go），
// 其余（文生图、SiliconFlow Kolors 等非 openai 的图生图）走
// JSON /images/generations（见 generations.go）。
// 统一流程：发请求 → 解析 data[].url / data[].b64_json（兼容部分厂商的
// images[] 字段）→ 每个产物转存本地 → 返回 /api/files/ 地址列表。
package image_generations

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go_agent/internal/biz/llm"
)

// ImageClient 图片生成客户端
type ImageClient struct {
	http *http.Client
}

// NewImageClient 创建客户端
func NewImageClient() *ImageClient {
	return &ImageClient{http: &http.Client{}}
}

var _ llm.ImageGenClient = (*ImageClient)(nil)

// Generate 生成图片并转存本地。请求超时由目标快照携带（媒体生成默认 ≥300s）。
func (c *ImageClient) Generate(ctx context.Context, t llm.MediaTarget, in *llm.ImageGenInput) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	base := strings.TrimRight(t.BaseURL, "/")
	var outputs []imgOutput
	switch {
	case in.Mode == llm.ModeImg2Img && t.Kind == llm.KindOpenAI:
		// gpt-image-1 的图生图走 multipart edits（参考图作为文件上传）
		var err error
		outputs, err = c.generationsEdits(ctx, base, t.APIKey, t.Model, in)
		if err != nil {
			return nil, err
		}
	default:
		var err error
		outputs, err = c.generationsJSON(ctx, base, t.APIKey, t.Model, in)
		if err != nil {
			return nil, err
		}
	}

	// 转存：b64 直接解码；url 下载
	urls := make([]string, 0, len(outputs))
	for i, o := range outputs {
		local, err := c.persist(ctx, o, i)
		if err != nil {
			// 一张失败不拖累整批：已落盘的照常返回
			continue
		}
		urls = append(urls, local)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("厂商未返回可保存的图片产物")
	}
	return urls, nil
}
