// openai.go OpenAI 视频协议：/videos 提交与轮询（Sora 系列）。
// 成功后成品为二进制，需再请求 GET /videos/{id}/content 落盘。
package video_generations

import (
	"context"
	"fmt"
	"net/http"

	"go_agent/internal/biz/llm"
	"go_agent/internal/pkg/mediastore"
)

func (c *VideoClient) submitOpenAI(ctx context.Context, base, apiKey, model string, in *llm.VideoGenInput) (string, error) {
	payload := map[string]any{"model": model, "prompt": in.Prompt}
	if in.Size != "" {
		payload["size"] = in.Size
	}
	if in.DurationSec > 0 {
		payload["seconds"] = fmt.Sprintf("%d", in.DurationSec)
	}
	out, err := c.postJSON(ctx, base+"/videos", apiKey, payload)
	if err != nil {
		return "", err
	}
	if id, ok := out["id"].(string); ok && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("提交响应里没有任务 ID: %v", out)
}

func (c *VideoClient) pollOpenAI(ctx context.Context, base, apiKey, model, taskID string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/videos/"+taskID, nil)
	if err != nil {
		return "", "", err
	}
	setKey(req, apiKey)
	out, err := c.doJSON(req)
	if err != nil {
		return "", "", err
	}
	status, _ := out["status"].(string)
	switch status {
	case "completed":
		// 成品二进制：GET /videos/{id}/content
		req2, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/videos/"+taskID+"/content", nil)
		if err != nil {
			return "", "", err
		}
		setKey(req2, apiKey)
		resp, err := c.http.Do(req2)
		if err != nil {
			return llm.MediaFailed, "成品下载失败: " + err.Error(), nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return llm.MediaFailed, fmt.Sprintf("成品下载 HTTP %d", resp.StatusCode), nil
		}
		local, derr := mediastore.SaveBytes(".mp4", resp.Body)
		if derr != nil {
			return llm.MediaFailed, "成品保存失败: " + derr.Error(), nil
		}
		return llm.MediaSucceeded, local, nil
	case "failed":
		// 错误细节在 error.message
		reason := status
		if e, ok := out["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok && m != "" {
				reason = m
			}
		}
		return llm.MediaFailed, "厂商任务失败: " + reason, nil
	case "queued":
		return llm.MediaQueued, "", nil
	default: // in_progress
		return llm.MediaRunning, "", nil
	}
}
