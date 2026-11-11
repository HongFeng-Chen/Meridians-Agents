package agentfactory

// UseCase：AgentFactory 域的应用层。
//
// 装配链（wire）：MySQL Repo + Milvus VectorStore + Redis Cache + llm.Manager(Chatter)
// + embedder.EmbeddingClient(Embedder) + mcp.UseCase(ToolPort)。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go_agent/internal/biz/audit"
	"go_agent/internal/pkg/budget"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/trace"

	"github.com/go-kratos/kratos/v3/log"
)

// Options 运行参数（护栏默认值，见 conf data.agentfactory）
type Options struct {
	MaxAgents       int           // Agent 总数配额
	DefaultMaxTurns int           // 未指定时的执行图轮数
	WorkerCount     int           // 任务 worker 数
	LockTTL         time.Duration // 任务 Redlock 租期（到期自动续）
	ShutdownTimeout time.Duration // 优雅停机时等待执行中任务的最长时间

	// MaxParallel 并行波次并发上限（<=0 用 defaultMaxParallel）。
	// 一个 20 节点的 DAG 不会再同时拉起 20 个 Agent。
	MaxParallel int
	// BudgetTokens / BudgetCalls 单任务树（含派生子任务）的 token 与调用次数上限，
	// 0 表示不限制；超限时执行图走「预算耗尽」收敛分支。
	BudgetTokens int64
	BudgetCalls  int32

	// —— V2-P2 执行链路（执行体注册 / 租约 / 超时重试）——

	// LeaseTTL 任务执行租约有效期：执行期间按 TTL/3 自动续租，进程死亡即自然过期
	LeaseTTL time.Duration
	// HeartbeatInterval 执行体心跳间隔
	HeartbeatInterval time.Duration
	// HeartbeatTTL 心跳有效期（> 2×HeartbeatInterval，容忍漏一次心跳）
	HeartbeatTTL time.Duration
	// TaskTimeout 单任务执行截止时间，超过即落 timeout 终态
	TaskTimeout time.Duration
	// MaxAttempts 基础设施故障（执行体失联）重试上限；任务自身 max_attempts 优先
	MaxAttempts int
	// SweepInterval 清扫间隔：回收过期租约 / 超时任务 / 到期重试
	SweepInterval time.Duration
	// WorkerKey 执行体标识（空 = hostname-pid-随机后缀）
	WorkerKey string

	// MaxDelegationDepth 委派深度上限（根 = 0），0 走默认值。
	// 防止 A→B→A 这类成环链路把预算烧光；与 MaxAgents 数量配额互补。
	MaxDelegationDepth int
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
	if o.MaxDelegationDepth <= 0 {
		o.MaxDelegationDepth = MaxDelegationDepth
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = 60 * time.Second
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 15 * time.Second
	}
	if o.HeartbeatTTL <= 2*o.HeartbeatInterval {
		o.HeartbeatTTL = 3 * o.HeartbeatInterval
	}
	if o.TaskTimeout <= 0 {
		o.TaskTimeout = 30 * time.Minute
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = 20 * time.Second
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
	// authz 运行时二次授权（可为 nil：单测 / 未接线的部署不做资源级判权）
	authz   ResourceAuthorizer
	catalog *Catalog
	opt     Options
	// workers 执行体注册表（可为 nil：单机/测试态退化为「无注册表」，
	// 此时租约仍写库、仍能被清扫，只是看不到实例维度的心跳）
	workers WorkerRepo
	// leaseOwner 本进程的执行体标识：任务认领时写入 lease_owner
	leaseOwner string
	// workerStarted 本进程启动时间（注册表展示）
	workerStarted time.Time

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

// New 构造用例（不含执行体注册表：单机 / 测试态）
func New(repo Repo, vec VectorStore, cache Cache, lock TaskLock, chat Chatter, emb Embedder, tools ToolPort, skills SkillPort, prompts PromptSaver, opt Options) (*UseCase, error) {
	return NewWithWorkers(repo, vec, cache, lock, chat, emb, tools, skills, prompts, nil, opt)
}

// NewWithWorkers 构造用例并注入执行体注册表（生产装配）。
// workers 为 nil 时退化为无注册表模式：任务租约照常写库、失联任务照常能被清扫，
// 只是实例心跳不可见（本进程回收自己遗留的 running 依然生效）。
func NewWithWorkers(repo Repo, vec VectorStore, cache Cache, lock TaskLock, chat Chatter, emb Embedder, tools ToolPort, skills SkillPort, prompts PromptSaver, workers WorkerRepo, opt Options) (*UseCase, error) {
	if repo == nil || vec == nil || cache == nil || emb == nil {
		return nil, fmt.Errorf("AgentFactory 用例初始化失败: 依赖缺失")
	}
	opt.fill()
	owner := strings.TrimSpace(opt.WorkerKey)
	if owner == "" {
		owner = newWorkerKey()
	}
	u := &UseCase{
		repo:          repo,
		vec:           vec,
		cache:         cache,
		lock:          lock,
		chat:          chat,
		emb:           emb,
		tools:         tools,
		prompts:       prompts,
		opt:           opt,
		workers:       workers,
		leaseOwner:    owner,
		workerStarted: time.Now(),
		baseCtx:       context.Background(),
		cancels:       make(map[uint64]context.CancelFunc),
		drainCh:       make(chan struct{}),
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
	def.AutoCompose = in.AutoCompose
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
	def.RequireApproval = in.RequireApproval
	def.ApprovalTimeoutHours = in.ApprovalTimeoutHours
	def.RiskPauseLevel = in.RiskPauseLevel
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
	// 生成配置快照（内容没变则复用最新一条）：已提交的任务继续跑旧快照，
	// 新任务钉新快照，审计链因此可复现。
	if _, serr := u.publishSnapshot(ctx, &def); serr != nil {
		u.warn("Agent 快照生成失败", "agent", def.AgentKey, "err", serr)
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
		// 归属用户取自提交时的请求身份：worker 后续用 baseCtx 执行，但归属不再变化。
		// 委派出的子任务在同一 ctx 下提交，因此天然继承父任务身份。
		// cron 自动排期走 baseCtx（无请求身份）→ 落在 default 作用域。
		UserID:   identity.UserID(ctx),
		TenantID: identity.TenantID(ctx),
		// 业务重试次数由调用方决定：默认 0（一次失败即终态），
		// 基础设施故障的重试上限由 Options.MaxAttempts 兜底
		MaxAttempts: in.MaxAttempts,
		// 钉住提交时刻的配置快照：之后改提示词 / 能力不影响这条任务
		SnapshotID: u.latestSnapshotID(ctx, def.ID),
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
//
// 状态判断与写库之间必须没有窗口（P1-05）：取消「读到的 pending」时，worker 可能
// 已经认领成 running 并开始产生副作用，此时再把 running 覆盖成 canceled，
// 库里就成了「已取消」，而事实是「已经执行过」。所以这里用 CAS：
// 迁移失败说明状态变了，重新读一次按新状态处理。
func (u *UseCase) CancelTask(ctx context.Context, id uint64) (*Task, error) {
	for attempt := 0; attempt < 3; attempt++ {
		t, err := u.repo.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		switch t.Status {
		case TaskPending, TaskPaused, TaskRetryWait:
			prev := t.Status
			t.Status = TaskCanceled
			t.FinishedAt = time.Now()
			t.LeaseOwner, t.LeaseExpiresAt, t.NextAttemptAt = "", time.Time{}, time.Time{}
			switch prev {
			case TaskPaused:
				// 挂起任务取消 = 直接驳回终态（清 checkpoint，不再等审批）
				t.ErrorMsg = "任务在审批挂起期间被取消"
			case TaskRetryWait:
				// 重试等待中的任务没有执行体在跑，直接终态，不必等退避到期
				t.ErrorMsg = "任务在重试等待期间被取消"
			}
			ok, err := u.repo.TransitionTask(ctx, id, []string{prev}, t)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // 状态被 worker / 审批改掉了，重新读一次
			}
			if prev == TaskPaused {
				u.repo.TrimCheckpoints(ctx, id, 0)
				u.emitSystemEvent(ctx, id, EventApproval, map[string]any{"decision": "cancel", "by": identity.UserID(ctx)})
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
		case TaskSucceeded, TaskFailed, TaskCanceled, TaskTimeout:
			return nil, fmt.Errorf("%w: 任务已是终态 %s", ErrTaskStateDeny, t.Status)
		default:
			return nil, ErrTaskStateDeny
		}
	}
	return nil, fmt.Errorf("%w: 任务状态反复变化，请稍后重试", ErrTaskStateDeny)
}

// ApproveTask 审批挂起中的任务（O3）：approve 为「这一次具体调用」签发凭据
// （能力 + 参数指纹 + 策略版本）并置回 pending 从 checkpoint 续跑；reject 直接终态失败。
func (u *UseCase) ApproveTask(ctx context.Context, id uint64, decision, note string) (*Task, error) {
	t, err := u.repo.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Status != TaskPaused {
		return nil, fmt.Errorf("%w: 仅挂起（paused）任务可审批，当前 %s", ErrTaskStateDeny, t.Status)
	}
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != "approve" && decision != "reject" {
		return nil, fmt.Errorf("%w: decision 仅支持 approve / reject", ErrInvalidTask)
	}

	by := identity.UserID(ctx)
	payload := map[string]any{"decision": decision, "by": by, "note": truncateRunes(note, 500)}

	if decision == "reject" {
		t.Status = TaskFailed
		t.ErrorMsg = "人工驳回：" + truncateRunes(note, 300)
		t.FinishedAt = time.Now()
		// paused → failed 走 CAS：审批与「挂起期取消」可能同时发生，
		// 后到的那个不能把对方的结果覆盖掉
		ok, err := u.repo.TransitionTask(ctx, id, []string{TaskPaused}, t)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: 任务状态已变化（可能已被取消），请刷新后重试", ErrTaskStateDeny)
		}
		u.repo.TrimCheckpoints(ctx, id, 0)
		u.emitSystemEvent(ctx, id, EventApproval, payload)
		return t, nil
	}

	// approve：把「这一次具体调用」记入凭据，任务置回 pending 重新入队
	cp, _ := u.repo.LatestCheckpoint(ctx, id)
	var st *RunState
	if cp != nil {
		st = decodeState(cp.State)
	}
	if st == nil {
		st = newState(t.Input)
	}
	req, hasReq := approvalRequest{}, false
	if cp != nil {
		req, hasReq = decodeApprovalCursor(cp.Cursor)
	}
	if hasReq {
		// 审批人资格：请求人自批高危一律拒绝；跨用户审批需要管理员
		selfApproved, aerr := u.checkApprover(ctx, t, req)
		if aerr != nil {
			return nil, aerr
		}
		grant := ApprovalGrant{
			Ability:       req.Ability,
			ArgsHash:      req.ArgsHash,
			PolicyVersion: req.PolicyVersion,
			Requester:     t.UserID,
			Approver:      by,
			Decision:      "approve",
			Reason:        truncateRunes(note, 500),
			HighRisk:      req.HighRisk,
			CreatedAt:     time.Now(),
			ExpiresAt:     time.Now().Add(defaultApprovalTTL),
			SelfApproved:  selfApproved,
		}
		st.grant(grant)
		payload["ability"] = req.Ability
		payload["args_hash"] = req.ArgsHash
		payload["requester"] = t.UserID
		payload["policy_version"] = req.PolicyVersion
		payload["expires_at"] = grant.ExpiresAt.Format(time.RFC3339)
		payload["high_risk"] = req.HighRisk
		if selfApproved {
			payload["self_approved"] = true
		}
		audit.Info(ctx, "agentfactory.task.approve", "task", fmt.Sprint(id), map[string]any{
			"ability": req.Ability, "args_hash": req.ArgsHash, "requester": t.UserID,
			"approver": by, "high_risk": req.HighRisk, "self_approved": selfApproved,
			"policy_version": req.PolicyVersion,
		})
	} else {
		// 老格式 / 损坏的挂起点：不补发凭据。续跑时会重新命中闸门并生成新的
		// 挂起点——多问一次人，好过按能力名补一张永久通行证。
		audit.Info(ctx, "agentfactory.task.approve", "task", fmt.Sprint(id), map[string]any{
			"approver": by, "note": "无可解析的审批挂起点，续跑将重新申请",
		})
	}
	if cp != nil {
		st.Cursor = ""
		_ = u.repo.SaveCheckpoint(ctx, &TaskCheckpoint{
			TaskID:    id,
			Seq:       cp.Seq + 1,
			GraphType: cp.GraphType,
			Cursor:    `{"kind":"resume"}`,
			State:     encodeState(st),
			CreatedAt: time.Now(),
		})
	}
	t.Status = TaskPending
	t.ErrorMsg = ""
	u.runMu.Lock()
	if cancel, ok := u.cancels[id]; ok {
		cancel() // 理论上 paused 任务没有运行句柄，防御性清理
	}
	u.runMu.Unlock()
	// paused → pending 走 CAS：期间可能被取消（另一个请求已经落了 canceled）
	ok, err := u.repo.TransitionTask(ctx, id, []string{TaskPaused}, t)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: 任务状态已变化（可能已被取消），请刷新后重试", ErrTaskStateDeny)
	}
	if err := u.cache.EnqueueTask(ctx, t.ID); err != nil {
		return t, fmt.Errorf("重新入队失败（恢复扫描会自动拾起）: %w", err)
	}
	u.emitSystemEvent(ctx, id, EventApproval, payload)
	return t, nil
}

// emitSystemEvent 无 Recorder 场景下直接落一条任务事件（审批结论等系统事件）
func (u *UseCase) emitSystemEvent(ctx context.Context, taskID uint64, typ string, payload any) {
	_ = u.repo.AddEvent(u.baseCtx, &TaskEvent{
		TaskID:    taskID,
		Type:      typ,
		Payload:   marshalPayload(payload),
		CreatedAt: time.Now(),
	})
}

// approvalTimeoutScan 审批超时扫描（随调度器每 30s 跑一次）：挂起超过
// Agent 配置的超时（默认 72h）自动驳回，避免任务永久悬挂。
func (u *UseCase) approvalTimeoutScan(ctx context.Context) {
	tasks, err := u.repo.ListTasks(ctx, TaskFilter{Status: TaskPaused, Limit: 200})
	if err != nil {
		u.warn("审批超时扫描失败", "err", err)
		return
	}
	for i := range tasks {
		t := &tasks[i]
		cp, err := u.repo.LatestCheckpoint(ctx, t.ID)
		if err != nil || cp == nil {
			continue
		}
		timeout := 72 * time.Hour
		if def, derr := u.repo.GetDef(ctx, t.AgentDefID); derr == nil && def != nil && def.ApprovalTimeoutHours > 0 {
			timeout = time.Duration(def.ApprovalTimeoutHours) * time.Hour
		}
		if time.Since(cp.CreatedAt) <= timeout {
			continue
		}
		t.Status = TaskFailed
		t.ErrorMsg = "审批超时自动驳回"
		t.FinishedAt = time.Now()
		// paused → failed 走 CAS：扫描与「人工审批/取消」可能同时发生，
		// 输掉的一方不能把对方已落库的结论覆盖成「审批超时」
		ok, terr := u.repo.TransitionTask(ctx, t.ID, []string{TaskPaused}, t)
		if terr != nil || !ok {
			if terr != nil {
				u.warn("审批超时落库失败", "task", t.ID, "err", terr)
			}
			continue
		}
		u.repo.TrimCheckpoints(ctx, t.ID, 0)
		u.emitSystemEvent(ctx, t.ID, EventApproval, map[string]any{
			"decision": "reject", "by": "system", "note": "审批超时自动驳回",
		})
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
	// 重试 = 完整重跑：清掉旧 checkpoint（审批续跑走 ApproveTask，两条路径不混用）
	u.repo.TrimCheckpoints(ctx, id, 0)
	t.Status = TaskPending
	t.Progress = 0
	t.Result = ""
	t.ErrorMsg = ""
	t.FinishedAt = time.Time{}
	t.Archived = false
	// 手工重试是「人确认后再来一次」：预算重新给满，别让上次的死信原因与
	// attempt 计数影响这次判定（否则第一次失联就直接又进死信）
	t.Attempt = 0
	t.DeadLetterReason = ""
	t.LeaseOwner, t.LeaseExpiresAt = "", time.Time{}
	t.NextAttemptAt, t.DeadlineAt = time.Time{}, time.Time{}
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
		case TaskRetryWait:
			st.RetryWait = n
		case TaskSucceeded:
			st.Succeeded = n
		case TaskFailed:
			st.Failed = n
		case TaskCanceled:
			st.Canceled = n
		case TaskTimeout:
			st.Timeout = n
		}
	}
	if caps, err := u.catalog.Schemas(ctx); err == nil {
		st.Capabilities = int64(len(caps))
	}
	return st, nil
}

// ———— 异步 worker ————

// StartWorkers 启动 worker 池 + 崩溃恢复 + 执行体心跳/清扫（在 wire cleanup 中调用 Stop）
func (u *UseCase) StartWorkers() error {
	u.workerCtx, u.stopFn = context.WithCancel(u.baseCtx)

	// ① 执行体注册 + 心跳：多副本部署时，清扫协程靠它区分「活着但空闲」
	// 与「进程已经没了，它手上的任务该收回来」
	u.registerWorker(u.workerCtx)
	if u.workers != nil && u.opt.HeartbeatInterval > 0 {
		u.wg.Add(1)
		go u.heartbeatLoop(u.workerCtx)
	}

	// ② 崩溃恢复：上个进程遗留的 running 按同一套重试策略收回重排，
	// 顺带把「入队消息丢了但库里有」的 pending 重新入队
	u.recoverInterrupted(u.workerCtx)

	// ③ 清扫协程：过期租约 / 超时任务 / 到期重试 / 失联执行体
	if u.opt.SweepInterval > 0 {
		u.wg.Add(1)
		go u.sweepLoop(u.workerCtx)
	}

	for i := 0; i < u.opt.WorkerCount; i++ {
		u.wg.Add(1)
		u.workerWG.Add(1)
		go u.workerLoop(u.workerCtx, i)
	}
	// cron 调度器：后台自动运行根 Agent（与 worker 同生命周期）
	u.startScheduler(u.workerCtx)
	return nil
}

// recoverInterrupted 启动时收回上个进程遗留的执行中任务。
//
// 不直接「running 一律改 pending」：那样崩溃循环里的任务会无限重排，
// 永远撞不到重试上限。这里与运行期清扫共用 recoverLostTask，
// 区别只是不等待退避——进程刚起来，任务应该马上继续。
func (u *UseCase) recoverInterrupted(ctx context.Context) {
	now := time.Now()
	running, err := u.repo.ListTasks(ctx, TaskFilter{Status: TaskRunning, Limit: 1000})
	if err != nil {
		u.warn("任务恢复扫描失败", "err", err)
	}
	for i := range running {
		u.recoverLostTask(ctx, &running[i], now, false)
	}
	if len(running) > 0 {
		log.Info("恢复中断任务", "count", len(running))
	}
	// retry_wait 已到期的立即重排：进程重启不必再等一轮退避
	if ids, err := u.repo.PromoteRetryWait(ctx, now, 1000); err != nil {
		u.warn("重试任务恢复失败", "err", err)
	} else {
		for _, id := range ids {
			if err := u.cache.EnqueueTask(ctx, id); err != nil {
				u.warn("恢复入队失败", "task", id, "err", err)
			}
		}
	}
	// pending 行可能因为「保存成功但入队失败」或队列随进程重启丢失而没有消息，
	// 一起重新入队（重复入队无害：认领是 CAS，抢不到直接跳过）
	pending, err := u.repo.ListTasks(ctx, TaskFilter{Status: TaskPending, Limit: 1000})
	if err != nil {
		u.warn("待执行任务扫描失败", "err", err)
		return
	}
	for _, t := range pending {
		if err := u.cache.EnqueueTask(ctx, t.ID); err != nil {
			u.warn("待执行任务入队失败", "task", t.ID, "err", err)
		}
	}
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
	u.deregisterWorker()
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
		// 注册表同步置 draining：运维看板能区分「扩容中」与「正在下线」
		u.setWorkerStatus(WorkerDraining)
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
		// pending → failed 走 CAS：期间可能已被用户取消（已归档进历史表），
		// 这里若无条件写终态会把它从「取消」改成「失败」
		if ok, terr := u.repo.TransitionTask(u.baseCtx, t.ID, []string{TaskPending}, t); terr != nil || !ok {
			if terr != nil {
				u.warn("任务终态落库失败", "task", t.ID, "err", terr)
			}
		}
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
		UserID:     identity.UserID(ctx),   // 试运行同样归属当前请求用户
		TenantID:   identity.TenantID(ctx), // 租户同理
		SnapshotID: u.latestSnapshotID(ctx, def.ID),
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

	// ② 原子认领：pending → running（并发安全），同时写入执行租约。
	// 租约让「进程被 OOM kill / 被驱逐」这类事故可以被自动发现：
	// 续租一停，租约到期，清扫协程就会把任务收回重排（而不是永远卡在 running）。
	if t.Status == TaskPending {
		lease := u.leaseFor(t, time.Now())
		ok, err := u.repo.ClaimTask(ctx, t.ID, lease)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: 已被其他 worker 认领", ErrTaskStateDeny)
		}
		t.Attempt = lease.Attempt
		t.LeaseOwner = lease.Owner
		t.LeaseExpiresAt = lease.ExpiresAt
		t.DeadlineAt = lease.Deadline
		t.NextAttemptAt = time.Time{}
	}
	t.Status = TaskRunning
	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now() // checkpoint 续跑时保留原始开始时间，耗时统计不被重置
	}
	if t.Progress < 5 {
		t.Progress = 5
	}
	_ = u.repo.SaveTask(ctx, t)

	// 按任务钉住的快照加载配置（任务提交后定义再改也不影响这一条的执行内容）
	def, snap := u.defForTask(ctx, t, def)

	// 执行链路根 span：一次执行一条 Trace，LLM / MCP / RAG / Skill 的 span 都挂在它下面。
	// 从 ctx 派生后，下游（runGraph → chatWith → CallTool → rag）拿到的就是这条 trace。
	ctx, taskSpan := trace.Start(ctx, "task.run",
		trace.String(trace.AttrKind, trace.KindTask),
		trace.String(trace.AttrTaskID, strconv.FormatUint(t.ID, 10)),
		trace.String(trace.AttrAgentKey, def.AgentKey),
		trace.Int(trace.AttrAttempt, t.Attempt),
		trace.Int(trace.AttrDepth, t.Depth),
		trace.String(trace.AttrTenantID, t.TenantID),
	)
	var runErr error
	// 用 EndPtr 而不是 End：defer 求值那一刻才知道最终错误
	defer taskSpan.EndPtr(&runErr)
	// trace id 落到任务行：内存 trace 被淘汰后，仍可拿它去 collector 查历史链路
	t.TraceID = taskSpan.TraceID()
	if t.TraceID != "" {
		_ = u.repo.SaveTask(ctx, t)
	}
	if snap != nil {
		taskSpan.Set(trace.AttrSnapshotID, snap.ID)
	}

	rec := &Recorder{u: u, task: t, def: def, mu: &sync.Mutex{}}
	rec.status("", TaskRunning)
	rec.snapshot()
	if snap != nil {
		rec.event(EventSnapshot, 0, map[string]any{
			"snapshot_id": snap.ID,
			"version":     snap.Version,
			"checksum":    snap.Checksum,
		})
	}

	// 截止时间：到期强制收敛为 timeout。不这么做就只能等外部掐断连接，
	// 结果会被记成「用户取消」，超时率与取消率混在一起无法区分。
	parent := ctx
	if !t.DeadlineAt.IsZero() {
		if left := time.Until(t.DeadlineAt); left > 0 {
			var stopDeadline context.CancelFunc
			parent, stopDeadline = context.WithTimeout(ctx, left)
			defer stopDeadline()
		}
	}

	// 注册取消句柄
	runCtx, cancel := context.WithCancel(parent)
	u.runMu.Lock()
	u.cancels[t.ID] = cancel
	u.runMu.Unlock()
	defer func() {
		cancel()
		u.runMu.Lock()
		delete(u.cancels, t.ID)
		u.runMu.Unlock()
	}()

	// 续租：执行期间定期延长 lease_expires_at；续不上说明任务已被收回，
	// 立刻取消本次执行，避免两个执行体同时跑出重复的外部副作用。
	stopLease := u.startLeaseRenewal(runCtx, t.ID, cancel)
	defer stopLease()

	// 运行时二次授权：快照冻结的是配置，不是权限。任务恢复 / 重试期间
	// 归属或授权可能已变，执行前重新问一次「这个主体还能不能跑这个 Agent」。
	var runRes *RunResult
	runErr = u.authorizeTaskRuntime(ctx, t, def)
	if runErr == nil {
		runRes, runErr = u.runGraph(runCtx, def, t.Input, rec)
	}
	answer := ""
	if runRes != nil {
		answer = runRes.Answer
	}
	// 预算耗尽：不是系统故障，统一按「成功但未跑完」处理，并把用量写进事件流，
	// 前端任务详情能直接看到 budget_exceeded 与实际消耗。
	if errors.Is(runErr, budget.ErrExceeded) {
		runErr = nil
		if strings.TrimSpace(answer) == "" {
			answer = "已用尽本轮执行预算（budget.maxTokens / maxCalls），任务未产出结论。可提高配额后重跑。"
		}
		rec.event(EventError, rec.turn, map[string]any{
			"message":         "token 预算耗尽，任务提前收敛",
			"budget_exceeded": true,
			"budget":          budget.From(runCtx).Snapshot(),
		})
	} else if runRes != nil && runRes.BudgetExceeded {
		rec.event(EventThought, rec.turn, map[string]any{
			"budget_exceeded": true,
			"budget":          budget.From(runCtx).Snapshot(),
		})
	}

	now := time.Now()
	t.FinishedAt = now
	// 超时判定以任务自身的截止时间为准，不看 ctx：sync 调用方的 ctx 也可能有
	// 截止时间，那是「调用方失去耐心」，不该算成任务超时。
	timedOut := !t.DeadlineAt.IsZero() && !now.Before(t.DeadlineAt)
	if runErr != nil {
		// 人工审批闸门（O3）：命中 require_approval / 高危转人工 → 任务挂起。
		// 不写终态、不归档：任务留在任务池，批准后置回 pending 从 checkpoint 续跑。
		if errors.Is(runErr, ErrApprovalNeeded) {
			t.Status = TaskPaused
			t.ErrorMsg = "等待人工审批"
			t.FinishedAt = time.Time{}
			t.LeaseOwner, t.LeaseExpiresAt = "", time.Time{}
			rec.status(TaskRunning, TaskPaused)
			// running → paused 走 CAS：期间若已被取消，就不要再把状态改回 paused
			if ok, perr := u.repo.TransitionTask(u.baseCtx, t.ID, []string{TaskRunning}, t); perr == nil && !ok {
				rec.event(EventError, rec.turn, map[string]any{"message": "任务状态已被并发修改，放弃挂起"})
			}
			rec.snapshot()
			return "", runErr
		}
		// 停机导致的中断：不写终态。任务保持 running，下次启动由
		// recoverInterrupted 收回重排，避免被误标成「已取消」而丢失。
		// 租约保留：新进程启动时靠它（或直接扫 running）识别遗留任务。
		if u.draining.Load() && errors.Is(runErr, context.Canceled) {
			rec.event(EventError, rec.turn, map[string]any{"message": "服务停止，任务中断，将在重启后自动重跑"})
			rec.status(TaskRunning, TaskRunning)
			t.ErrorMsg = "服务停止，任务中断，将在重启后自动重跑"
			t.FinishedAt = time.Time{}
			_ = u.repo.SaveTask(u.baseCtx, t)
			rec.snapshot()
			return "", runErr
		}
		// 业务失败且调用方显式要求重试（max_attempts>1）：进 retry_wait，
		// 退避到期后由清扫协程置回 pending 重新入队。
		// 默认 max_attempts=0 不走这条路——大模型任务的失败多是确定性的，
		// 无脑重试只会把 token 烧在同一个坑里。
		if !timedOut && u.shouldRetryBusiness(t) && retryableBusinessError(runErr) {
			t.Status = TaskRetryWait
			t.ErrorMsg = runErr.Error()
			t.Progress = 0
			t.FinishedAt = time.Time{}
			t.LeaseOwner, t.LeaseExpiresAt = "", time.Time{}
			t.NextAttemptAt = now.Add(RetryBackoff(t.Attempt))
			rec.event(EventError, rec.turn, map[string]any{
				"message":          "第 " + strconv.Itoa(t.Attempt) + " 次尝试失败，等待重试",
				"retry_wait_until": t.NextAttemptAt.Format(time.RFC3339),
				"attempt":          t.Attempt,
			})
			rec.status(TaskRunning, TaskRetryWait)
			if ok, terr := u.repo.TransitionTask(u.baseCtx, t.ID, []string{TaskRunning}, t); terr != nil || !ok {
				if terr != nil {
					u.warn("任务重试等待落库失败", "task", t.ID, "err", terr)
				}
			}
			// checkpoint 保留：重排后从断点续跑，不重头再来
			rec.snapshot()
			return "", runErr
		}
		switch {
		case timedOut:
			t.Status = TaskTimeout
			t.ErrorMsg = "任务执行超时（超过 " + u.opt.TaskTimeout.String() + "）"
		case errors.Is(runCtx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			t.Status = TaskCanceled
			t.ErrorMsg = "任务被取消"
		default:
			t.Status = TaskFailed
			t.ErrorMsg = runErr.Error()
		}
		t.FinishedAt = now
		t.LeaseOwner, t.LeaseExpiresAt = "", time.Time{}
		rec.event(EventError, rec.turn, map[string]any{"message": t.ErrorMsg})
		rec.status(TaskRunning, t.Status)
		t.Progress = 100
		// running → 终态走 CAS：审批驳回 / 挂起期取消可能先一步落了终态，
		// 后到的 worker 只补事件、不覆盖别人的结论
		if ok, terr := u.repo.TransitionTask(u.baseCtx, t.ID, []string{TaskRunning}, t); terr != nil || !ok {
			if terr != nil {
				u.warn("任务终态落库失败", "task", t.ID, "err", terr)
			}
		}
		u.repo.TrimCheckpoints(u.baseCtx, t.ID, 5)
		rec.snapshot()
		return "", runErr
	}
	t.Status = TaskSucceeded
	t.Result = answer
	t.Progress = 100
	t.LeaseOwner, t.LeaseExpiresAt = "", time.Time{}
	rec.status(TaskRunning, TaskSucceeded)
	// 同上：只有「当前仍是 running」才允许落成功终态
	if ok, terr := u.repo.TransitionTask(u.baseCtx, t.ID, []string{TaskRunning}, t); terr != nil || !ok {
		if terr != nil {
			u.warn("任务终态落库失败", "task", t.ID, "err", terr)
		}
	}
	u.repo.TrimCheckpoints(u.baseCtx, t.ID, 5)
	rec.snapshot()
	return answer, nil
}
