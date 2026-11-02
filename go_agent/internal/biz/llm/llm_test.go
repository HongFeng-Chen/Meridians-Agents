package llm

import (
	"net/http"
	"strings"
	"testing"
)

// TestHintLLMStatus 错误提示必须能区分「模型不存在」与「路径不存在」这两类 404 ——
// 这正是配置 Qwen 数据源时踩过的坑（base_url 少了 /compatible-mode 且模型名大小写不符）。
func TestHintLLMStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		base   string
		want   string
	}{
		{"模型不存在", http.StatusNotFound, `{"error":{"code":"model_not_found","message":"Model not exist."}}`, "https://x/compatible-mode/v1", "模型不存在"},
		{"路径不存在", http.StatusNotFound, `404 page not found`, "https://x/api/v1", "compatible-mode/v1"},
		{"未鉴权", http.StatusUnauthorized, `{"error":{"message":"No API-key provided."}}`, "https://x/v1", "鉴权失败"},
		{"限流", http.StatusTooManyRequests, `{}`, "https://x/v1", "限流"},
		{"其他错误不瞎猜", http.StatusInternalServerError, `{}`, "https://x/v1", ""},
	}
	for _, c := range cases {
		got := hintLLMStatus(c.status, []byte(c.body), c.base)
		if c.want == "" {
			if got != "" {
				t.Fatalf("%s: 不应加提示, got %q", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Fatalf("%s: 提示应包含 %q, got %q", c.name, c.want, got)
		}
	}
	// 路径类 404 要把当前 base_url 回显出来，便于对照改正
	if got := hintLLMStatus(http.StatusNotFound, []byte("404"), "https://x/api/v1"); !strings.Contains(got, "https://x/api/v1") {
		t.Fatalf("提示应回显 base_url, got %q", got)
	}
}
