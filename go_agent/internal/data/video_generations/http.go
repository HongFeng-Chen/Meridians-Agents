// http.go 各厂商协议共享的 HTTP 辅助：JSON 请求/响应、鉴权头、错误截断。
package video_generations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// postJSON 发送 JSON POST 并解析响应
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

// doJSON 执行请求并把响应统一解析成 map；非 200 或厂商 error 字段都返回错误
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

// setKey 设置 Bearer 鉴权头
func setKey(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

// truncateForErr 截断响应体片段用于错误信息
func truncateForErr(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
