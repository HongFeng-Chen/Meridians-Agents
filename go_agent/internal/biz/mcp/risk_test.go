package mcp

// 风险分级与审批流的回归用例。
//
// 这三组断言对应三个「必须成立」的安全属性：
//  1. 高危工具不会被误判成只读（分级）；
//  2. 未审批的调用不能执行（闸门）；
//  3. 审批令牌不能复用、不能换参数、不能自己批自己（令牌约束）。

import (
	"errors"
	"testing"
	"time"
)

func TestClassifyToolRisk(t *testing.T) {
	cases := []struct {
		name string
		tool Tool
		want RiskLevel
	}{
		{"proxy GET 只读", Tool{ToolName: "get_user", Route: `{"method":"GET"}`}, RiskRead},
		{"proxy DELETE 高危", Tool{ToolName: "user", Route: `{"method":"DELETE"}`}, RiskDestructive},
		{"proxy PUT 写", Tool{ToolName: "user", Route: `{"method":"PUT"}`}, RiskWrite},
		{"proxy POST 写", Tool{ToolName: "user", Route: `{"method":"POST"}`}, RiskWrite},
		{"proxy GET 但名字是删除", Tool{ToolName: "delete_user", Route: `{"method":"GET"}`}, RiskDestructive},
		{"显式标签优先", Tool{ToolName: "wipe_everything", Tags: []string{"readonly"}}, RiskRead},
		{"显式高危标签", Tool{ToolName: "list_users", Tags: []string{"destructive"}}, RiskDestructive},
		{"名称高危", Tool{ToolName: "delete_repository"}, RiskDestructive},
		{"名称高危（中文）", Tool{ToolName: "清空缓存"}, RiskDestructive},
		{"执行类高危", Tool{ToolName: "execute_sql"}, RiskDestructive},
		{"写操作", Tool{ToolName: "create_issue"}, RiskWrite},
		{"只读", Tool{ToolName: "list_pods"}, RiskRead},
		{"未知默认写", Tool{ToolName: "foo"}, RiskWrite},
	}
	for _, c := range cases {
		if got := ClassifyToolRisk(c.tool); got != c.want {
			t.Errorf("%s: 分类 = %s，期望 %s", c.name, got, c.want)
		}
	}
}

func TestRiskLevelRank(t *testing.T) {
	if !(RiskRead.Rank() < RiskWrite.Rank() && RiskWrite.Rank() < RiskDestructive.Rank()) {
		t.Fatal("风险等级排序错误")
	}
	if _, ok := ParseRiskLevel("nonsense"); ok {
		t.Fatal("未知字符串不应被解析成合法等级")
	}
}

func TestApprovalRequiredDefaultPolicy(t *testing.T) {
	setApprovalPolicy(approvalPolicy{Threshold: RiskDestructive, TTL: time.Minute})
	if ApprovalRequired(Tool{ToolName: "list_pods"}) {
		t.Error("只读工具不应要求审批")
	}
	if ApprovalRequired(Tool{ToolName: "create_issue"}) {
		t.Error("默认阈值是 destructive，写操作不应要求审批")
	}
	if !ApprovalRequired(Tool{ToolName: "delete_repository"}) {
		t.Error("高危工具必须要求审批")
	}

	// 阈值调到 write：写操作也要审批
	setApprovalPolicy(approvalPolicy{Threshold: RiskWrite, TTL: time.Minute})
	if !ApprovalRequired(Tool{ToolName: "create_issue"}) {
		t.Error("阈值为 write 时，写操作应要求审批")
	}

	// 关闭审批
	setApprovalPolicy(approvalPolicy{Threshold: "", TTL: time.Minute})
	if ApprovalRequired(Tool{ToolName: "delete_repository"}) {
		t.Error("关闭审批后不应要求审批")
	}
}

func TestToolApprovalFlow(t *testing.T) {
	SetApprovalStore(&memoryApprovalStore{items: map[string]*ToolApproval{}})
	setApprovalPolicy(approvalPolicy{Threshold: RiskDestructive, TTL: time.Minute})
	t.Cleanup(func() {
		SetApprovalStore(&memoryApprovalStore{items: map[string]*ToolApproval{}})
	})

	tool := Tool{ID: 7, ServerID: 3, UniqueName: "srv__delete_repo", ToolName: "delete_repo"}
	args := map[string]any{"repo": "billing", "force": true}

	a, err := RequestToolApproval(tool, args, "alice", "t1")
	if err != nil {
		t.Fatalf("申请审批失败: %v", err)
	}
	if a.Status != ApprovalPending || a.Risk != RiskDestructive {
		t.Fatalf("审批单状态/风险不对: %+v", a)
	}

	// 未批准前重放 → 拒绝
	if err := ConsumeToolApproval(a.ID, "alice", "t1", tool, args); err == nil {
		t.Fatal("未批准的令牌不应可用")
	}

	// 自己批自己 → 拒绝（四眼原则）
	if _, err := DecideToolApproval(a.ID, "alice", true, "自己批"); !errors.Is(err, ErrApprovalSelfApprove) {
		t.Fatalf("自批应被拒绝，实际 %v", err)
	}

	// 管理员批准
	if _, err := DecideToolApproval(a.ID, "admin", true, "确认删除测试库"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}

	// 改了参数 → 指纹不匹配
	if err := ConsumeToolApproval(a.ID, "alice", "t1", tool, map[string]any{"repo": "prod"}); err == nil {
		t.Fatal("参数被修改后不应通过校验")
	}

	// 正常消费
	if err := ConsumeToolApproval(a.ID, "alice", "t1", tool, args); err != nil {
		t.Fatalf("合法令牌应通过: %v", err)
	}
	// 一次性：再消费失败
	if err := ConsumeToolApproval(a.ID, "alice", "t1", tool, args); err == nil {
		t.Fatal("令牌只能用一次")
	}
}

func TestToolApprovalDenied(t *testing.T) {
	SetApprovalStore(&memoryApprovalStore{items: map[string]*ToolApproval{}})
	setApprovalPolicy(approvalPolicy{Threshold: RiskDestructive, TTL: time.Minute})

	tool := Tool{UniqueName: "srv__drop_table", ToolName: "drop_table"}
	a, err := RequestToolApproval(tool, nil, "bob", "t1")
	if err != nil {
		t.Fatalf("申请审批失败: %v", err)
	}
	if _, err := DecideToolApproval(a.ID, "admin", false, "不允许"); err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	if err := ConsumeToolApproval(a.ID, "bob", "t1", tool, nil); !errors.Is(err, ErrApprovalDenied) {
		t.Fatalf("被驳回的令牌应返回 ErrApprovalDenied，实际 %v", err)
	}
}

func TestToolApprovalExpired(t *testing.T) {
	SetApprovalStore(&memoryApprovalStore{items: map[string]*ToolApproval{}})
	setApprovalPolicy(approvalPolicy{Threshold: RiskDestructive, TTL: time.Millisecond})

	tool := Tool{UniqueName: "srv__exec_shell", ToolName: "exec_shell"}
	a, err := RequestToolApproval(tool, map[string]any{"cmd": "ls"}, "carol", "t1")
	if err != nil {
		t.Fatalf("申请审批失败: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := DecideToolApproval(a.ID, "admin", true, ""); err == nil {
		t.Fatal("过期审批单不应还能被批准")
	}
}

func TestToolApprovalCrossUser(t *testing.T) {
	SetApprovalStore(&memoryApprovalStore{items: map[string]*ToolApproval{}})
	setApprovalPolicy(approvalPolicy{Threshold: RiskDestructive, TTL: time.Minute})

	tool := Tool{UniqueName: "srv__delete_ns", ToolName: "delete_ns"}
	a, _ := RequestToolApproval(tool, nil, "alice", "t1")
	if _, err := DecideToolApproval(a.ID, "admin", true, ""); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	// 别人拿到令牌也不能用：绑定的是申请人与租户
	if err := ConsumeToolApproval(a.ID, "mallory", "t1", tool, nil); err == nil {
		t.Fatal("他人的审批令牌不应可用")
	}
}
