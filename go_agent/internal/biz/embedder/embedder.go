// Package embedder 封装 OpenAI 兼容的 Embedding 接口调用。
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go_agent/internal/conf"
	"io"
	"net/http"
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

// Embed 批量生成文本向量，返回顺序与输入一致
func (c *EmbeddingClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(embedRequest{Model: c.model, Input: texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) &&
			c.apiKey == "" {
			return nil, fmt.Errorf("embed api %d: 未配置 EMBED_API_KEY（若与 LLM 同一服务商，可直接复用 LLM_API_KEY）: %s",
				resp.StatusCode, string(raw))
		}
		return nil, fmt.Errorf("embed api %d: %s", resp.StatusCode, string(raw))
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

// EmbedOne 生成单条文本向量
func (c *EmbeddingClient) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
