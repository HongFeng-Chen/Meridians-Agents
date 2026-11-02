package agentfactory

import (
	"testing"
	"time"
)

// TestParseCronBasic 覆盖常用写法
func TestParseCronBasic(t *testing.T) {
	cases := []struct {
		expr string
		ok   bool
	}{
		{"0 9 * * *", true},     // 每天 9:00
		{"*/10 * * * *", true},  // 每 10 分钟
		{"0 9 * * 1-5", true},   // 工作日 9:00
		{"0 0 1 * *", true},     // 每月 1 号
		{"30 8,20 * * *", true}, // 每天 8:30 / 20:30
		{"0 9 * *", false},      // 段数不足
		{"0 25 * * *", false},   // 时超范围
		{"0 9 * * 8", false},    // 周超范围
		{"*/0 * * * *", false},  // 步长为 0
		{"0 9 * * abc", false},  // 非法字符
	}
	for _, c := range cases {
		err := ValidateCron(c.expr)
		if c.ok && err != nil {
			t.Fatalf("%q 应合法，却报错: %v", c.expr, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%q 应非法，却通过了", c.expr)
		}
	}
}

// TestCronNext 验证下次触发时间计算
func TestCronNext(t *testing.T) {
	spec, err := ParseCron("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-29 20:00 → 次日 09:00
	base := time.Date(2026, 10, 29, 20, 0, 0, 0, time.Local)
	next := spec.Next(base)
	want := time.Date(2026, 10, 30, 9, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}

	// 每 10 分钟：20:03 → 20:10
	spec2, _ := ParseCron("*/10 * * * *")
	next2 := spec2.Next(time.Date(2026, 10, 29, 20, 3, 0, 0, time.Local))
	if want2 := time.Date(2026, 10, 29, 20, 10, 0, 0, time.Local); !next2.Equal(want2) {
		t.Fatalf("next2 = %v, want %v", next2, want2)
	}
}

// TestNormalizeRoleAndSchedule 角色/编排/定时的入参校验
func TestNormalizeRoleAndSchedule(t *testing.T) {
	// 默认角色 root、编排 auto
	in := AgentInput{AgentKey: "test-agent", Name: "测试 Agent"}
	if err := in.Normalize(12); err != nil {
		t.Fatal(err)
	}
	if in.Role != RoleRoot || in.Orchestration != OrchestrationAuto {
		t.Fatalf("默认值错误: role=%s orchestration=%s", in.Role, in.Orchestration)
	}

	// 开启定时但缺 cron → 报错
	bad := AgentInput{AgentKey: "daily-report", Name: "定时任务", AutoRun: true}
	if err := bad.Normalize(12); err == nil {
		t.Fatal("缺少 cron 表达式应报错")
	}

	// 合法定时
	ok := AgentInput{AgentKey: "daily-report", Name: "定时任务", AutoRun: true, CronExpr: "0 9 * * *", CronInput: "生成日报"}
	if err := ok.Normalize(12); err != nil {
		t.Fatal(err)
	}

	// 非 root 角色忽略编排与定时
	sub := AgentInput{AgentKey: "sub-agent", Name: "子 Agent", Role: RoleSub, Members: []string{"a"}, AutoRun: true, CronExpr: "0 9 * * *"}
	if err := sub.Normalize(12); err != nil {
		t.Fatal(err)
	}
	if len(sub.Members) != 0 || sub.AutoRun {
		t.Fatalf("非 root 角色应忽略编排/定时: %+v", sub)
	}
}
