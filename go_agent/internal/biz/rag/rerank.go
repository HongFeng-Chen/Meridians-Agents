package rag

// rerank.go 召回后精排（两段式检索的 rerank 段）。
//
// 向量/BM25 混合召回（快、召回广）之后，用交叉编码器模型对 query-doc 对逐一精排
// （准、但贵），只对 topK 条做精排再取 topN 喂回生成——喂回内容更准且更短，
// token 与回答质量同时受益。
//
// 支持两种协议（conf.Data.Rerank.provider 选择）：
//   - cohere    POST {base}/rerank        —— Jina / SiliconFlow / Cohere 兼容
//   - dashscope POST {base}/api/v1/services/rerank/rerank —— 百炼原生（gte-rerank）
//
// 精排是可选增强：任何失败（未启用 / 网络错误 / 响应异常）都静默降级为检索原序，
// 绝不让精排故障影响召回主链路。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go_agent/internal/conf"
)

// Reranker 重排序客户端
type Reranker struct {
	provider string // cohere | dashscope
	baseURL  string
	apiKey   string
	model    string
	topN     int
	http     *http.Client
}

// NewReranker 从 conf 构建精排客户端；未启用返回 nil。
func NewReranker(c *conf.Data) *Reranker {
	rc := c.GetRerank()
	if rc == nil || !rc.GetEnabled() || rc.GetBaseURL() == "" || rc.GetModel() == "" {
		return nil
	}
	timeout := 10 * time.Second
	if rc.GetHttpTimeout() != nil && rc.GetHttpTimeout().AsDuration() > 0 {
		timeout = rc.GetHttpTimeout().AsDuration()
	}
	provider := rc.GetProvider()
	if provider != "dashscope" {
		provider = "cohere"
	}
	return &Reranker{
		provider: provider,
		baseURL:  trimSuffixSlash(rc.GetBaseURL()),
		apiKey:   rc.GetApiKey(),
		model:    rc.GetModel(),
		topN:     int(rc.GetTopN()),
		http:     &http.Client{Timeout: timeout},
	}
}

func trimSuffixSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Rerank 精排：返回按相关度降序的文档下标（长度 = min(len(docs), topN)，topN<=0 取 len(docs)）。
func (r *Reranker) Rerank(ctx context.Context, query string, docs []string) ([]int, error) {
	if r == nil || len(docs) == 0 {
		return nil, fmt.Errorf("rerank 未启用")
	}
	if r.topN > 0 && len(docs) > r.topN {
		docs = docs[:r.topN]
	}
	var (
		url  string
		body []byte
		err  error
	)
	if r.provider == "dashscope" {
		url = r.baseURL + "/api/v1/services/rerank/rerank"
		body, err = json.Marshal(map[string]any{
			"model": r.model,
			"input": map[string]any{"query": query, "documents": docs},
			"parameters": map[string]any{
				"return_documents": false,
				"top_n":            len(docs),
			},
		})
	} else {
		url = r.baseURL + "/rerank"
		body, err = json.Marshal(map[string]any{
			"model":            r.model,
			"query":            query,
			"documents":        docs,
			"top_n":            len(docs),
			"return_documents": false,
		})
	}
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank api %d: %s", resp.StatusCode, truncateBytes(raw, 200))
	}

	// 两种协议的 results 均为 [{index, relevance_score}]，dashscope 多一层 output 包裹
	type item struct {
		Index int     `json:"index"`
		Score float64 `json:"relevance_score"`
	}
	var cr struct {
		Results []item `json:"results"`
		Output  struct {
			Results []item `json:"results"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, err
	}
	results := cr.Results
	if len(results) == 0 {
		results = cr.Output.Results
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("rerank 响应无 results")
	}
	out := make([]int, 0, len(results))
	for _, it := range results {
		if it.Index >= 0 && it.Index < len(docs) {
			out = append(out, it.Index)
		}
	}
	return out, nil
}

func truncateBytes(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
