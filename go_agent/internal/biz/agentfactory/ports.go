package agentfactory

import (
	"context"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/skill"
)

// 端口（接口定义在 biz，实现在 data / 其他 biz 包）。
// 依赖注入见 internal/data/data.go 与 internal/biz/biz.go 的 wire.Bind。

// Repo MySQL 持久化：Agent 定义、任务、事件
type Repo interface {
	// —— Agent 定义 ——
	ListDefs(ctx context.Context, f DefFilter) ([]AgentDef, error)
	GetDef(ctx context.Context, id uint64) (*AgentDef, error)
	GetDefByKey(ctx context.Context, key string) (*AgentDef, error)
	CountDefs(ctx context.Context) (int64, error)
	CreateDef(ctx context.Context, def *AgentDef) error
	UpdateDef(ctx context.Context, def *AgentDef) error
	DeleteDef(ctx context.Context, id uint64) error
	SetDefEnabled(ctx context.Context, id uint64, enabled bool) error
	MarkDefsVectorized(ctx context.Context, ids []uint64, ok bool) error
	// UpdateDefSchedule 只更新调度时间（下次/上次运行），不推进定义版本
	UpdateDefSchedule(ctx context.Context, id uint64, next, last time.Time) error

	// —— Agent 配置快照（不可变，只增不改） ——

	CreateSnapshot(ctx context.Context, s *Snapshot) error
	GetSnapshot(ctx context.Context, id uint64) (*Snapshot, error)
	// LatestSnapshot 某定义的最新快照（按 id 倒序取一条）；没有返回 (nil, nil)
	LatestSnapshot(ctx context.Context, agentDefID uint64) (*Snapshot, error)
	ListSnapshots(ctx context.Context, agentDefID uint64, limit int) ([]Snapshot, error)

	// —— 任务 ——
	CreateTask(ctx context.Context, t *Task) error
	SaveTask(ctx context.Context, t *Task) error
	// ClaimTask 原子认领：pending → running，并写入执行租约（持有者 + 到期时间）。
	// lease.Owner 为空时只做状态迁移，兼容未启用执行体注册的单机/测试态。
	ClaimTask(ctx context.Context, id uint64, lease Lease) (bool, error)
	// RenewLease 续租：仅当 id 仍为 running 且持有者匹配时生效，返回是否续上
	RenewLease(ctx context.Context, id uint64, owner string, expiresAt time.Time) (bool, error)
	// ListExpiredLeases 租约已过期但仍处于 running 的任务（执行体失联）。
	// 由清扫协程收回：未超重试上限 → retry_wait，超限 → failed + 死信原因。
	ListExpiredLeases(ctx context.Context, now time.Time, limit int) ([]Task, error)
	// ListOverdueTasks 超过 DeadlineAt 仍未收敛的 running 任务
	ListOverdueTasks(ctx context.Context, now time.Time, limit int) ([]Task, error)
	// PromoteRetryWait 退避到期的 retry_wait 任务置回 pending，返回需重新入队的 ID
	PromoteRetryWait(ctx context.Context, now time.Time, limit int) ([]uint64, error)
	// ListDeadLetters 死信任务（已归档且 dead_letter_reason 非空），按 ID 倒序
	ListDeadLetters(ctx context.Context, tenantID string, limit int) ([]Task, error)
	ListTasks(ctx context.Context, f TaskFilter) ([]Task, error)
	GetTask(ctx context.Context, id uint64) (*Task, error)
	// CountTasksByStatus 各状态任务数（stats 用；任务池 + 任务历史合并）
	CountTasksByStatus(ctx context.Context) (map[string]int64, error)
	// FinishTask 任务到达终态：同一事务内把任务从任务池（agent_tasks）
	// 迁移到任务历史表（agent_task_history），保证任一时刻任务只存在一张表中
	FinishTask(ctx context.Context, t *Task) error
	// TransitionTask 条件迁移（CAS）：仅当库内状态在 from 中时才把 t 的状态与
	// 终态字段写进去；返回 false 表示状态已被别人改过，调用方应重新读取再决策。
	// t.Status 为终态时同样走「任务池 → 历史表」的两表迁移。
	TransitionTask(ctx context.Context, id uint64, from []string, t *Task) (bool, error)
	// ReviveTask 重试归档任务：把任务历史表中的终态任务复活回任务池
	// （重置 pending），同一事务内完成「插任务池 + 删历史」
	ReviveTask(ctx context.Context, t *Task) error

	// —— checkpoint（步骤级快照，O2）——
	// SaveCheckpoint 追加一条快照；排序以自增 ID 为准，Seq 仅作任务内逻辑序号
	SaveCheckpoint(ctx context.Context, cp *TaskCheckpoint) error
	// LatestCheckpoint 取该任务最新一条快照（无则返回 nil, nil）
	LatestCheckpoint(ctx context.Context, taskID uint64) (*TaskCheckpoint, error)
	// TrimCheckpoints 只保留最近 keep 条（keep=0 全删：重试/驳回时清空）
	TrimCheckpoints(ctx context.Context, taskID uint64, keep int) error

	// —— 事件（trace） ——
	AddEvent(ctx context.Context, e *TaskEvent) error
	ListEvents(ctx context.Context, taskID uint64, limit int) ([]TaskEvent, error)
}

// WorkerRepo 执行体注册表（worker_instances）：注册 / 心跳 / 下线 / 失联判定。
//
// 与 Repo 分开是因为它只服务执行链路治理：多副本部署时靠心跳区分「活着但因为
// 空闲没干活」和「进程已经没了、它手上的任务该收回来重跑」。
type WorkerRepo interface {
	// UpsertWorker 注册或刷新执行体（同 key 重复注册不报错）
	UpsertWorker(ctx context.Context, w Worker) error
	// HeartbeatWorker 心跳：刷新 heartbeat_at/expires_at/当前在执行任务数
	HeartbeatWorker(ctx context.Context, key, status string, running int, expiresAt time.Time) error
	// SetWorkerStatus 只改状态（draining / offline）
	SetWorkerStatus(ctx context.Context, key, status string) error
	// ListWorkers 全部执行体（在线与历史实例都返回，由调用方按 expires_at 判活）
	ListWorkers(ctx context.Context) ([]Worker, error)
	// MarkStaleWorkers 心跳过期的执行体置 offline，返回本次下线数量
	MarkStaleWorkers(ctx context.Context, now time.Time) (int64, error)
}

// VectorStore Milvus Agent 向量注册表（语义召回）
type VectorStore interface {
	EnsureCollection(ctx context.Context) error
	Upsert(ctx context.Context, defs []AgentDef, vecs [][]float32) error
	Search(ctx context.Context, vec []float32, topK int, f VectorFilter) ([]VectorHit, error)
	Delete(ctx context.Context, ids []uint64) error
	Count(ctx context.Context) (int64, error)
}

// Cache Redis：Agent 定义热缓存 + 任务快照（实时状态轮询）+ 任务队列
type Cache interface {
	GetDef(ctx context.Context, id uint64) (*AgentDef, bool)
	SetDef(ctx context.Context, def *AgentDef)
	DeleteDefs(ctx context.Context, ids []uint64)

	// GetTask 任务快照（只缓存活跃任务，终态直接读库）
	GetTask(ctx context.Context, id uint64) (*Task, bool)
	SetTask(ctx context.Context, t *Task)

	// EnqueueTask 任务入队（LPUSH）
	EnqueueTask(ctx context.Context, id uint64) error
	// DequeueTask 出队（BRPOP，timeout 到期返回 false）
	DequeueTask(ctx context.Context, timeout time.Duration) (uint64, bool)
}

// TaskLock Redis 分布式锁（Redlock）：保证同一任务不会被多个执行体同时触发。
// 实现需满足：加锁带随机 token（SET NX PX）、释放/续期校验 token（Lua CAS）、
// 多节点 Quorum（多数节点成功才算获取成功）。
type TaskLock interface {
	// Acquire 尝试获取任务锁，成功返回随机 token（释放/续期凭据）
	Acquire(ctx context.Context, taskID uint64, ttl time.Duration) (token string, ok bool)
	// Release 释放锁（token 不匹配则忽略，防止误删他人锁）
	Release(ctx context.Context, taskID uint64, token string)
	// Extend 续期（token 匹配才续），返回是否续期成功
	Extend(ctx context.Context, taskID uint64, token string, ttl time.Duration) bool
}

// Chatter LLM 对话（绑定 llm.Manager）
type Chatter interface {
	Chat(ctx context.Context, system, user string) (string, error)
	ChatTemp(ctx context.Context, system, user string, temperature float64) (string, error)
	// ChatSource 指定数据源对话：source 为 llm 数据源名，未命中或调用失败由实现方回退默认链路
	ChatSource(ctx context.Context, source, system, user string, temperature float64) (string, error)
	// ChatOpts 统一参数通道：JSON 输出模式（数据源不支持时自动降级普通文本）、
	// 补全长度上限、指定数据源/场景与温度。
	ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error)
	Model() string
}

// Embedder 向量化（绑定 embedder.EmbeddingClient）
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}

// ToolPort MCP 工具供给（绑定 mcp.UseCase）：能力目录 + 调用
// SkillPort 技能供给：目录列出启用技能，调用时读取 SKILL.md 内容。
// 方法签名与 skill.UseCase 一致，wire.Bind 直接绑 *skill.UseCase。
type SkillPort interface {
	List(ctx context.Context, f skill.Filter) ([]skill.Skill, error)
	Get(ctx context.Context, name string) (*skill.Skill, error)
}

type ToolPort interface {
	ListTools(ctx context.Context, f mcp.ToolFilter) ([]mcp.Tool, error)
	CallTool(ctx context.Context, toolID uint64, uniqueName string, args map[string]any) (*mcp.CallResult, error)
}

// MediaPort 媒体生成供给（绑定 llm.MediaManager）：
// llm:image / llm:video / llm:video_status 三个能力的实现后端。
// 为 nil 时（最小部署未装配媒体生成）目录仍列出能力，调用时给出明确报错。
type MediaPort interface {
	GenerateImage(ctx context.Context, in *llm.ImageGenInput) (*llm.ImageGenRecord, error)
	SubmitVideo(ctx context.Context, in *llm.VideoGenInput) (*llm.VideoGenRecord, error)
	GetVideo(ctx context.Context, id uint64) (*llm.VideoGenRecord, error)
	// MediaCatalog 可用的图像 / 视频数据源与模型（能力描述用，best effort）
	MediaCatalog(ctx context.Context) (image, video []llm.MediaModelInfo, err error)
}

// PromptSaver 提示词工程端口：LLM 创建 Agent 成功后，把角色提示词沉淀为
// 提示词工程模板（实现：prompt.UseCase.SaveAgentPrompt）。
type PromptSaver interface {
	SaveAgentPrompt(ctx context.Context, agentKey, agentName, content string) error
}
