// outputs.go 产物模型：响应解析与落盘。
package image_generations

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	"go_agent/internal/pkg/mediastore"
)

// imgOutput 厂商返回的单个产物
type imgOutput struct {
	URL string
	B64 string
	Ext string // b64 解码时用（厂商不回格式，默认 png）
}

// parseOutputs 兼容两种响应形态：OpenAI 的 data[]、部分厂商的 images[]
func parseOutputs(out map[string]any) ([]imgOutput, error) {
	for _, key := range []string{"data", "images"} {
		arr, ok := out[key].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		var outputs []imgOutput
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			o := imgOutput{Ext: ".png"}
			if u, ok := m["url"].(string); ok && u != "" {
				o.URL = u
			}
			if b, ok := m["b64_json"].(string); ok && b != "" {
				o.B64 = b
			}
			// 个别厂商把 MIME 放在 content_type / mime_type
			if mt, ok := m["content_type"].(string); ok {
				if ext := mediastore.ExtByMIME(mt); ext != "" {
					o.Ext = ext
				}
			}
			if o.URL != "" || o.B64 != "" {
				outputs = append(outputs, o)
			}
		}
		if len(outputs) > 0 {
			return outputs, nil
		}
	}
	return nil, fmt.Errorf("响应里没有图片产物（data / images 均为空）")
}

// persist 产物落盘，返回本地 URL
func (c *ImageClient) persist(ctx context.Context, o imgOutput, idx int) (string, error) {
	if o.B64 != "" {
		data, err := base64.StdEncoding.DecodeString(o.B64)
		if err != nil {
			return "", fmt.Errorf("b64 解码: %w", err)
		}
		return mediastore.SaveBytes(o.Ext, bytes.NewReader(data))
	}
	return mediastore.SaveFromURL(ctx, o.URL)
}
