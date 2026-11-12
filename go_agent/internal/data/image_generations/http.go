// http.go 通用 HTTP 辅助：JSON 请求/响应、鉴权头、错误截断。
package image_generations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// postJSON 发 JSON 请求并解析响应（错误体带厂商原话，便于排查）
func (c *ImageClient) postJSON(ctx context.Context, url, apiKey string, payload map[string]any) (map[string]any, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return c.doAndParseMap(req)
}

// doAndParseMap 通用响应处理
func (c *ImageClient) doAndParseMap(req *http.Request) (map[string]any, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("图片生成请求: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("images api %d（POST %s）: %s",
			resp.StatusCode, req.URL.Path, truncateForErr(raw, 300))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("图片生成响应解析: %w", err)
	}
	if e, ok := out["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		return nil, fmt.Errorf("厂商错误: %s", msg)
	}
	return out, nil
}

// doAndParse 执行请求并解析出图片产物列表（multipart edits 用）
func (c *ImageClient) doAndParse(req *http.Request) ([]imgOutput, error) {
	out, err := c.doAndParseMap(req)
	if err != nil {
		return nil, err
	}
	return parseOutputs(out)
}

func truncateForErr(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
