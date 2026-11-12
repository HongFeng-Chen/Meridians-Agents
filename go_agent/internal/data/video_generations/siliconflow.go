// siliconflow.go SiliconFlow 协议：/video/submit 提交 + /video/status 轮询。
// 国内视频厂商（智谱 CogVideoX、MiniMax、通义万相等）的事实标准协议。
package video_generations

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go_agent/internal/biz/llm"
	"go_agent/internal/pkg/mediastore"
)

func (c *VideoClient) submitSiliconFlow(ctx context.Context, base, apiKey, model string, in *llm.VideoGenInput) (string, error) {
	payload := map[string]any{"model": model, "prompt": in.Prompt}
	if in.Image != "" {
		payload["image_url"] = in.Image
	}
	if in.Seed != nil {
		payload["seed"] = *in.Seed
	}
	out, err := c.postJSON(ctx, base+"/video/submit", apiKey, payload)
	if err != nil {
		return "", err
	}
	for _, key := range []string{"requestId", "request_id", "id"} {
		if id, ok := out[key].(string); ok && id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("提交响应里没有任务 ID: %v", out)
}

func (c *VideoClient) pollSiliconFlow(ctx context.Context, base, apiKey, taskID string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/video/status?requestId="+taskID, nil)
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
	case "succeed", "succeeded", "success":
		url := firstVideoURL(out)
		if url == "" {
			return llm.MediaFailed, "任务成功但响应里没有视频地址", nil
		}
		local, derr := mediastore.SaveFromURL(ctx, url)
		if derr != nil {
			return llm.MediaFailed, "成品下载失败: " + derr.Error(), nil
		}
		return llm.MediaSucceeded, local, nil
	case "failed", "fail":
		reason, _ := out["failCode"].(string)
		if reason == "" {
			reason = statusText(out)
		}
		return llm.MediaFailed, "厂商任务失败: " + reason, nil
	case "inqueue", "in_queue":
		return llm.MediaQueued, "", nil
	default: // inprogress / 其它
		return llm.MediaRunning, "", nil
	}
}

// firstVideoURL SiliconFlow 响应里的视频地址（results.videos[].url 或顶层 url）
func firstVideoURL(out map[string]any) string {
	if results, ok := out["results"].(map[string]any); ok {
		if videos, ok := results["videos"].([]any); ok && len(videos) > 0 {
			if v, ok := videos[0].(map[string]any); ok {
				if u, ok := v["url"].(string); ok {
					return u
				}
			}
		}
		if u, ok := results["video_url"].(string); ok {
			return u
		}
	}
	if u, ok := out["url"].(string); ok {
		return u
	}
	return ""
}

// statusText 从响应里尽量挖一句人话错误
func statusText(out map[string]any) string {
	if m, ok := out["message"].(string); ok && m != "" {
		return m
	}
	raw, _ := json.Marshal(out)
	return truncateForErr(raw, 200)
}
