package repo

// 执行链路仓储测试（V2-P2）：租约认领/续租、失联回收、退避重排、超时归档、死信查询、
// 执行体心跳。
//
// 用内存 SQLite 跑真实 SQL：这些逻辑的关键都在 WHERE 条件与「两表迁移」上，
// 伪实现测不出来（比如 timeout 落终态时必须进历史表，否则任务查不到）。

import (
	"context"
	"testing"
	"time"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/data/db/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newTaskDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(
		&models.AgentTaskPO{}, &models.AgentTaskHistoryPO{},
		&models.AgentTaskEventPO{}, &models.WorkerInstancePO{},
	); err != nil {
		t.Fatal(err)
	}
	return gdb
}

func newTaskUnderLease(t *testing.T, r *AgentFactoryRepo, status string) *agentfactory.Task {
	t.Helper()
	task := &agentfactory.Task{
		AgentDefID: 1, AgentKey: "chat-agent", Title: "巡检", Input: "巡检",
		Status: agentfactory.TaskPending, UserID: "u1", TenantID: "acme",
	}
	if err := r.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if status != agentfactory.TaskPending {
		row := models.AgentTaskPO{}
		if err := r.db.First(&row, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		row.Status = status
		if err := r.db.Model(&models.AgentTaskPO{}).Where("id = ?", task.ID).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		task.Status = status
	}
	return task
}

// TestClaimTaskWritesLease 认领要同时写状态、attempt、租约与截止时间
func TestClaimTaskWritesLease(t *testing.T) {
	r := &AgentFactoryRepo{db: newTaskDB(t)}
	ctx := context.Background()
	task := newTaskUnderLease(t, r, agentfactory.TaskPending)

	now := time.Now()
	lease := agentfactory.Lease{
		Owner:     "worker-a",
		ExpiresAt: now.Add(time.Minute),
		Deadline:  now.Add(30 * time.Minute),
		Attempt:   1,
	}
	ok, err := r.ClaimTask(ctx, task.ID, lease)
	if err != nil || !ok {
		t.Fatalf("首次认领应成功: ok=%v err=%v", ok, err)
	}
	got, err := r.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agentfactory.TaskRunning {
		t.Fatalf("状态应为 running, got %s", got.Status)
	}
	if got.Attempt != 1 {
		t.Fatalf("attempt 应为 1, got %d", got.Attempt)
	}
	if got.LeaseOwner != "worker-a" {
		t.Fatalf("lease_owner 未写入, got %q", got.LeaseOwner)
	}
	if got.LeaseExpiresAt.IsZero() || got.DeadlineAt.IsZero() {
		t.Fatalf("租约到期/截止时间未写入: %+v", got)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("started_at 应被补上")
	}

	// 已认领过的任务不能被第二次认领（重复投递的最后一道防线）
	if ok, _ := r.ClaimTask(ctx, task.ID, lease); ok {
		t.Fatal("running 状态不应再次认领成功")
	}
}

// TestRenewLeaseOnlyHolder 只有持有者能续租；任务不在 running 时续不上
func TestRenewLeaseOnlyHolder(t *testing.T) {
	r := &AgentFactoryRepo{db: newTaskDB(t)}
	ctx := context.Background()
	task := newTaskUnderLease(t, r, agentfactory.TaskPending)
	now := time.Now()
	if _, err := r.ClaimTask(ctx, task.ID, agentfactory.Lease{Owner: "worker-a", ExpiresAt: now.Add(time.Minute), Attempt: 1}); err != nil {
		t.Fatal(err)
	}

	if ok, _ := r.RenewLease(ctx, task.ID, "worker-b", now.Add(2*time.Minute)); ok {
		t.Fatal("非持有者不应续租成功")
	}
	if ok, err := r.RenewLease(ctx, task.ID, "worker-a", now.Add(5*time.Minute)); err != nil || !ok {
		t.Fatalf("持有者续租应成功: ok=%v err=%v", ok, err)
	}
	got, _ := r.GetTask(ctx, task.ID)
	if got.LeaseExpiresAt.Before(now.Add(4 * time.Minute)) {
		t.Fatalf("续租未生效, got %v", got.LeaseExpiresAt)
	}

	// 落终态后不能再续：租约随状态迁移一起清空
	done := *got
	done.Status = agentfactory.TaskSucceeded
	done.Progress = 100
	done.FinishedAt = now
	if ok, err := r.TransitionTask(ctx, task.ID, []string{agentfactory.TaskRunning}, &done); err != nil || !ok {
		t.Fatalf("running → succeeded 应成功: ok=%v err=%v", ok, err)
	}
	if ok, _ := r.RenewLease(ctx, task.ID, "worker-a", now.Add(time.Minute)); ok {
		t.Fatal("任务已终态，不应续租成功")
	}
}

// TestExpiredLeaseAndPromote 失联回收 → retry_wait → 退避到期回 pending
func TestExpiredLeaseAndPromote(t *testing.T) {
	r := &AgentFactoryRepo{db: newTaskDB(t)}
	ctx := context.Background()
	task := newTaskUnderLease(t, r, agentfactory.TaskPending)
	now := time.Now()
	if _, err := r.ClaimTask(ctx, task.ID, agentfactory.Lease{
		Owner: "gone", ExpiresAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour), Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	expired, err := r.ListExpiredLeases(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != task.ID {
		t.Fatalf("应扫到 1 条过期租约, got %+v", expired)
	}

	// 转入退避等待（清扫协程的决策由 biz 做，这里只验证仓储能落库）
	lost := expired[0]
	lost.Status = agentfactory.TaskRetryWait
	lost.LeaseOwner = ""
	lost.NextAttemptAt = now.Add(time.Minute)
	if ok, err := r.TransitionTask(ctx, task.ID, []string{agentfactory.TaskRunning}, &lost); err != nil || !ok {
		t.Fatalf("running → retry_wait 应成功: ok=%v err=%v", ok, err)
	}
	if ids, _ := r.PromoteRetryWait(ctx, now, 10); len(ids) != 0 {
		t.Fatalf("退避未到期不应重排, got %v", ids)
	}
	ids, err := r.PromoteRetryWait(ctx, now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != task.ID {
		t.Fatalf("退避到期应重排, got %v", ids)
	}
	got, _ := r.GetTask(ctx, task.ID)
	if got.Status != agentfactory.TaskPending {
		t.Fatalf("重排后应为 pending, got %s", got.Status)
	}
	if !got.NextAttemptAt.IsZero() {
		t.Fatalf("重排后应清空 next_attempt_at, got %v", got.NextAttemptAt)
	}
}

// TestOverdueTaskArchivesAsTimeout 超时任务落 timeout 终态并归档进历史表
func TestOverdueTaskArchivesAsTimeout(t *testing.T) {
	r := &AgentFactoryRepo{db: newTaskDB(t)}
	ctx := context.Background()
	task := newTaskUnderLease(t, r, agentfactory.TaskPending)
	now := time.Now()
	if _, err := r.ClaimTask(ctx, task.ID, agentfactory.Lease{
		Owner: "worker-a", ExpiresAt: now.Add(time.Minute), Deadline: now.Add(-time.Second), Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	overdue, err := r.ListOverdueTasks(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(overdue) != 1 {
		t.Fatalf("应扫到 1 条超时任务, got %d", len(overdue))
	}

	hit := overdue[0]
	hit.Status = agentfactory.TaskTimeout
	hit.Progress = 100
	hit.FinishedAt = now
	if ok, err := r.TransitionTask(ctx, task.ID, []string{agentfactory.TaskRunning}, &hit); err != nil || !ok {
		t.Fatalf("running → timeout 应成功: ok=%v err=%v", ok, err)
	}
	got, err := r.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agentfactory.TaskTimeout {
		t.Fatalf("终态应为 timeout, got %s", got.Status)
	}
	if !got.Archived {
		t.Fatal("timeout 是终态，必须归档进历史表")
	}
	// 任务池里不能再留副本
	var n int64
	r.db.Model(&models.AgentTaskPO{}).Where("id = ?", task.ID).Count(&n)
	if n != 0 {
		t.Fatalf("终态任务不应留在任务池, 剩 %d 条", n)
	}
}

// TestDeadLetterListing 死信随归档迁移并可按租户检索
func TestDeadLetterListing(t *testing.T) {
	r := &AgentFactoryRepo{db: newTaskDB(t)}
	ctx := context.Background()
	task := newTaskUnderLease(t, r, agentfactory.TaskPending)
	now := time.Now()
	if _, err := r.ClaimTask(ctx, task.ID, agentfactory.Lease{Owner: "gone", ExpiresAt: now.Add(-time.Minute), Attempt: 3}); err != nil {
		t.Fatal(err)
	}
	dead := *task
	dead.Status = agentfactory.TaskFailed
	dead.Attempt = 3
	dead.DeadLetterReason = "worker_lost"
	dead.Progress = 100
	dead.FinishedAt = now
	if ok, err := r.TransitionTask(ctx, task.ID, []string{agentfactory.TaskRunning}, &dead); err != nil || !ok {
		t.Fatalf("running → failed 应成功: ok=%v err=%v", ok, err)
	}

	list, err := r.ListDeadLetters(ctx, "acme", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != task.ID {
		t.Fatalf("应查到 1 条死信, got %+v", list)
	}
	if list[0].DeadLetterReason != "worker_lost" || list[0].Attempt != 3 {
		t.Fatalf("死信字段未随归档迁移: %+v", list[0])
	}
	if other, _ := r.ListDeadLetters(ctx, "other-tenant", 10); len(other) != 0 {
		t.Fatalf("跨租户不应看到死信, got %+v", other)
	}
}

// TestWorkerRegistry 执行体注册/心跳/失联下线
func TestWorkerRegistry(t *testing.T) {
	gdb := newTaskDB(t)
	r := &WorkerRepo{db: gdb}
	ctx := context.Background()
	now := time.Now()

	w := agentfactory.Worker{
		Key: "host-1-abc", Host: "host-1", Capacity: 4, Running: 0,
		Status: agentfactory.WorkerOnline, StartedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := r.UpsertWorker(ctx, w); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	// 重复注册（同一实例重启后重新注册）不能报唯一键冲突
	if err := r.UpsertWorker(ctx, w); err != nil {
		t.Fatalf("重复注册应幂等: %v", err)
	}

	if err := r.HeartbeatWorker(ctx, w.Key, agentfactory.WorkerOnline, 3, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Running != 3 {
		t.Fatalf("心跳应刷新在执行任务数, got %+v", list)
	}

	// 心跳过期 → 下线
	if n, err := r.MarkStaleWorkers(ctx, now.Add(5*time.Minute)); err != nil || n != 1 {
		t.Fatalf("应下线 1 个失联实例: n=%d err=%v", n, err)
	}
	list, _ = r.ListWorkers(ctx)
	if list[0].Status != agentfactory.WorkerOffline {
		t.Fatalf("失联实例应标记 offline, got %s", list[0].Status)
	}
	// 已下线的不要每次清扫都再「下线」一遍，否则日志刷屏
	if n, _ := r.MarkStaleWorkers(ctx, now.Add(10*time.Minute)); n != 0 {
		t.Fatalf("已下线实例不应重复计数, got %d", n)
	}

	// draining 由 SetWorkerStatus 显式设置
	if err := r.SetWorkerStatus(ctx, w.Key, agentfactory.WorkerDraining); err != nil {
		t.Fatal(err)
	}
	list, _ = r.ListWorkers(ctx)
	if list[0].Status != agentfactory.WorkerDraining {
		t.Fatalf("状态应更新为 draining, got %s", list[0].Status)
	}
}

// TestSaveTaskKeepsRenewedLease 进度保存不能覆盖续租结果。
//
// 回归用例：执行期间 setProgress 会反复 SaveTask，而内存里的 Task 带着「认领那一刻」
// 的租约到期时间。如果 SaveTask 把 lease_expires_at 一起写回去，长任务的租约会被
// 反复重置回认领时刻 + TTL，到期就被当成失联任务抢走。
func TestSaveTaskKeepsRenewedLease(t *testing.T) {
	r := &AgentFactoryRepo{db: newTaskDB(t)}
	ctx := context.Background()
	task := newTaskUnderLease(t, r, agentfactory.TaskPending)
	now := time.Now()
	if _, err := r.ClaimTask(ctx, task.ID, agentfactory.Lease{Owner: "worker-a", ExpiresAt: now.Add(time.Minute), Attempt: 1}); err != nil {
		t.Fatal(err)
	}

	renewed := now.Add(time.Hour)
	if ok, err := r.RenewLease(ctx, task.ID, "worker-a", renewed); err != nil || !ok {
		t.Fatalf("续租应成功: ok=%v err=%v", ok, err)
	}

	// 模拟执行中推进进度：手里还是认领时的旧租约值
	stale, err := r.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale.Progress = 60
	stale.LeaseExpiresAt = now.Add(time.Minute) // 旧值
	if err := r.SaveTask(ctx, stale); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}

	got, _ := r.GetTask(ctx, task.ID)
	if got.LeaseExpiresAt.Before(renewed.Add(-time.Second)) {
		t.Fatalf("进度保存覆盖了续租结果: got %v, want >= %v", got.LeaseExpiresAt, renewed)
	}
	if got.Progress != 60 {
		t.Fatalf("进度应正常落库, got %d", got.Progress)
	}
}

// TestDelegatedCapsSurviveArchive 委派能力必须随归档迁移到历史表。
//
// 这是权限复盘的关键一列：「这个子任务当时被下放了哪些能力」只在任务行上，
// 归档时漏掉就永久丢失，且事后无从重建（快照只记 Agent 自身配置，不含下放清单）。
// 同时覆盖三态：nil（未受限）/ 空切片（受限全禁）/ 非空清单。
func TestDelegatedCapsSurviveArchive(t *testing.T) {
	r := &AgentFactoryRepo{db: newTaskDB(t)}
	ctx := context.Background()
	cases := []struct {
		name string
		caps []string
	}{
		{"受限-部分下放", []string{"mcp:read_*", "skill:summarize"}},
		{"受限-全禁", []string{}},
		{"未受限", nil},
	}
	for _, c := range cases {
		task := newTaskUnderLease(t, r, agentfactory.TaskRunning)
		task.DelegatedCaps = c.caps
		if ok, err := r.TransitionTask(ctx, task.ID, []string{agentfactory.TaskRunning}, task); err != nil || !ok {
			t.Fatalf("%s: 保存委派能力失败 ok=%v err=%v", c.name, ok, err)
		}

		done := *task
		done.Status = agentfactory.TaskCanceled
		done.Progress = 100
		done.FinishedAt = time.Now()
		if ok, err := r.TransitionTask(ctx, task.ID, []string{agentfactory.TaskRunning}, &done); err != nil || !ok {
			t.Fatalf("%s: 归档失败 ok=%v err=%v", c.name, ok, err)
		}

		got, err := r.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("%s: GetTask: %v", c.name, err)
		}
		if !got.Archived {
			t.Fatalf("%s: 终态任务应在历史表", c.name)
		}
		// nil 与空切片语义不同：前者未受限，后者「一个都不许」
		if (got.DelegatedCaps == nil) != (c.caps == nil) {
			t.Fatalf("%s: nil 语义丢失 got=%v want=%v", c.name, got.DelegatedCaps, c.caps)
		}
		if len(got.DelegatedCaps) != len(c.caps) {
			t.Fatalf("%s: 清单不一致 got=%v want=%v", c.name, got.DelegatedCaps, c.caps)
		}
		for i := range c.caps {
			if got.DelegatedCaps[i] != c.caps[i] {
				t.Fatalf("%s: 能力项不一致 got=%v want=%v", c.name, got.DelegatedCaps, c.caps)
			}
		}
	}
}
