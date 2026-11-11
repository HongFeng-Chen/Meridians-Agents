package models

import (
	"time"

	"gorm.io/gorm"
)

// AgentDefPO agent_definitions：Agent 的可配置「定义」（低代码数据，不是代码）
type AgentDefPO struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	// AgentKey 全局唯一标识（委派能力名 = agent:<agent_key>）
	AgentKey string `gorm:"column:agent_key;size:64;not null;uniqueIndex:uk_agent_defs_key" json:"agent_key"`
	Name     string `gorm:"column:name;size:128;not null" json:"name"`
	// Description 给 LLM 看的职责描述（语义召回向量化来源）
	Description string `gorm:"column:description;type:text" json:"description"`
	// System 角色提示词
	System string `gorm:"column:system;type:text" json:"system"`
	// LLMModel 指定模型（空 = 平台默认数据源）
	LLMModel string `gorm:"column:llm_model;size:128;not null;default:''" json:"llm_model"`
	// LLMSource 默认模型数据源（空 = 平台默认），调用失败回退 default
	LLMSource string `gorm:"column:llm_source;size:64;not null;default:''" json:"llm_source"`
	// GraphType 执行图：auto | react | simple | ...
	GraphType string `gorm:"column:graph_type;size:32;not null;default:'react'" json:"graph_type"`
	// AutoCompose 自动装配：每次任务前由 LLM 判定架构与成员，缺成员时自动创建子 Agent
	AutoCompose bool `gorm:"column:auto_compose;not null;default:false" json:"auto_compose"`
	// Capabilities 能力白名单，JSON 数组字符串（空集必须写 []，MySQL JSON 列拒收空串）
	Capabilities string `gorm:"column:capabilities;type:json" json:"capabilities"`
	// Middlewares 中间件，JSON 数组字符串
	Middlewares string `gorm:"column:middlewares;type:json" json:"middlewares"`
	MaxTurns    int    `gorm:"column:max_turns;not null;default:12" json:"max_turns"`
	// Role 角色：root | sub | supervisor | risk
	Role string `gorm:"column:role;size:16;not null;default:'root';index:idx_agent_defs_role" json:"role"`
	// Orchestration 成员编排：auto | serial | parallel（仅 root 生效）
	Orchestration string `gorm:"column:orchestration;size:16;not null;default:'auto'" json:"orchestration"`
	// Members 编排成员 agent_key 列表，JSON 数组字符串
	Members string `gorm:"column:members;type:json" json:"members"`
	// Flow 编排画布布局（节点坐标），JSON 对象字符串，仅前端渲染用
	Flow string `gorm:"column:flow_json;type:text" json:"flow"`
	// SupervisorKey 监察 Agent（role=supervisor 的 agent_key）
	SupervisorKey string `gorm:"column:supervisor_key;size:64;not null;default:''" json:"supervisor_key"`
	// SupervisionScope 监察范围（supervisor 角色使用），JSON 数组字符串
	SupervisionScope string `gorm:"column:supervision_scope;type:json" json:"supervision_scope"`
	// RiskAgentKey 风险评估 Agent（role=risk 的 agent_key），仅 MCP 调用前生效
	RiskAgentKey string `gorm:"column:risk_agent_key;size:64;not null;default:''" json:"risk_agent_key"`
	// RequireApproval 需人工审批的能力前缀，JSON 数组字符串（支持尾部 * 通配）
	RequireApproval string `gorm:"column:require_approval;type:json" json:"require_approval"`
	// ApprovalTimeoutHours 审批超时（小时），0 = 默认 72h
	ApprovalTimeoutHours int `gorm:"column:approval_timeout_hours;not null;default:0" json:"approval_timeout_hours"`
	// RiskPauseLevel 风险转人工阈值：none（默认）| high
	RiskPauseLevel string `gorm:"column:risk_pause_level;size:16;not null;default:'none'" json:"risk_pause_level"`
	// AutoRun 后台自动运行（cron）
	AutoRun bool `gorm:"column:auto_run;not null;default:false;index:idx_agent_defs_autorun" json:"auto_run"`
	// CronExpr 5 段 cron 表达式
	CronExpr string `gorm:"column:cron_expr;size:64;not null;default:''" json:"cron_expr"`
	// CronInput 定时运行的固定输入
	CronInput string `gorm:"column:cron_input;type:text" json:"cron_input"`
	// NextRunAt 下次自动运行时间（调度器维护；NULL 表示未排期）
	NextRunAt *time.Time `gorm:"column:next_run_at" json:"next_run_at"`
	// LastRunAt 上次自动运行时间
	LastRunAt *time.Time `gorm:"column:last_run_at" json:"last_run_at"`
	// Sandbox 沙箱灰度：禁止委派其他 Agent
	Sandbox bool `gorm:"column:is_sandbox;not null;default:false" json:"is_sandbox"`
	// Source 来源：manual | llm
	Source string `gorm:"column:source;size:16;not null;default:'manual';index:idx_agent_defs_source" json:"source"`
	// Version 定义版本（每次更新 +1，为回滚预留）
	Version int `gorm:"column:version;not null;default:1" json:"version"`
	// Enabled 启用状态
	Enabled bool `gorm:"column:is_enabled;not null;default:true;index:idx_agent_defs_enabled" json:"is_enabled"`
	// Vectorized 是否已写入向量注册表
	Vectorized bool `gorm:"column:is_vectorized;not null;default:false" json:"is_vectorized"`

	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index:idx_agent_defs_deleted" json:"-"`
}

func (AgentDefPO) TableName() string { return "agent_definitions" }

// AgentSnapshotPO agent_snapshots：Agent 定义的不可变配置快照。
//
// 只增不改：任务提交时钉住 snapshot_id，执行时按快照加载 Prompt / 能力 / 模型，
// 定义后续怎么改都不影响已提交任务；审计也能回答「当时跑的是哪一版配置」。
type AgentSnapshotPO struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	AgentDefID uint64 `gorm:"column:agent_def_id;not null;index:idx_agent_snapshots_def" json:"agent_def_id"`
	AgentKey   string `gorm:"column:agent_key;size:64;not null;index:idx_agent_snapshots_key" json:"agent_key"`
	// Version 对应 AgentDef.Version（内容未变的重复保存不会产生新行）
	Version int `gorm:"column:version;not null;default:0" json:"version"`
	// Checksum 配置摘要（sha256，排除运行态字段）；与最新一条相同即复用
	Checksum string `gorm:"column:checksum;size:64;not null;index:idx_agent_snapshots_sum" json:"checksum"`
	// Config 冻结的完整 AgentDef JSON
	Config string `gorm:"column:config;type:mediumtext" json:"config"`
	// Deps 依赖清单 JSON：{"agents":[],"tools":[],"skills":[]}
	Deps      string    `gorm:"column:deps;type:text" json:"deps"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 表名
func (AgentSnapshotPO) TableName() string { return "agent_snapshots" }

// AgentTaskPO agent_tasks：异步任务（worker 的基本单位；parent_task_id 形成多 Agent 协作链路树）
type AgentTaskPO struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	AgentDefID uint64 `gorm:"column:agent_def_id;not null;index:idx_agent_tasks_agent" json:"agent_def_id"`
	// AgentKey 冗余：定义删除后任务历史仍可读
	AgentKey string `gorm:"column:agent_key;size:64;not null;index:idx_agent_tasks_key" json:"agent_key"`
	Title    string `gorm:"column:title;size:128;not null" json:"title"`
	Input    string `gorm:"column:input;type:text" json:"input"`
	// Status: pending | running | retry_wait | paused | succeeded | failed | canceled | timeout
	Status string `gorm:"column:status;size:16;not null;default:'pending';index:idx_agent_tasks_status" json:"status"`
	// Progress 0~100
	Progress int    `gorm:"column:progress;not null;default:0" json:"progress"`
	Result   string `gorm:"column:result;type:mediumtext" json:"result"`
	ErrorMsg string `gorm:"column:error_msg;type:text" json:"error_msg"`
	// ParentTaskID 父任务（0 = 根任务）
	ParentTaskID uint64 `gorm:"column:parent_task_id;not null;default:0;index:idx_agent_tasks_parent" json:"parent_task_id"`
	// Depth 委派深度（根=0）
	Depth int `gorm:"column:depth;not null;default:0" json:"depth"`
	// ConvID 来源会话（会话编排调用 Agent 时写入；任务链路与会话链路共用）
	ConvID string `gorm:"column:conv_id;size:64;not null;default:'';index:idx_agent_tasks_conv" json:"conv_id"`
	// UserID 任务归属用户（隔离键）：任务中心列表与取消/重试只允许归属人操作。
	// 默认 'default' 兼容认证未启用时的单机开发态与历史数据。
	UserID string `gorm:"column:user_id;size:128;not null;default:'default';index:idx_agent_tasks_user" json:"user_id"`
	// TenantID 租户隔离键：租户之间互不可见，即使 user_id 相同。
	// 默认 'public'：加列时历史行按默认值回填，不会变成「谁都能看」。
	TenantID string `gorm:"column:tenant_id;size:128;not null;default:'public';index:idx_agent_tasks_tenant" json:"tenant_id"`
	// SnapshotID 提交时钉住的配置快照（0 = 无快照，执行时回退当前定义）
	SnapshotID uint64 `gorm:"column:snapshot_id;not null;default:0" json:"snapshot_id"`
	// DelegatedCaps 委派上下文（JSON 数组）：NULL 表示未受限，'[]' 表示受限且全禁。
	// 必须区分 NULL 与 '[]'：前者是「父级没约束」，后者是「父级明确一项都不给」。
	DelegatedCaps *string `gorm:"column:delegated_caps;type:text" json:"delegated_caps"`

	// Attempt 已认领次数（含当前这次）：租约回收时判断是否还有重试机会
	Attempt int `gorm:"column:attempt;not null;default:0" json:"attempt"`
	// MaxAttempts 业务失败重试上限（0 = 不重试）
	MaxAttempts int `gorm:"column:max_attempts;not null;default:0" json:"max_attempts"`
	// LeaseOwner 持有执行租约的执行体 key（''=无租约）
	LeaseOwner string `gorm:"column:lease_owner;size:128;not null;default:'';index:idx_agent_tasks_lease" json:"lease_owner"`
	// LeaseExpiresAt 租约到期时间；执行期间自动续租，过期即视为执行体失联
	LeaseExpiresAt *time.Time `gorm:"column:lease_expires_at;index:idx_agent_tasks_lease_exp" json:"lease_expires_at"`
	// DeadlineAt 执行截止时间：认领时按配置计算，超时落 timeout 终态
	DeadlineAt *time.Time `gorm:"column:deadline_at" json:"deadline_at"`
	// NextAttemptAt 重试等待到期时间（status=retry_wait 时有效）
	NextAttemptAt *time.Time `gorm:"column:next_attempt_at;index:idx_agent_tasks_next_attempt" json:"next_attempt_at"`
	// DeadLetterReason 死信原因（重试次数用尽仍失败）：'' 表示不是死信
	DeadLetterReason string `gorm:"column:dead_letter_reason;size:64;not null;default:''" json:"dead_letter_reason"`
	// TraceID OpenTelemetry Trace（32 位十六进制）：任务详情跳转 span 树用。
	// 每次执行（含重试）重新生成，只保留最后一次，历史尝试在 trace 存储里。
	TraceID string `gorm:"column:trace_id;size:32;not null;default:'';index:idx_agent_tasks_trace" json:"trace_id"`

	CreatedAt time.Time  `gorm:"column:created_at;not null;index:idx_agent_tasks_created" json:"created_at"`
	StartedAt *time.Time `gorm:"column:started_at" json:"started_at"`
	// FinishedAt 终态时间；*time.Time 让未开始的任务写 NULL（MySQL 拒绝零值 datetime）
	FinishedAt *time.Time     `gorm:"column:finished_at" json:"finished_at"`
	DeletedAt  gorm.DeletedAt `gorm:"column:deleted_at;index:idx_agent_tasks_deleted" json:"-"`
}

func (AgentTaskPO) TableName() string { return "agent_tasks" }

// AgentTaskHistoryPO agent_task_history：任务历史表（归档区）。
//
// 任务池（agent_tasks）只保存 pending/running 活跃任务；任务到达终态
// （succeeded/failed/canceled）时由 FinishTask 在同一事务内迁入本表并从
// 池中删除，任一时刻同一条任务只存在一张表中。字段与任务池一致，
// 另加 archived_at 记录归档时间。
type AgentTaskHistoryPO struct {
	ID         uint64 `gorm:"primaryKey" json:"id"` // 沿用任务池分配的 ID，不重新自增
	AgentDefID uint64 `gorm:"column:agent_def_id;not null;index:idx_agent_task_history_agent" json:"agent_def_id"`
	AgentKey   string `gorm:"column:agent_key;size:64;not null;index:idx_agent_task_history_key" json:"agent_key"`
	Title      string `gorm:"column:title;size:128;not null" json:"title"`
	Input      string `gorm:"column:input;type:text" json:"input"`
	// Status 仅终态：succeeded | failed | canceled
	Status   string `gorm:"column:status;size:16;not null;index:idx_agent_task_history_status" json:"status"`
	Progress int    `gorm:"column:progress;not null;default:0" json:"progress"`
	Result   string `gorm:"column:result;type:mediumtext" json:"result"`
	ErrorMsg string `gorm:"column:error_msg;type:text" json:"error_msg"`
	// ParentTaskID 父任务（0 = 根任务）；父任务可能已在历史表
	ParentTaskID uint64 `gorm:"column:parent_task_id;not null;default:0;index:idx_agent_task_history_parent" json:"parent_task_id"`
	Depth        int    `gorm:"column:depth;not null;default:0" json:"depth"`
	// ConvID 来源会话（与任务池同义）
	ConvID string `gorm:"column:conv_id;size:64;not null;default:'';index:idx_agent_task_history_conv" json:"conv_id"`
	// UserID 任务归属用户（与任务池同义，归档时原样迁移）
	UserID string `gorm:"column:user_id;size:128;not null;default:'default';index:idx_agent_task_history_user" json:"user_id"`
	// TenantID 租户隔离键（归档时随行迁移）
	TenantID string `gorm:"column:tenant_id;size:128;not null;default:'public';index:idx_agent_task_history_tenant" json:"tenant_id"`
	// SnapshotID 执行所用的配置快照（归档保留，事后可追溯）
	SnapshotID uint64 `gorm:"column:snapshot_id;not null;default:0" json:"snapshot_id"`
	// DelegatedCaps 委派上下文（JSON 数组；NULL = 未受限，'[]' = 受限全禁）。
	// 归档必须保留：否则「这个子任务当时被下放了哪些能力」事后无从查证，
	// 而它正是权限事故复盘时最需要的那一列。
	DelegatedCaps *string `gorm:"column:delegated_caps;type:text" json:"delegated_caps"`
	// Attempt 归档时的已认领次数（死信排查用；任务池列不随归档迁移，这里留存）
	Attempt int `gorm:"column:attempt;not null;default:0" json:"attempt"`
	// MaxAttempts 业务重试上限（归档快照）
	MaxAttempts int `gorm:"column:max_attempts;not null;default:0" json:"max_attempts"`
	// DeadLetterReason 死信原因（''=非死信）：重试次数用尽的任务在这里留痕
	DeadLetterReason string `gorm:"column:dead_letter_reason;size:64;not null;default:'';index:idx_agent_task_history_dlq" json:"dead_letter_reason"`
	// TraceID 归档时的执行 Trace（与任务池同义，保留供事后追查）
	TraceID string `gorm:"column:trace_id;size:32;not null;default:''" json:"trace_id"`

	CreatedAt time.Time  `gorm:"column:created_at;not null;index:idx_agent_task_history_created" json:"created_at"`
	StartedAt *time.Time `gorm:"column:started_at" json:"started_at"`
	// FinishedAt 终态时间（历史表必填）
	FinishedAt *time.Time `gorm:"column:finished_at;not null" json:"finished_at"`
	// ArchivedAt 归档时间（任务池 → 历史表迁移时刻）
	ArchivedAt time.Time `gorm:"column:archived_at;not null;index:idx_agent_task_history_archived" json:"archived_at"`
}

func (AgentTaskHistoryPO) TableName() string { return "agent_task_history" }

// AgentTaskEventPO agent_task_events：任务事件流（trace 链路检索单元）
type AgentTaskEventPO struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	// TaskID 所属任务
	TaskID uint64 `gorm:"column:task_id;not null;index:idx_agent_task_events_task" json:"task_id"`
	// Turn 执行轮次（0 = 状态类事件）
	Turn int `gorm:"column:turn;not null;default:0" json:"turn"`
	// Type 事件类型：status/thought/tool_call/tool_result/delegate/final/error
	Type string `gorm:"column:type;size:32;not null;index:idx_agent_task_events_type" json:"type"`
	// Payload JSON 字符串
	Payload string `gorm:"column:payload;type:text" json:"payload"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (AgentTaskEventPO) TableName() string { return "agent_task_events" }

// AgentTaskCheckpointPO agent_task_checkpoints：任务步骤级快照（O2 续跑）。
// 每个步骤边界（计划步骤 / 波次 / 辩论轮次 / 审批挂起点）追加一条，
// 恢复时按 id 取最新一条反序列化 RunState。
type AgentTaskCheckpointPO struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	// TaskID 所属任务
	TaskID uint64 `gorm:"column:task_id;not null;index:idx_agent_task_checkpoints_task" json:"task_id"`
	// Seq 任务内逻辑序号（恢复路由不依赖它，按 ID 取最新）
	Seq       int    `gorm:"column:seq;not null;default:0" json:"seq"`
	GraphType string `gorm:"column:graph_type;size:32;not null;default:''" json:"graph_type"`
	// Cursor 恢复点描述（JSON）：{"kind":"plan_step|wave|round|loop|approval","index":n,"ability":""}
	Cursor string `gorm:"column:cursor;size:256;not null;default:''" json:"cursor"`
	// State RunState JSON
	State string `gorm:"column:state;type:mediumtext" json:"state"`
	// Tokens 落盘时已消耗 token（观测用）
	Tokens    int64     `gorm:"column:tokens;not null;default:0" json:"tokens"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (AgentTaskCheckpointPO) TableName() string { return "agent_task_checkpoints" }

// WorkerInstancePO worker_instances：执行体注册表（一个进程一行）。
//
// 存在的意义只有一个：区分「活着但空闲」和「进程已经没了」。任务租约负责
// 回收某条任务，这张表负责回答「谁还活着、谁手上压了多少任务」，扩容决策与
// 事故复盘都看它。心跳过期即判失联，不依赖进程优雅退出（OOM / 断电没有退出钩子）。
type WorkerInstancePO struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	// WorkerKey 实例唯一标识（hostname-pid-随机后缀；重启即新实例）
	WorkerKey string `gorm:"column:worker_key;size:160;not null;uniqueIndex:uk_worker_instances_key" json:"worker_key"`
	Host      string `gorm:"column:host;size:128;not null;default:''" json:"host"`
	// Capacity 并发容量（worker 协程数）
	Capacity int `gorm:"column:capacity;not null;default:0" json:"capacity"`
	// Running 当前正在执行的任务数（心跳刷新）
	Running int `gorm:"column:running;not null;default:0" json:"running"`
	// Status online | draining | offline
	Status string `gorm:"column:status;size:16;not null;default:'online';index:idx_worker_instances_status" json:"status"`
	// StartedAt 进程启动时间
	StartedAt time.Time `gorm:"column:started_at;not null" json:"started_at"`
	// HeartbeatAt 最近一次心跳
	HeartbeatAt time.Time `gorm:"column:heartbeat_at;not null" json:"heartbeat_at"`
	// ExpiresAt 心跳租约到期时间：超过即判失联
	ExpiresAt *time.Time `gorm:"column:expires_at;index:idx_worker_instances_expires" json:"expires_at"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (WorkerInstancePO) TableName() string { return "worker_instances" }
