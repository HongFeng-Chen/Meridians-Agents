// generations.go JSON 形态的图片生成：POST /images/generations。
// 文生图，以及非 openai 数据源的图生图（SiliconFlow Kolors 风格，
// 参考图通过 image 字段以 base64 / data URI / URL 传入）。
package image_generations

import (
	"context"

	"go_agent/internal/biz/llm"
)

// generationsJSON 文生图（及非 openai 的图生图）
func (c *ImageClient) generationsJSON(ctx context.Context, base, apiKey, model string, in *llm.ImageGenInput) ([]imgOutput, error) {
	payload := map[string]any{
		"model":  model,
		"prompt": in.Prompt,
		"n":      in.Count,
	}
	if in.Size != "" {
		payload["size"] = in.Size
	}
	if in.Seed != nil {
		payload["seed"] = *in.Seed
	}
	if in.Mode == llm.ModeImg2Img {
		// SiliconFlow Kolors 风格：image 字段为 base64 / data URI / URL；
		// 单图传字符串（兼容面最广），多图传数组
		if len(in.Images) == 1 {
			payload["image"] = in.Images[0]
		} else {
			payload["image"] = in.Images
		}
		// OpenAI 协议的 generations 不接受 image，个别网关会 400；
		// 参数缺失比 400 好排查，故不额外删字段
	}
	out, err := c.postJSON(ctx, base+"/images/generations", apiKey, payload)
	if err != nil {
		return nil, err
	}
	return parseOutputs(out)
}
