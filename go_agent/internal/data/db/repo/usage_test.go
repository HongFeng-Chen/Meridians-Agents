package repo

// usage_test.go 单任务成本聚合（P4）：成本与 trace 用同一个 task_id 串联。

import (
	"context"
	"testing"
	"time"

	"go_agent/internal/biz/usage"
	"go_agent/internal/data/db/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newUsageDB(t *testing.T) *UsageRepo {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&models.UsageRecordPO{}); err != nil {
		t.Fatal(err)
	}
	return &UsageRepo{db: gdb}
}

func TestSummaryByTaskID(t *testing.T) {
	r := newUsageDB(t)
	ctx := context.Background()
	now := time.Now()
	add := func(task, model string, in, out, micros int64) {
		if err := r.Add(ctx, &usage.Record{
			TenantID: "public", SubjectID: "alice", AgentID: "writer", TaskID: task,
			Provider: "default", Model: model, Kind: usage.KindLLM,
			InputTokens: in, OutputTokens: out, TotalTokens: in + out,
			CostMicros: micros, Currency: "CNY", PriceVersion: "in=2/out=8", CreatedAt: now,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// 任务 42：两次调用（两个模型）；任务 43：一次
	add("42", "qwen-max", 1000, 500, 6000)
	add("42", "qwen-plus", 2000, 1000, 6000)
	add("43", "qwen-max", 500, 100, 1800)

	buckets, err := r.Summary(ctx, usage.Filter{TenantID: "public", TaskID: "42"})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("单任务聚合应只有 1 行，得到 %d", len(buckets))
	}
	b := buckets[0]
	if b.InputTokens != 3000 || b.OutputTokens != 1500 || b.TotalTokens != 4500 {
		t.Fatalf("token 汇总不对: %+v", b)
	}
	if b.CostMicros != 12000 {
		t.Fatalf("成本汇总不对: %d", b.CostMicros)
	}
	if b.Calls != 2 {
		t.Fatalf("调用次数不对: %d", b.Calls)
	}

	// 按模型拆分同一任务
	byModel, err := r.Summary(ctx, usage.Filter{TenantID: "public", TaskID: "42", GroupBy: "model"})
	if err != nil {
		t.Fatalf("Summary by model: %v", err)
	}
	if len(byModel) != 2 {
		t.Fatalf("任务 42 应有两个模型，得到 %d", len(byModel))
	}

	// 不带 task 过滤：全部（两个任务三次调用）
	all, err := r.Summary(ctx, usage.Filter{TenantID: "public", GroupBy: "task"})
	if err != nil {
		t.Fatalf("Summary by task: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("按任务分组应有 2 行，得到 %d", len(all))
	}
	total := int64(0)
	for _, x := range all {
		total += x.CostMicros
	}
	if total != 13800 {
		t.Fatalf("总成本不对: %d", total)
	}
}

// 分组白名单：未知分组不能拼进 SQL（注入面）。
func TestGroupExprWhitelist(t *testing.T) {
	if _, ok := groupExpr["task"]; !ok {
		t.Fatal("group_by=task 应可用")
	}
	if _, ok := groupExpr["task_id; DROP TABLE usage_records"]; ok {
		t.Fatal("非法分组不应出现在白名单里")
	}
}
