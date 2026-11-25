package embedder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func mkBody(dim int) string {
	vec := make([]float64, dim)
	for i := range vec {
		vec[i] = 0.1
	}
	b, _ := json.Marshal(map[string]any{
		"data": []map[string]any{{"index": 0, "embedding": vec}},
	})
	return string(b)
}

// TestEmbedRetriesTransient400 上游 400 body 带连接类错误（llama-server 内部
// tokenize 连不上）→ 视为瞬时故障重试，第二次成功。
func TestEmbedRetriesTransient400(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Post \"http://127.0.0.1:49678/tokenize\": dial tcp 127.0.0.1:49678: connectex: No connection could be made because the target machine actively refused it.","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mkBody(3)))
	}))
	defer srv.Close()

	c := &EmbeddingClient{baseURL: srv.URL, apiKey: "k", model: "m", dim: 3, http: srv.Client()}
	vecs, err := c.Embed(context.Background(), []string{"实体描述"})
	if err != nil {
		t.Fatalf("瞬时 400 后重试应成功: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 3 {
		t.Fatalf("向量形状错误: %v", vecs)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应恰好调用 2 次，实际 %d", got)
	}
}

// TestEmbedRetriesNetworkError 连接层失败（服务未起）→ 重试。
func TestEmbedRetriesNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(mkBody(3)))
	}))
	url := srv.URL
	srv.Close() // 立刻关掉制造连接拒绝 —— 但重试也不会成功，这里验证的是重试次数

	c := &EmbeddingClient{baseURL: url, apiKey: "k", model: "m", dim: 3, http: &http.Client{}}
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("服务已关闭，应返回错误")
	}
}

// TestEmbedNoRetryOnAuthError 401/403 缺 key 是确定性错误，不应重试。
func TestEmbedNoRetryOnAuthError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	c := &EmbeddingClient{baseURL: srv.URL, apiKey: "k", model: "m", dim: 3, http: srv.Client()}
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("应返回 401 错误，实际: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("确定性错误不应重试，实际调用 %d 次", got)
	}
}

// TestTransientEmbedFailure 单元化判定函数本身。
func TestTransientEmbedFailure(t *testing.T) {
	if !transientEmbedFailure(nil, 503, nil) {
		t.Fatal("503 应重试")
	}
	if !transientEmbedFailure(nil, 400, []byte("connectex: No connection could be made")) {
		t.Fatal("400 带连接类错误应重试")
	}
	if transientEmbedFailure(nil, 400, []byte(`{"error":"input too long"}`)) {
		t.Fatal("普通 400 不应重试")
	}
	if transientEmbedFailure(context.Canceled, 0, nil) {
		t.Fatal("ctx 取消不应重试")
	}
	if transientEmbedFailure(nil, 401, nil) {
		t.Fatal("401 不应重试")
	}
}
