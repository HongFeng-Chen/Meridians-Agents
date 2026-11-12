// client.go 视频生成厂商客户端：三家主流异步任务协议。
//
// 状态归一：厂商的排队/生成中状态统一映射成 queued / running，
// 成功时把成品转存本地并返回 /api/files/ 地址；失败时把厂商原因放在 output 位返回。
package video_generations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/pkg/mediastore"
)

// VideoClient 视频生成客户端
type VideoClient struct {
	http *http.Client
}

// NewVideoClient 创建客户端
func NewVideoClient() *VideoClient {
	return &VideoClient{http: &http.Client{}}
}

var _ llm.VideoGenClient = (*VideoClient)(nil)

// Submit 提交任务，返回厂商任务 ID
func (c *VideoClient) Submit(ctx context.Context, t llm.MediaTarget, in *llm.VideoGenInput) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	base := strings.TrimRight(t.BaseURL, "/")

	switch protocolOf(t.Kind) {
	case protoArk:
		return c.submitArk(ctx, base, t.APIKey, t.Model, in)
	case protoOpenAI:
		return c.submitOpenAI(ctx, base, t.APIKey, t.Model, in)
	default: // siliconflow 及未识别 kind 的默认
		return c.submitSiliconFlow(ctx, base, t.APIKey, t.Model, in)
	}
}

// Poll 查询任务状态。返回 (status, output, err)：
//   - succeeded：output = 已落盘的本地地址
//   - failed：  output = 失败原因
//   - 其它：    output = 空，err = nil（仍在生成中）
func (c *VideoClient) Poll(ctx context.Context, t llm.MediaTarget, taskID string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	base := strings.TrimRight(t.BaseURL, "/")

	switch protocolOf(t.Kind) {
	case protoArk:
		return c.pollArk(ctx, base, t.APIKey, taskID)
	case protoOpenAI:
		return c.pollOpenAI(ctx, base, t.APIKey, t.Model, taskID)
	default:
		return c.pollSiliconFlow(ctx, base, t.APIKey, taskID)
	}
}

// —— 协议选择 ——

type videoProto string

const (
	protoSiliconFlow videoProto = "siliconflow"
	protoArk         videoProto = "ark"
	protoOpenAI      videoProto = "openai"
)

// protocolOf 按数据源 kind 选协议。火山引擎的 base_url 常见
// https://ark.cn-beijing.volces.com/api/v3，kind 可能为 custom；
// 用 host 兜底识别，识别不出统一走 SiliconFlow 协议（国内视频厂商的事实标准）。
func protocolOf(kind string) videoProto {
	switch kind {
	case llm.KindOpenAI:
		return protoOpenAI
	case "ark", "volcengine", "doubao":
		return protoArk
	}
	return protoSiliconFlow
}

// —— SiliconFlow 协议（/video/submit + /video/status）——

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

// —— 火山 Ark 协议（/content/generation/tasks）——

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

// —— OpenAI 协议（/videos，Sora）——

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

// —— 通用辅助 ——

func (c *VideoClient) postJSON(ctx context.Context, url, apiKey string, payload map[string]any) (map[string]any, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setKey(req, apiKey)
	return c.doJSON(req)
}

func (c *VideoClient) doJSON(req *http.Request) (map[string]any, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("视频任务请求: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("video api %d（%s %s）: %s",
			resp.StatusCode, req.Method, req.URL.Path, truncateForErr(raw, 300))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("视频任务响应解析: %w", err)
	}
	if e, ok := out["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		return nil, fmt.Errorf("厂商错误: %s", msg)
	}
	return out, nil
}

func setKey(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
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

func truncateForErr(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
