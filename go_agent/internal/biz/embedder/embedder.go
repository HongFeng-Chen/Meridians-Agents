// Package embedder 封装 OpenAI 兼容的 Embedding 接口调用。
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go_agent/internal/conf"
	"io"
	"net/http"
	"regexp"
	"time"
)

// Client Embedding 客户端
type EmbeddingClient struct {
	baseURL string
	apiKey  string
	model   string
	dim     int
	http    *http.Client
}

// NewEmbedding 创建 Embedding 客户端
func NewEmbedding(cfg *conf.Data) *EmbeddingClient {
	timeout := 60 * time.Second
	if t := cfg.GetEmbedding().GetHttpTimeout(); t != nil && t.AsDuration() > 0 {
		timeout = t.AsDuration()
	}
	return &EmbeddingClient{
		baseURL: cfg.GetEmbedding().GetBaseURL(),
		apiKey:  cfg.GetEmbedding().GetApiKey(),
		model:   cfg.GetEmbedding().GetModel(),
		dim:     int(cfg.GetEmbedding().GetDimension()),
		http:    &http.Client{Timeout: timeout},
	}
}

// Dim 返回向量维度
func (c *EmbeddingClient) Dim() int { return c.dim }

// Model 返回模型名
func (c *EmbeddingClient) Model() string { return c.model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed 批量生成文本向量，返回顺序与输入一致。
// 瞬时故障（连接拒绝 / 429 / 5xx / 上游 400 带连接类错误）自动退避重试，
// 图谱抽取一次要向量化上百条文本，不应因后端重启窗口整批失败。
func (c *EmbeddingClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(embedRequest{Model: c.model, Input: texts})

	const maxAttempts = 3
	var (
		raw    []byte
		status int
		err    error
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, status, err = c.attempt(ctx, body)
		if err == nil {
			break
		}
		if attempt == maxAttempts || !transientEmbedFailure(err, status, raw) {
			break
		}
		// 线性退避：300ms / 600ms，避开后端重启窗口
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, err
	}

	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(vectors) {
			continue
		}
		if len(d.Embedding) != c.dim {
			return nil, fmt.Errorf("embedding dim mismatch: got %d, want %d (check EMBED_DIM)",
				len(d.Embedding), c.dim)
		}
		vectors[d.Index] = d.Embedding
	}
	for i, v := range vectors {
		if v == nil {
			return nil, fmt.Errorf("embedding %d missing", i)
		}
	}
	return vectors, nil
}

// attempt 发一次请求，返回原始响应体、状态码与（非 200 时的）错误。
func (c *EmbeddingClient) attempt(ctx context.Context, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) &&
			c.apiKey == "" {
			return raw, resp.StatusCode, fmt.Errorf("embed api %d: 未配置 EMBED_API_KEY（若与 LLM 同一服务商，可直接复用 LLM_API_KEY）: %s",
				resp.StatusCode, string(raw))
		}
		return raw, resp.StatusCode, fmt.Errorf("embed api %d: %s", resp.StatusCode, string(raw))
	}
	return raw, resp.StatusCode, nil
}

// transientBodyRe 上游 400/502 的 body 里出现连接类错误时同样视为瞬时故障。
// 本次线上案例：llama-server 内部调 /tokenize 失败，回 400 带 "connectex ... refused"。
var transientBodyRe = regexp.MustCompile(
	`(?i)connection refused|connectex|no connection could be made|dial tcp|server is loading|temporarily unavailable`)

// transientEmbedFailure 判断是否值得重试。
func transientEmbedFailure(err error, status int, body []byte) bool {
	// 请求方主动取消 / 截止：不重试
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return false
	}
	if status == 0 {
		return true // 网络层失败（连接拒绝 / 重置 / 超时）
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return len(body) > 0 && transientBodyRe.Match(body)
}

// EmbedOne 生成单条文本向量
func (c *EmbeddingClient) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
