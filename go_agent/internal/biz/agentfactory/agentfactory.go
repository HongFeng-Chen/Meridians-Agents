package agentfactory

// UseCase：AgentFactory 域的应用层。
//
// 装配链（wire）：MySQL Repo + Milvus VectorStore + Redis Cache + llm.Manager(Chatter)
// + embedder.EmbeddingClient(Embedder) + mcp.UseCase(ToolPort)。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kratos/kratos/v3/log"
)

// Options 运行参数（护栏默认值，见 conf data.agentfactory）
type Options struct {
	MaxAgents       int           // Agent 总数配额
	DefaultMaxTurns int           // 未指定时的执行图轮数
	WorkerCount     int           // 任务 worker 数
	LockTTL         time.Duration // 任务 Redlock 租期（到期自动续）
	ShutdownTimeout time.Duration // 优雅停机时等待执行中任务的最长时间
}

func (o *Options) fill() {
	if o.MaxAgents <= 0 {
		o.MaxAgents = 200
	}
	if o.DefaultMaxTurns <= 0 {
		o.DefaultMaxTurns = 12
	}
	if o.WorkerCount <= 0 {
		o.WorkerCount = 2
	}
	if o.LockTTL <= 0 {
		o.LockTTL = 30 * time.Second
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 20 * time.Second
	}
}

// UseCase AgentFactory 用例
type UseCase struct {
	repo    Repo
	vec     VectorStore
	cache   Cache
	lock    TaskLock
	chat    Chatter
	emb     Embedder
	tools   ToolPort
	prompts PromptSaver // 可为 nil：LLM 创建 Agent 时沉淀角色提示词到提示词工程
	catalog *Catalog
	opt     Options

	baseCtx context.Context // Recorder 落事件用（生命周期 = 进程）

	// worker 池
	wg        sync.WaitGroup // 全部后台协程（worker + 调度器），随 ctx 取消退出
	workerWG  sync.WaitGroup // 仅 worker，用于优雅停机时判断「执行中的任务已结束」
	workerCtx context.Context
	stopFn    context.CancelFunc

	// 优雅停机：draining 置位后不再领取新任务，drainCh 唤醒阻塞在取任务上的 worker
	draining  atomic.Bool
	drainOnce sync.Once
	drainCh   chan struct{}
	inFlight  atomic.Int64 // 正在执行的任务数（仅用于停机日志）
	stopOnce  sync.Once

	// 运行中任务的取消句柄（CancelTask 用）
	runMu   sync.Mutex
	cancels map[uint64]context.CancelFunc

	vecMu   sync.Mutex
	vecInit bool
}

// New 构造用例
func New(repo Repo, vec VectorStore, cache Cache, lock TaskLock, chat Chatter, emb Embedder, tools ToolPort, skills SkillPort, prompts PromptSaver, opt Options) (*UseCase, error) {
	if repo == nil || vec == nil || cache == nil || emb == nil {
		return nil, fmt.Errorf("AgentFactory 用例初始化失败: 依赖缺失")
	}
	opt.fill()
	u := &UseCase{
		repo:    repo,
		vec:     vec,
		cache:   cache,
		lock:    lock,
		chat:    chat,
		emb:     emb,
		tools:   tools,
		prompts: prompts,
		opt:     opt,
		baseCtx: context.Background(),
		cancels: make(map[uint64]context.CancelFunc),
		drainCh: make(chan struct{}),
	}
	u.catalog = NewCatalog(tools, func(ctx context.Context) ([]AgentDef, error) {
		return repo.ListDefs(ctx, DefFilter{Enabled: boolPtr(true), Limit: 500})
	}, skills)
	return u, nil
}

func (u *UseCase) warn(msg string, kv ...any) {
	log.Warn(msg, kv...)
}

// ———— Agent 定义 CRUD ————

// ListDefs 定义列表
func (u *UseCase) ListDefs(ctx context.Context, f DefFilter) ([]AgentDef, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	return u.repo.ListDefs(ctx, f)
}

// GetDef 详情（缓存优先）
func (u *UseCase) GetDef(ctx context.Context, id uint64) (*AgentDef, error) {
	if def, ok := u.cache.GetDef(ctx, id); ok {
		return def, nil
	}
	def, err := u.repo.GetDef(ctx, id)
	if err != nil {
		return nil, err
	}
	u.cache.SetDef(ctx, def)
	return def, nil
}

// loadDefByKey 按标识加载（委派/提交任务用），停用的直接拒
func (u *UseCase) loadDefByKey(ctx context.Context, key string) (*AgentDef, error) {
	def, err := u.repo.GetDefByKey(ctx, strings.TrimSpace(strings.ToLower(key)))
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, ErrDefNotFound
	}
	return def, nil
}

// CreateDef 人工低代码创建
func (u *UseCase) CreateDef(ctx context.Context, in AgentInput) (*AgentDef, error) {
	return u.Factory().Create(ctx, SourceManual, in)
}

// UpdateDef 更新定义（版本 +1，白名单重新校验，向量与缓存同步刷新）
func (u *UseCase) UpdateDef(ctx context.Context, id uint64, in AgentInput) (*AgentDef, error) {
	old, err := u.repo.GetDef(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := in.Normalize(u.opt.DefaultMaxTurns); err != nil {
		return nil, err
	}
	for _, c := range in.Capabilities {
		ok, err := u.catalog.Known(ctx, c)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrCapabilityDeny, c)
		}
	}
	// 护栏 4（沙箱不可委派）已在 in.Normalize → ValidateSandboxCaps 内统一校验
	// 角色引用校验（成员 / 监察 / 风险 Agent）
	if err := u.validateRoleRefs(ctx, in); err != nil {
		return nil, err
	}
	// key 冲突检查
	if in.AgentKey != old.AgentKey {
		if exists, err := u.repo.GetDefByKey(ctx, in.AgentKey); err != nil {
			return nil, err
		} else if exists != nil && exists.ID != id {
			return nil, fmt.Errorf("%w: 标识 %q 已被占用", ErrDefExists, in.AgentKey)
		}
	}

	def := *old
	def.AgentKey = in.AgentKey
	def.Name = in.Name
	def.Description = in.Description
	def.System = in.System
	def.LLMModel = in.LLMModel
	def.LLMSource = in.LLMSource
	def.GraphType = in.GraphType
	def.Capabilities = in.Capabilities
	def.Middlewares = in.Middlewares
	def.MaxTurns = in.MaxTurns
	def.Sandbox = in.Sandbox
	def.Role = in.Role
	def.Orchestration = in.Orchestration
	def.Members = in.Members
	def.Flow = in.Flow
	def.SupervisorKey = in.SupervisorKey
	def.SupervisionScope = in.SupervisionScope
	def.RiskAgentKey = in.RiskAgentKey
	def.AutoRun = in.AutoRun
	def.CronExpr = in.CronExpr
	def.CronInput = in.CronInput
	// 定时运行：关闭时清空调度时间；开启时重算下次触发
	if def.AutoRun {
		def.NextRunAt = nextRunTime(def.CronExpr, time.Now())
	} else {
		def.NextRunAt = time.Time{}
	}
	def.Version++
	if err := u.repo.UpdateDef(ctx, &def); err != nil {
		return nil, err
	}
	if err := u.pushDefVectors(ctx, &def); err != nil {
		u.warn("Agent 向量更新失败", "key", def.AgentKey, "err", err)
	}
	u.cache.SetDef(ctx, &def)
	return &def, nil
}

// DeleteDef 删除（连带向量与缓存；任务历史保留，agent_key 冗余在任务上）
func (u *UseCase) DeleteDef(ctx context.Context, id uint64) error {
	def, err := u.repo.GetDef(ctx, id)
	if err != nil {
		return err
	}
	if err := u.repo.DeleteDef(ctx, id); err != nil {
		return err
	}
	if def.Vectorized {
		if err := u.vec.Delete(ctx, []uint64{id}); err != nil {
			u.warn("删除 Agent 向量失败", "id", id, "err", err)
		}
	}
	u.cache.DeleteDefs(ctx, []uint64{id})
	return nil
}

// ToggleDef 启停（关闭沙箱也走这里：sandbox 随 UpdateDef 调整）
func (u *UseCase) ToggleDef(ctx context.Context, id uint64, enabled bool) (*AgentDef, error) {
	if err := u.repo.SetDefEnabled(ctx, id, enabled); err != nil {
		return nil, err
	}
	def, err := u.repo.GetDef(ctx, id)
	if err != nil {
		return nil, err
	}
	u.cache.SetDef(ctx, def)
	return def, nil
}

// Capabilities 当前全部可用能力（前端白名单选择器 + LLM 提示词共用）
func (u *UseCase) Capabilities(ctx context.Context) ([]CapabilitySchema, error) {
	return u.catalog.Schemas(ctx)
}

// ———— 语义召回（注册表） ————

// SearchDefs 语义召回：需求 → 最匹配的 Agent 定义
func (u *UseCase) SearchDefs(ctx context.Context, query string, limit int) ([]DefHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("查询内容必填")
	}
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	if err := u.ensureVectorStore(ctx); err != nil {
		return nil, err
	}
	vecs, err := u.emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("查询向量化失败: %w", err)
	}
	hits, err := u.vec.Search(ctx, vecs[0], limit, VectorFilter{Enabled: boolPtr(true)})
	if err != nil {
		return nil, fmt.Errorf("Agent 向量检索失败: %w", err)
	}
	if len(hits) == 0 {
		return []DefHit{}, nil
	}
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.AgentID)
	}
	defs, err := u.repo.ListDefs(ctx, DefFilter{IDs: ids, Limit: len(ids)})
	if err != nil {
		return nil, err
	}
	byID := make(map[uint64]AgentDef, len(defs))
	for _, d := range defs {
		byID[d.ID] = d
	}
	out := make([]DefHit, 0, len(hits))
	for _, h := range hits {
		d, ok := byID[h.AgentID]
		if !ok {
			continue // 向量与库短暂不一致，跳过
		}
		out = append(out, DefHit{Agent: d, Score: h.Score, Source: "milvus"})
	}
	return out, nil
}

// DefHit 语义召回命中
type DefHit struct {
	Agent  AgentDef `json:"agent"`
	Score  float64  `json:"score"`
	Source string   `json:"source"`
}

// pushDefVectors 定义写入向量注册表
func (u *UseCase) pushDefVectors(ctx context.Context, def *AgentDef) error {
	if err := u.ensureVectorStore(ctx); err != nil {
		return fmt.Errorf("Agent 向量集合不可用: %w", err)
	}
	vecs, err := u.emb.Embed(ctx, []string{def.EmbeddingText()})
	if err != nil {
		return fmt.Errorf("Agent 向量化失败: %w", err)
	}
	if err := u.vec.Upsert(ctx, []AgentDef{*def}, vecs); err != nil {
		return err
	}
	_ = u.repo.MarkDefsVectorized(ctx, []uint64{def.ID}, true)
	def.Vectorized = true
	return nil
}

// SyncAllVectors 全量修复向量注册表（增删改都即时同步，此接口兜底）
func (u *UseCase) SyncAllVectors(ctx context.Context) (int, error) {
	defs, err := u.repo.ListDefs(ctx, DefFilter{Enabled: boolPtr(true), Limit: 1000})
	if err != nil {
		return 0, err
	}
	if len(defs) == 0 {
		return 0, nil
	}
	if err := u.ensureVectorStore(ctx); err != nil {
		return 0, err
	}
	texts := make([]string, 0, len(defs))
	for _, d := range defs {
		texts = append(texts, d.EmbeddingText())
	}
	vecs, err := u.emb.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("批量向量化失败: %w", err)
	}
	if err := u.vec.Upsert(ctx, defs, vecs); err != nil {
		return 0, err
	}
	ids := make([]uint64, 0, len(defs))
	for _, d := range defs {
		ids = append(ids, d.ID)
	}
	_ = u.repo.MarkDefsVectorized(ctx, ids, true)
	return len(defs), nil
}

func (u *UseCase) ensureVectorStore(ctx context.Context) error {
	u.vecMu.Lock()
	defer u.vecMu.Unlock()
	if u.vecInit {
		return nil
	}
	if err := u.vec.EnsureCollection(ctx); err != nil {
		return err
	}
	u.vecInit = true
	return nil
}

// ———— 任务：提交 / 查询 / 取消 / 重试 ————

// SubmitTask 提交异步任务：落库 pending → Redis 入队
func (u *UseCase) SubmitTask(ctx context.Context, in TaskInput) (*Task, error) {
	if u.draining.Load() {
		return nil, ErrShuttingDown
	}
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	var def *AgentDef
	var err error
	if in.AgentDefID > 0 {
		def, err = u.GetDef(ctx, in.AgentDefID)
	} else {
		def, err = u.loadDefByKey(ctx, in.AgentKey)
	}
	if err != nil {
		return nil, err
	}
	if !def.Enabled {
		return nil, fmt.Errorf("%w: Agent「%s」已停用，先启用再提交", ErrInvalidTask, def.Name)
	}
	task := &Task{
		AgentDefID: def.ID,
		AgentKey:   def.AgentKey,
		Title:      truncateRunes(in.Input, 80),
		Input:      in.Input,
		Status:     TaskPending,
		ConvID:     in.ConvID,
	}
	if err := u.repo.CreateTask(ctx, task); err != nil {
		return nil, err
	}
	if err := u.cache.EnqueueTask(ctx, task.ID); err != nil {
		// 入队失败不静默：直接报错，任务留在 pending，恢复扫描会重新入队
		return task, fmt.Errorf("任务已保存但入队失败（worker 稍后会自动拾起）: %w", err)
	}
	return task, nil
}

// ListTasks 任务列表
func (u *UseCase) ListTasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	return u.repo.ListTasks(ctx, f)
}

// GetTask 任务详情（运行中优先读 Redis 快照，实时性更好）
func (u *UseCase) GetTask(ctx context.Context, id uint64) (*Task, error) {
	if t, ok := u.cache.GetTask(ctx, id); ok {
		return t, nil
	}
	t, err := u.repo.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// TaskEvents 任务事件（trace 链路）
func (u *UseCase) TaskEvents(ctx context.Context, taskID uint64, limit int) ([]TaskEvent, error) {
	if _, err := u.repo.GetTask(ctx, taskID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	return u.repo.ListEvents(ctx, taskID, limit)
}

// CancelTask 取消任务：pending 直接置 canceled；running 通过 ctx 取消执行
func (u *UseCase) CancelTask(ctx context.Context, id uint64) (*Task, error) {
	t, err := u.repo.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	switch t.Status {
	case TaskPending:
		t.Status = TaskCanceled
		t.FinishedAt = time.Now()
		// pending 是活跃态（任务池），取消即终态 → 迁移到任务历史表
		if err := u.repo.FinishTask(ctx, t); err != nil {
			return nil, err
		}
		return t, nil
	case TaskRunning:
		u.runMu.Lock()
		cancel, ok := u.cancels[id]
		u.runMu.Unlock()
		if ok {
			cancel()
		}
		return t, nil
	case TaskSucceeded, TaskFailed, TaskCanceled:
		return nil, fmt.Errorf("%w: 任务已是终态 %s", ErrTaskStateDeny, t.Status)
	default:
		return nil, ErrTaskStateDeny
	}
}

// RetryTask 重试终态任务：终态任务已归档在任务历史表，先复活回任务池
// （重置为 pending），再重新入队（trace 保留在同一条任务上）
func (u *UseCase) RetryTask(ctx context.Context, id uint64) (*Task, error) {
	t, err := u.repo.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if !IsTerminalStatus(t.Status) {
		return nil, fmt.Errorf("%w: 仅终态任务可重试，当前 %s", ErrTaskStateDeny, t.Status)
	}
	t.Status = TaskPending
	t.Progress = 0
	t.Result = ""
	t.ErrorMsg = ""
	t.FinishedAt = time.Time{}
	t.Archived = false
	// 历史表 → 任务池（事务内完成插池 + 删历史）
	if err := u.repo.ReviveTask(ctx, t); err != nil {
		return nil, err
	}
	if err := u.cache.EnqueueTask(ctx, t.ID); err != nil {
		return t, fmt.Errorf("重新入队失败（恢复扫描会自动拾起）: %w", err)
	}
	return t, nil
}

// ———— 统计 ————

// Stats 规模统计
func (u *UseCase) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	defs, err := u.repo.ListDefs(ctx, DefFilter{Limit: 10000})
	if err != nil {
		return st, err
	}
	st.Agents = int64(len(defs))
	for _, d := range defs {
		if d.Enabled {
			st.EnabledAgents++
		}
		if d.Source == SourceLLM {
			st.LLMBorn++
		}
		if d.Sandbox {
			st.Sandboxed++
		}
		if d.Vectorized {
			st.Vectorized++
		}
	}
	taskCounts, err := u.repo.CountTasksByStatus(ctx)
	if err != nil {
		return st, err
	}
	for status, n := range taskCounts {
		st.Tasks += n
		switch status {
		case TaskPending:
			st.Pending = n
		case TaskRunning:
			st.Running = n
		case TaskSucceeded:
			st.Succeeded = n
		case TaskFailed:
			st.Failed = n
		case TaskCanceled:
			st.Canceled = n
		}
	}
	if caps, err := u.catalog.Schemas(ctx); err == nil {
		st.Capabilities = int64(len(caps))
	}
	return st, nil
}

// ———— 异步 worker ————

// StartWorkers 启动 worker 池 + 崩溃恢复（在 wire cleanup 中调用 Stop）
func (u *UseCase) StartWorkers() error {
	// 崩溃恢复：上次进程退出时遗留的 running 置回 pending，与 pending 一起重新入队
	ids, err := u.repo.RequeueInterrupted(u.baseCtx)
	if err != nil {
		u.warn("任务恢复扫描失败", "err", err)
	}
	for _, id := range ids {
		if err := u.cache.EnqueueTask(u.baseCtx, id); err != nil {
			u.warn("恢复入队失败", "task", id, "err", err)
		}
	}
	if len(ids) > 0 {
		log.Info("恢复中断任务", "count", len(ids))
	}
	u.workerCtx, u.stopFn = context.WithCancel(u.baseCtx)
	for i := 0; i < u.opt.WorkerCount; i++ {
		u.wg.Add(1)
		u.workerWG.Add(1)
		go u.workerLoop(u.workerCtx, i)
	}
	// cron 调度器：后台自动运行根 Agent（与 worker 同生命周期）
	u.startScheduler(u.workerCtx)
	return nil
}

// Stop 硬停：取消 worker 上下文并等待退出。
// 正常停机应先调用 Drain（优雅排空），此处只作为兜底（Drain 已退出时立即返回）。
func (u *UseCase) Stop() {
	u.stopOnce.Do(func() {
		if u.stopFn != nil {
			u.stopFn()
		}
	})
	u.wg.Wait()
}

// Drain 优雅停机：立刻停止领取新任务，等待「已在执行」的任务跑完再返回。
//
// 与 Stop 的区别：Drain 期间 workerCtx 不取消，所以运行中的 LLM 调用、工具调用
// 不会被掐断；worker 只是不再从队列里取新任务。等待上限取
// max(u.opt.ShutdownTimeout, 传入 ctx 的剩余时间中更短者)，避免超过编排层的
// kill 期限（k8s terminationGracePeriod 默认 30s）。
//
// 超时后强制取消：被打断的任务保持 running 状态不写终态，下次启动由
// RequeueInterrupted 重新入队执行（不会丢任务，也不会被误标成「已取消」）。
// 可重复调用（幂等）。
func (u *UseCase) Drain(ctx context.Context) error {
	u.drainOnce.Do(func() {
		u.draining.Store(true)
		close(u.drainCh) // 唤醒阻塞在 DequeueTask 上的 worker
		log.Info("开始优雅停机：停止领取新任务，等待执行中的任务完成",
			"in_flight", u.inFlight.Load(), "timeout", u.opt.ShutdownTimeout.String())
	})
	if u.workerCtx == nil { // worker 未启动
		return nil
	}

	wait := u.opt.ShutdownTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < wait {
			wait = remaining
		}
	}
	done := make(chan struct{})
	go func() {
		u.workerWG.Wait() // 只看 worker：调度器随 ctx 退出，不计入排空等待
		close(done)
	}()
	select {
	case <-done:
		log.Info("优雅停机完成：执行中的任务已全部结束")
		return nil
	case <-time.After(wait):
		log.Warn("优雅停机等待超时，强制中断执行中的任务（下次启动会自动重跑，不会丢任务）",
			"in_flight", u.inFlight.Load())
		u.Stop() // 取消 ctx 并等待 goroutine 退出
		return nil
	}
}

func (u *UseCase) workerLoop(ctx context.Context, idx int) {
	defer u.wg.Done()
	defer u.workerWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.drainCh: // 优雅停机：不再领新任务
			return
		default:
		}
		if u.draining.Load() {
			return
		}
		id, ok := u.cache.DequeueTask(ctx, time.Second)
		if !ok {
			continue
		}
		// 已出队的任务必须跑完：消息已被消费，跳过会留下孤儿任务（只能等下次启动恢复）
		u.inFlight.Add(1)
		u.claimAndRun(ctx, id)
		u.inFlight.Add(-1)
	}
}

// claimAndRun 认领并执行任务
func (u *UseCase) claimAndRun(ctx context.Context, id uint64) {
	t, err := u.repo.GetTask(ctx, id)
	if err != nil || t == nil {
		u.warn("任务读取失败", "task", id, "err", err)
		return
	}
	if t.Status != TaskPending {
		return // 已被其他 worker 处理/已取消
	}
	def, err := u.repo.GetDef(ctx, t.AgentDefID)
	if err != nil || def == nil {
		t.Status = TaskFailed
		t.ErrorMsg = "Agent 定义不存在或已删除"
		t.FinishedAt = time.Now()
		_ = u.repo.FinishTask(u.baseCtx, t)
		return
	}
	if _, err := u.executeTaskNow(ctx, t, def); err != nil {
		// 锁被其他节点持有：不视为失败，任务由持有者执行；
		// 停机打断属预期（任务已保留 running 待重跑），不打错误日志
		if !errors.Is(err, ErrTaskLockBusy) && !(u.draining.Load() && errors.Is(err, context.Canceled)) {
			u.warn("任务执行失败", "task", id, "err", err)
		}
	}
}

// acquireTaskLock 获取任务执行 Redlock，返回释放函数。
// lock 未装配（单测场景）时直接放行；获取失败返回 ErrTaskLockBusy。
// 成功后启动续期协程：TTL/3 周期续约，长任务不会被锁过期误伤。
func (u *UseCase) acquireTaskLock(ctx context.Context, taskID uint64) (func(), error) {
	if u.lock == nil {
		return func() {}, nil
	}
	token, ok := u.lock.Acquire(ctx, taskID, u.opt.LockTTL)
	if !ok {
		return nil, fmt.Errorf("%w: task %d", ErrTaskLockBusy, taskID)
	}
	ttl := u.opt.LockTTL
	// 续期协程：锁持有期间周期性 Extend，ctx 结束即停
	renewCtx, stopRenew := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				u.lock.Extend(renewCtx, taskID, token, ttl)
			}
		}
	}()
	release := func() {
		stopRenew()
		wg.Wait()
		u.lock.Release(context.WithoutCancel(ctx), taskID, token)
	}
	return release, nil
}

// executeTaskNow 同步执行任务（worker 主路径与子 Agent 委派共用）：
// Redlock 防重复触发 → 认领 → running → 执行图 → 终态落历史表。
// 返回最终答案。
// RunOnce 同步执行一次 Agent（评测 harness 的被测入口）：建一条独立任务记录并内联执行，
// 返回最终答案。trace 事件 / LLM 调用记录正常落库，链路可查。
func (u *UseCase) RunOnce(ctx context.Context, agentKey, input string) (string, error) {
	def, err := u.loadDefByKey(ctx, agentKey)
	if err != nil {
		return "", err
	}
	if !def.Enabled {
		return "", fmt.Errorf("%w: Agent「%s」已停用", ErrInvalidTask, def.Name)
	}
	t := &Task{
		AgentDefID: def.ID,
		AgentKey:   def.AgentKey,
		Title:      truncateRunes(input, 80),
		Input:      input,
		Status:     TaskPending,
	}
	if err := u.repo.CreateTask(ctx, t); err != nil {
		return "", err
	}
	return u.executeTaskNow(ctx, t, def)
}

func (u *UseCase) executeTaskNow(ctx context.Context, t *Task, def *AgentDef) (string, error) {
	// ① Redlock 分布式锁：同一任务跨节点/跨 worker 互斥（队列重复投递、
	// 恢复扫描与实时提交并发等场景的最后一道防线）
	release, err := u.acquireTaskLock(ctx, t.ID)
	if err != nil {
		return "", err
	}
	defer release()

	// ② 原子认领：pending → running（并发安全）
	if t.Status == TaskPending {
		ok, err := u.repo.ClaimTask(ctx, t.ID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: 已被其他 worker 认领", ErrTaskStateDeny)
		}
	}
	t.Status = TaskRunning
	t.StartedAt = time.Now()
	if t.Progress < 5 {
		t.Progress = 5
	}
	_ = u.repo.SaveTask(ctx, t)

	rec := &Recorder{u: u, task: t, def: def}
	rec.status("", TaskRunning)
	rec.snapshot()

	// 注册取消句柄
	runCtx, cancel := context.WithCancel(ctx)
	u.runMu.Lock()
	u.cancels[t.ID] = cancel
	u.runMu.Unlock()
	defer func() {
		cancel()
		u.runMu.Lock()
		delete(u.cancels, t.ID)
		u.runMu.Unlock()
	}()

	runRes, runErr := u.runGraph(runCtx, def, t.Input, rec)
	answer := ""
	if runRes != nil {
		answer = runRes.Answer
	}

	now := time.Now()
	t.FinishedAt = now
	if runErr != nil {
		// 停机导致的中断：不写终态。任务保持 running，下次启动由
		// RequeueInterrupted 重新入队，避免被误标成「已取消」而丢失。
		if u.draining.Load() && errors.Is(runErr, context.Canceled) {
			rec.event(EventError, rec.turn, map[string]any{"message": "服务停止，任务中断，将在重启后自动重跑"})
			rec.status(TaskRunning, TaskRunning)
			t.ErrorMsg = "服务停止，任务中断，将在重启后自动重跑"
			t.FinishedAt = time.Time{}
			_ = u.repo.SaveTask(u.baseCtx, t)
			rec.snapshot()
			return "", runErr
		}
		if errors.Is(runCtx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Status = TaskCanceled
			t.ErrorMsg = "任务被取消"
		} else {
			t.Status = TaskFailed
			t.ErrorMsg = runErr.Error()
		}
		rec.event(EventError, rec.turn, map[string]any{"message": t.ErrorMsg})
		rec.status(TaskRunning, t.Status)
		t.Progress = 100
		_ = u.repo.FinishTask(u.baseCtx, t) // 终态：任务池 → 任务历史表
		rec.snapshot()
		return "", runErr
	}
	t.Status = TaskSucceeded
	t.Result = answer
	t.Progress = 100
	rec.status(TaskRunning, TaskSucceeded)
	_ = u.repo.FinishTask(u.baseCtx, t) // 终态：任务池 → 任务历史表
	rec.snapshot()
	return answer, nil
}
