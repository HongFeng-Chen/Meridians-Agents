package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestCounterAndLabels(t *testing.T) {
	r := NewRegistry()
	r.Describe("http_requests_total", "请求数", TypeCounter)
	r.Inc("http_requests_total", Labels{"path": "/api/x", "code": "200"})
	r.Inc("http_requests_total", Labels{"path": "/api/x", "code": "200"})
	r.Inc("http_requests_total", Labels{"code": "500", "path": "/api/x"}) // 标签顺序不同 → 同一序列

	var b strings.Builder
	r.WriteText(&b)
	out := b.String()
	if !strings.Contains(out, "# TYPE http_requests_total counter") {
		t.Fatalf("缺少 TYPE 声明:\n%s", out)
	}
	// 标签按 key 排序：code 在 path 前
	if !strings.Contains(out, `http_requests_total{code="200",path="/api/x"} 2`) {
		t.Fatalf("计数或标签排序不对:\n%s", out)
	}
	if !strings.Contains(out, `http_requests_total{code="500",path="/api/x"} 1`) {
		t.Fatalf("不同标签应独立计数:\n%s", out)
	}
}

func TestHistogram(t *testing.T) {
	r := NewRegistry()
	r.Observe("req_duration_seconds", Labels{"path": "/a"}, 0.003)
	r.Observe("req_duration_seconds", Labels{"path": "/a"}, 1.5)

	var b strings.Builder
	r.WriteText(&b)
	out := b.String()
	// 0.003 <= 0.005（第一个桶），1.5 落在 2.5 桶；累计计数
	if !strings.Contains(out, `req_duration_seconds_bucket{le="0.005",path="/a"} 1`) {
		t.Fatalf("桶计数不对:\n%s", out)
	}
	if !strings.Contains(out, `req_duration_seconds_bucket{le="2.5",path="/a"} 2`) {
		t.Fatalf("累计桶计数不对:\n%s", out)
	}
	if !strings.Contains(out, `req_duration_seconds_count{path="/a"} 2`) {
		t.Fatalf("总数不对:\n%s", out)
	}
	if !strings.Contains(out, `req_duration_seconds_sum{path="/a"} 1.503`) {
		t.Fatalf("总和不对:\n%s", out)
	}
}

func TestGaugeAndSince(t *testing.T) {
	r := NewRegistry()
	r.Set("inflight", nil, 3)
	r.AddGauge("inflight", nil, -1)
	r.ObserveSince("latency_seconds", nil, time.Now().Add(-10*time.Millisecond))

	var b strings.Builder
	r.WriteText(&b)
	out := b.String()
	if !strings.Contains(out, "inflight 2") {
		t.Fatalf("仪表值不对:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE inflight gauge") {
		t.Fatalf("缺少 gauge 类型:\n%s", out)
	}
	if !strings.Contains(out, "latency_seconds_count 1") {
		t.Fatalf("ObserveSince 未生效:\n%s", out)
	}
}
