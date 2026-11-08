package server

// 运维面路由的接线用例。
//
// 重点验证一件事：srv.Route 注册的 handler **不会**自动经过 http.Middleware，
// 我们要手动套 ctx.Middleware，否则 /api/mcp/tool-approvals 会变成匿名入口
// （身份为空 → 所有审批都被拒；更糟的是绕过限流）。
//
// 这里用真实的 Kratos server + 真实中间件跑集成断言，而不是只测 service 方法。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go_agent/internal/biz/audit"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/service"

	khttp "github.com/go-kratos/kratos/v3/transport/http"
)

func newOpsTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	// nil Security → 开发态（认证关闭），请求来自 httptest 的 127.0.0.1，
	// 因此被视为「本机管理员」，可以验证放行分支。
	srv := khttp.NewServer(khttp.Middleware(newGate(nil).middleware()))
	registerOpsRoutes(srv, service.NewMCPService(nil), audit.New(nil))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func TestListToolApprovalsRoute(t *testing.T) {
	ts := newOpsTestServer(t)

	resp, err := http.Get(ts.URL + "/api/mcp/tool-approvals?status=pending")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("期望 200，实际 %d：%s", resp.StatusCode, body)
	}
	var out struct {
		Approvals []map[string]any `json:"approvals"`
		Total     int              `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
}

// TestApproveRoute 走完整链路：申请 → 路由批准 → 状态变为 approved。
// 这条用例能同时证明「身份被中间件注入」（否则 requireAdmin 直接 403）。
func TestApproveRoute(t *testing.T) {
	ts := newOpsTestServer(t)

	tool := mcp.Tool{ID: 101, ServerID: 9, UniqueName: "srv__delete_ns", ToolName: "delete_ns"}
	a, err := mcp.RequestToolApproval(tool, map[string]any{"ns": "test"}, "alice", "public")
	if err != nil {
		t.Fatalf("申请审批失败: %v", err)
	}

	resp, err := http.Post(ts.URL+"/api/mcp/tool-approvals/"+a.ID+"/approve",
		"application/json", strings.NewReader(`{"comment":"确认"}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", resp.StatusCode, body)
	}
	var got mcp.ToolApproval
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应解析失败: %v（%s）", err, body)
	}
	if got.Status != mcp.ApprovalApproved {
		t.Fatalf("审批状态应为 approved，实际 %q（%s）", got.Status, body)
	}
	if got.DecidedBy == "" {
		t.Fatal("批准人应被记录")
	}
}

func TestApproveRouteUnknownID(t *testing.T) {
	ts := newOpsTestServer(t)
	resp, err := http.Post(ts.URL+"/api/mcp/tool-approvals/ap-not-exist/approve",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("不存在的审批单不应返回 200")
	}
}

func TestAuditRoute(t *testing.T) {
	ts := newOpsTestServer(t)
	resp, err := http.Get(ts.URL + "/api/admin/audits?result=denied&limit=10")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("期望 200，实际 %d：%s", resp.StatusCode, body)
	}
}
