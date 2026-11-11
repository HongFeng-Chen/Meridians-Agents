package repo

// 幂等记录仓储测试：真实 SQL 的唯一索引 upsert 与回放读取。

import (
	"context"
	"testing"
	"time"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/data/db/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newMcpIdemDB(t *testing.T) *MCPRepo {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&models.McpCallRecordPO{}); err != nil {
		t.Fatal(err)
	}
	return &MCPRepo{db: gdb}
}

func TestCallRecordRoundtrip(t *testing.T) {
	r := newMcpIdemDB(t)
	ctx := context.Background()
	key := "task:5:turn:2:default__create_issue:abcd1234"

	// 未写入前：不存在（nil, nil）
	got, err := r.GetCallRecord(ctx, key)
	if err != nil || got != nil {
		t.Fatalf("未写入应返回 (nil,nil), got %+v err %v", got, err)
	}

	res := &mcp.CallResult{
		Content:    []mcp.ContentPart{{Type: "text", Text: "工单 #101 已创建"}},
		Structured: `{"id":101}`,
		ElapsedMs:  88,
	}
	rec := &mcp.IdemRecord{IdemKey: key, TaskID: 5, Tool: "default__create_issue", ArgsHash: "abcd1234", OK: true, Result: res, CreatedAt: time.Now()}
	if err := r.SaveCallRecord(ctx, rec); err != nil {
		t.Fatalf("SaveCallRecord: %v", err)
	}

	got, err = r.GetCallRecord(ctx, key)
	if err != nil || got == nil {
		t.Fatalf("应能读回记录, err %v", err)
	}
	if got.TaskID != 5 || got.Tool != "default__create_issue" || got.ArgsHash != "abcd1234" || !got.OK {
		t.Fatalf("记录字段不符: %+v", got)
	}
	if got.Result == nil || got.Result.Structured != `{"id":101}` || len(got.Result.Content) != 1 {
		t.Fatalf("回放结果不符: %+v", got.Result)
	}
	if got.Result.ElapsedMs != 88 {
		t.Fatalf("耗时列丢失: %+v", got.Result)
	}

	// 并发/重复写同一 key：唯一索引兜底，覆盖不报错
	res2 := &mcp.CallResult{Structured: `{"id":202}`, ElapsedMs: 9}
	if err := r.SaveCallRecord(ctx, &mcp.IdemRecord{IdemKey: key, TaskID: 5, Tool: "default__create_issue", ArgsHash: "abcd1234", OK: true, Result: res2}); err != nil {
		t.Fatalf("重复写同一 key 不应报错: %v", err)
	}
	got, _ = r.GetCallRecord(ctx, key)
	if got.Result == nil || got.Result.Structured != `{"id":202}` {
		t.Fatalf("后写应覆盖先写: %+v", got.Result)
	}
}
