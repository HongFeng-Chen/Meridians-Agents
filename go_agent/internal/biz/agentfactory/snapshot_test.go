package agentfactory

// P3 快照测试：checksum 稳定性、内容去重、依赖抽取、按快照执行。

import (
	"context"
	"testing"
	"time"
)

func TestConfigChecksumIgnoresRuntimeFields(t *testing.T) {
	base := &AgentDef{
		ID: 7, AgentKey: "writer", Name: "写作", System: "你是编辑",
		Capabilities: []string{"mcp:a"}, Version: 3, Enabled: true,
		UpdatedAt: time.Now(), NextRunAt: time.Now().Add(time.Hour),
	}
	if ConfigChecksum(base) == "" {
		t.Fatal("checksum 不应为空")
	}

	// 运行态字段变化不影响 checksum（否则启停 / 调度会把快照刷爆）
	runtimeChanged := *base
	runtimeChanged.Enabled = false
	runtimeChanged.Version = 9
	runtimeChanged.Vectorized = true
	runtimeChanged.UpdatedAt = time.Now().Add(time.Minute)
	runtimeChanged.LastRunAt = time.Now()
	if ConfigChecksum(&runtimeChanged) != ConfigChecksum(base) {
		t.Fatal("运行态字段不应改变 checksum")
	}

	// 配置内容变化必须改变 checksum
	contentChanged := *base
	contentChanged.System = "你是另一个角色"
	if ConfigChecksum(&contentChanged) == ConfigChecksum(base) {
		t.Fatal("提示词变化必须改变 checksum")
	}
	capsChanged := *base
	capsChanged.Capabilities = []string{"mcp:a", "mcp:b"}
	if ConfigChecksum(&capsChanged) == ConfigChecksum(base) {
		t.Fatal("能力变化必须改变 checksum")
	}
}

func TestExtractDeps(t *testing.T) {
	def := &AgentDef{
		Members:       []string{"research", "writer"},
		SupervisorKey: "supervisor",
		RiskAgentKey:  "risk",
		Capabilities: []string{
			"mcp:mail_send", "mcp:kb__search", "skill:translate",
			"agent:research", "create_agent",
		},
	}
	deps := ExtractDeps(def)
	// 成员（research / writer）+ 监察 + 风控，去重后按字典序
	want := []string{"research", "risk", "supervisor", "writer"}
	if len(deps.Agents) != len(want) {
		t.Fatalf("agents 依赖不符: %v", deps.Agents)
	}
	for i := range want {
		if deps.Agents[i] != want[i] {
			t.Fatalf("agents 依赖不符: %v", deps.Agents)
		}
	}
	if len(deps.Tools) != 2 || deps.Tools[0] != "kb__search" || deps.Tools[1] != "mail_send" {
		t.Fatalf("tools 依赖不符: %v", deps.Tools)
	}
	if len(deps.Skills) != 1 || deps.Skills[0] != "translate" {
		t.Fatalf("skills 依赖不符: %v", deps.Skills)
	}
}

func TestBuildSnapshotRoundtrip(t *testing.T) {
	def := &AgentDef{
		ID: 3, AgentKey: "writer", Name: "写作", System: "旧提示词",
		GraphType: GraphReact, Capabilities: []string{"mcp:a"}, Version: 2, Enabled: true,
	}
	snap, err := BuildSnapshot(def)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if snap.AgentDefID != 3 || snap.Version != 2 || snap.Checksum == "" {
		t.Fatalf("快照字段不符: %+v", snap)
	}
	frozen, err := snap.AgentDef()
	if err != nil {
		t.Fatalf("AgentDef: %v", err)
	}
	if frozen.System != "旧提示词" || frozen.GraphType != GraphReact || len(frozen.Capabilities) != 1 {
		t.Fatalf("冻结配置不符: %+v", frozen)
	}
	// 未持久化的定义不能生成快照（ID=0）
	if _, err := BuildSnapshot(&AgentDef{AgentKey: "x"}); err == nil {
		t.Fatal("ID=0 应拒绝")
	}
}

// 内容未变 → 复用最新快照；内容变化 → 追加新快照。
func TestPublishSnapshotDedup(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	def, err := u.CreateDef(ctx, AgentInput{Name: "写作", AgentKey: "writer", System: "v1", Capabilities: nil})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	first, err := u.PublishSnapshot(ctx, def.ID)
	if err != nil {
		t.Fatalf("PublishSnapshot: %v", err)
	}
	again, err := u.PublishSnapshot(ctx, def.ID)
	if err != nil {
		t.Fatalf("PublishSnapshot: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("内容未变应复用快照: %d != %d", again.ID, first.ID)
	}

	in := AgentInput{Name: "写作", AgentKey: "writer", System: "v2"}
	if _, err := u.UpdateDef(ctx, def.ID, in); err != nil {
		t.Fatalf("UpdateDef: %v", err)
	}
	latest, err := u.repo.LatestSnapshot(ctx, def.ID)
	if err != nil || latest == nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if latest.ID == first.ID {
		t.Fatal("内容变化应产生新快照")
	}
	list, _ := u.Snapshots(ctx, def.ID, 10)
	if len(list) < 2 {
		t.Fatalf("应有至少两条快照，实际 %d", len(list))
	}
}

// 已提交任务按钉住的快照执行：定义改成 v2 后，旧任务仍用 v1 的配置。
func TestDefForTaskUsesSnapshot(t *testing.T) {
	u, repo, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	def, err := u.CreateDef(ctx, AgentInput{Name: "写作", AgentKey: "writer", System: "旧提示词"})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	snap, err := u.PublishSnapshot(ctx, def.ID)
	if err != nil {
		t.Fatalf("PublishSnapshot: %v", err)
	}
	task := &Task{ID: 1, AgentKey: "writer", SnapshotID: snap.ID}

	live, _ := repo.GetDef(ctx, def.ID)
	live.System = "新提示词（已改）"

	frozen, gotSnap := u.defForTask(ctx, task, live)
	if gotSnap == nil || gotSnap.ID != snap.ID {
		t.Fatalf("应命中快照: %+v", gotSnap)
	}
	if frozen.System != "旧提示词" {
		t.Fatalf("执行配置应来自快照，实际 %q", frozen.System)
	}
	if frozen.ID != live.ID || !frozen.Enabled {
		t.Fatal("主键与启用状态应取当前定义")
	}

	// 没有快照的任务：回退当前定义
	plain, _ := u.defForTask(ctx, &Task{ID: 2}, live)
	if plain.System != "新提示词（已改）" {
		t.Fatalf("无快照应回退当前定义，实际 %q", plain.System)
	}
}

// 快照缺失（例如历史任务、快照被清理）时不阻断执行：回退当前定义。
func TestDefForTaskMissingSnapshotFallsBack(t *testing.T) {
	u, _, _ := newTestUseCase(t, &fakeChatter{})
	ctx := context.Background()
	def, err := u.CreateDef(ctx, AgentInput{Name: "写作", AgentKey: "writer", System: "现行"})
	if err != nil {
		t.Fatalf("CreateDef: %v", err)
	}
	got, snap := u.defForTask(ctx, &Task{ID: 1, SnapshotID: 9999}, def)
	if snap != nil {
		t.Fatal("不存在的快照不应返回引用")
	}
	if got.System != "现行" {
		t.Fatalf("应回退当前定义，实际 %q", got.System)
	}
}
