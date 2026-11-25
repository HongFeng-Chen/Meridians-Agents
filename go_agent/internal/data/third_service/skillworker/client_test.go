package skillworker

// Worker 客户端协议测试：路径、信封字段、鉴权头、错误与「直接返回业务 JSON」的兼容。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go_agent/internal/biz/skill"
	"go_agent/internal/pkg/egress"
)

func allowLocal(t *testing.T) {
	t.Helper()
	prev := egress.CurrentPolicy()
	egress.SetPolicy(egress.Policy{AllowPrivate: true, MaxRedirects: 5})
	t.Cleanup(func() { egress.SetPolicy(prev) })
}

func TestExecuteSendsEnvelope(t *testing.T) {
	allowLocal(t)
	t.Setenv("SKILL_WORKER_TOKEN", "s3cret")

	var gotReq skill.WorkerRequest
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(skill.WorkerResponse{
			Success: true,
			Output:  map[string]any{"pages": 12},
		})
	}))
	defer srv.Close()

	c := New(nil)
	res, err := c.Execute(context.Background(), skill.Instance{ID: "w1", Endpoint: srv.URL}, skill.WorkerRequest{
		TaskID: "42", ExecutionID: "exec_1", SkillName: "pdf",
		TenantID: "t1", IdempotencyKey: "k1", Input: map[string]any{"file_id": "f1"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotPath != "/execute" {
		t.Fatalf("请求路径 = %s, want /execute", gotPath)
	}
	if gotAuth != "Bearer s3cret" {
		t.Fatalf("鉴权头 = %q", gotAuth)
	}
	if gotReq.TaskID != "42" || gotReq.TenantID != "t1" || gotReq.IdempotencyKey != "k1" {
		t.Fatalf("信封字段丢失: %+v", gotReq)
	}
	if gotReq.Input["file_id"] != "f1" {
		t.Fatalf("input 丢失: %+v", gotReq.Input)
	}
	if !res.Success || res.Output["pages"] != float64(12) {
		t.Fatalf("回包解析异常: %+v", res)
	}
	// Worker 没回 execution_id 时用请求里的补上，调用方不用自己拼
	if res.ExecutionID != "exec_1" || res.TaskID != "42" {
		t.Fatalf("回包未补齐标识: %+v", res)
	}
}

func TestExecuteHTTPError(t *testing.T) {
	allowLocal(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer srv.Close()

	c := New(nil)
	_, err := c.Execute(context.Background(), skill.Instance{ID: "w1", Endpoint: srv.URL}, skill.WorkerRequest{SkillName: "pdf"})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("应带状态码报错: %v", err)
	}
}

func TestExecutePlainJSONFallback(t *testing.T) {
	allowLocal(t)
	// Worker 直接返回业务 JSON（不实现信封）：客户端包一层成功结果，
	// 降低 Python 侧接入成本——先能跑通，再补 skill_version/幂等字段。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"summary":"ok"}`))
	}))
	defer srv.Close()

	c := New(nil)
	res, err := c.Execute(context.Background(), skill.Instance{ID: "w1", Endpoint: srv.URL}, skill.WorkerRequest{SkillName: "pdf"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success || res.Output["summary"] != "ok" {
		t.Fatalf("裸 JSON 未兼容: %+v", res)
	}
}

func TestExecuteRejectsEndpoint(t *testing.T) {
	// 默认策略下回环地址必须被拦（出站闸门对 Worker 地址同样生效）
	prev := egress.CurrentPolicy()
	egress.SetPolicy(egress.Policy{MaxRedirects: 5})
	t.Cleanup(func() { egress.SetPolicy(prev) })

	c := New(nil)
	_, err := c.Execute(context.Background(), skill.Instance{ID: "w1", Endpoint: "http://127.0.0.1:9999"}, skill.WorkerRequest{})
	if err == nil || !strings.Contains(err.Error(), "出站") {
		t.Fatalf("应拒绝回环地址: %v", err)
	}
}
