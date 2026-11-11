package repo

// Agent 快照与委派上下文的仓储测试（真实 SQL）：
// 快照只增不改、按最新一条取、任务列 NULL / '[]' 语义不混淆。

import (
	"context"
	"testing"
	"time"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/data/db/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newSnapshotDB(t *testing.T) *AgentFactoryRepo {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&models.AgentSnapshotPO{}, &models.AgentTaskPO{}, &models.AgentTaskHistoryPO{}); err != nil {
		t.Fatal(err)
	}
	return &AgentFactoryRepo{db: gdb}
}

func TestSnapshotRepoRoundtrip(t *testing.T) {
	r := newSnapshotDB(t)
	ctx := context.Background()

	// 没有快照：Latest 返回 (nil, nil)
	got, err := r.LatestSnapshot(ctx, 1)
	if err != nil || got != nil {
		t.Fatalf("空库应返回 (nil,nil)，got %+v err %v", got, err)
	}

	s1 := &agentfactory.Snapshot{
		AgentDefID: 1, AgentKey: "writer", Version: 1, Checksum: "sum1",
		Config: `{"agent_key":"writer","system":"v1"}`, Deps: `{"tools":["a"]}`,
		CreatedAt: time.Now(),
	}
	if err := r.CreateSnapshot(ctx, s1); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if s1.ID == 0 {
		t.Fatal("应回填自增 ID")
	}
	s2 := &agentfactory.Snapshot{
		AgentDefID: 1, AgentKey: "writer", Version: 2, Checksum: "sum2",
		Config: `{"agent_key":"writer","system":"v2"}`, CreatedAt: time.Now(),
	}
	if err := r.CreateSnapshot(ctx, s2); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	latest, err := r.LatestSnapshot(ctx, 1)
	if err != nil || latest == nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if latest.ID != s2.ID || latest.Version != 2 {
		t.Fatalf("最新一条应是 v2: %+v", latest)
	}
	list, err := r.ListSnapshots(ctx, 1, 10)
	if err != nil || len(list) != 2 || list[0].ID != s2.ID {
		t.Fatalf("列表应按新→旧: %+v err %v", list, err)
	}

	// 反解冻结配置
	def, err := latest.AgentDef()
	if err != nil {
		t.Fatalf("AgentDef: %v", err)
	}
	if def.System != "v2" {
		t.Fatalf("配置内容不符: %+v", def)
	}

	// 其它定义的快照互不干扰
	if other, _ := r.LatestSnapshot(ctx, 2); other != nil {
		t.Fatal("不应串到别的定义")
	}
}

// 委派上下文列：NULL（未受限）与 '[]'（受限且全禁）必须能区分。
func TestTaskDelegatedCapsSemantics(t *testing.T) {
	r := newSnapshotDB(t)
	ctx := context.Background()

	unrestricted := &agentfactory.Task{
		AgentDefID: 1, AgentKey: "a", Title: "不限", Input: "x",
		Status: agentfactory.TaskPending, UserID: "u1", TenantID: "acme",
		SnapshotID: 9,
	}
	if err := r.CreateTask(ctx, unrestricted); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, err := r.GetTask(ctx, unrestricted.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.DelegatedCaps != nil {
		t.Fatalf("未受限应为 nil，got %#v", got.DelegatedCaps)
	}
	if got.SnapshotID != 9 {
		t.Fatalf("snapshot_id 未持久化: %+v", got)
	}

	restricted := &agentfactory.Task{
		AgentDefID: 1, AgentKey: "a", Title: "全禁", Input: "x",
		Status: agentfactory.TaskPending, UserID: "u1", TenantID: "acme",
		DelegatedCaps: []string{},
	}
	if err := r.CreateTask(ctx, restricted); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, _ = r.GetTask(ctx, restricted.ID)
	if got.DelegatedCaps == nil {
		t.Fatal("'[]' 应解码为受限空集，不能退化成 nil")
	}
	if len(got.DelegatedCaps) != 0 {
		t.Fatalf("应为空集，got %v", got.DelegatedCaps)
	}

	partial := &agentfactory.Task{
		AgentDefID: 1, AgentKey: "a", Title: "部分", Input: "x",
		Status: agentfactory.TaskPending, UserID: "u1", TenantID: "acme",
		DelegatedCaps: []string{"mcp:a", "agent:b"},
	}
	if err := r.CreateTask(ctx, partial); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, _ = r.GetTask(ctx, partial.ID)
	if len(got.DelegatedCaps) != 2 || got.DelegatedCaps[0] != "mcp:a" {
		t.Fatalf("能力清单不符: %v", got.DelegatedCaps)
	}
}
