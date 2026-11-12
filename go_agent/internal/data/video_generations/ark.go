// ark.go 火山引擎 Ark 协议：/content/generation/tasks 提交与轮询。
// 豆包·视频生成模型（doubao-seedance 等）走此协议。
package video_generations

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go_agent/internal/biz/llm"
	"go_agent/internal/pkg/mediastore"
)

func (c *VideoClient) submitArk(ctx context.Context, base, apiKey, model string, in *llm.VideoGenInput) (string, error) {
	content := []map[string]any{{"type": "text", "text": in.Prompt + textSuffix(in)}}
	if in.Image != "" {
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{"url": in.Image}})
	}
	payload := map[string]any{"model": model, "content": content}
	out, err := c.postJSON(ctx, base+"/content/generation/tasks", apiKey, payload)
	if err != nil {
		return "", err
	}
	if id, ok := out["id"].(string); ok && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("提交响应里没有任务 ID: %v", out)
}

func (c *VideoClient) pollArk(ctx context.Context, base, apiKey, taskID string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/content/generation/tasks/"+taskID, nil)
	if err != nil {
		return "", "", err
	}
	setKey(req, apiKey)
	out, err := c.doJSON(req)
	if err != nil {
		return "", "", err
	}
	status, _ := out["status"].(string)
	switch strings.ToLower(status) {
	case "succeeded":
		// 成品在 content.video_url
		url := ""
		if content, ok := out["content"].(map[string]any); ok {
			url, _ = content["video_url"].(string)
		}
		if url == "" {
			return llm.MediaFailed, "任务成功但响应里没有视频地址", nil
		}
		local, derr := mediastore.SaveFromURL(ctx, url)
		if derr != nil {
			return llm.MediaFailed, "成品下载失败: " + derr.Error(), nil
		}
		return llm.MediaSucceeded, local, nil
	case "failed", "cancelled":
		return llm.MediaFailed, "厂商任务失败: " + status, nil
	case "queued":
		return llm.MediaQueued, "", nil
	default: // running / 其它
		return llm.MediaRunning, "", nil
	}
}

// textSuffix Ark 的文生视频要求 prompt 里带分辨率 / 时长描述，
// 结构化参数拼到文本后面（厂商按文本解析）
func textSuffix(in *llm.VideoGenInput) string {
	var parts []string
	if in.Size != "" {
		parts = append(parts, "--resolution "+in.Size)
	}
	if in.DurationSec > 0 {
		parts = append(parts, fmt.Sprintf("--duration %d", in.DurationSec))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}
