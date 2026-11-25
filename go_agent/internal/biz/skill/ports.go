package skill

import (
	"context"
	"time"
)

// Repo 技能仓库（MySQL 实现，位于 data/db/repo）。
// Save 为 upsert：技能主表按 name 覆盖，文件表整体替换。
type Repo interface {
	List(ctx context.Context, f Filter) ([]Skill, error)
	// Get 返回完整技能（含 content 与 files）
	Get(ctx context.Context, name string) (*Skill, error)
	Save(ctx context.Context, s *Skill) error
	Delete(ctx context.Context, name string) error
	SetEnabled(ctx context.Context, name string, enabled bool) error
	// SetExecutionSpec 只改执行配置（不动内容、不改 installed_at：
	// 切换 local/remote 是运维动作，不属于「重新安装」）
	SetExecutionSpec(ctx context.Context, name string, spec ExecutionSpec) error
	Stats(ctx context.Context) (Stats, error)

	ListHubs(ctx context.Context) ([]Hub, error)
	GetHub(ctx context.Context, name string) (*Hub, error)
	SaveHub(ctx context.Context, h *Hub) error
	DeleteHub(ctx context.Context, name string) error
	SetDefaultHub(ctx context.Context, name string) error
}

// HubClient 技能市场客户端（HTTP 实现，位于 data/third_service/skillhub）。
type HubClient interface {
	Search(ctx context.Context, hub Hub, keyword string, limit int) ([]HubSkill, error)
	// Fetch 拉取技能包；version 为空表示最新版
	Fetch(ctx context.Context, hub Hub, name, version string) (*Skill, error)
	// FetchURL 直接从 URL 拉技能包（zip / SKILL.md / JSON 包），用于「千问技能包」这类
	// 没有市场 API、只给分享链接的分发方式；token 为空表示不带鉴权。
	FetchURL(ctx context.Context, rawURL, token string) (*Skill, error)
}

// RuntimeStore 运行时状态存储（MySQL 实现，位于 data/db/repo/skill_runtime.go）。
//
// 拆成独立接口而不是塞进 Repo：技能定义是「配置面」，实例与执行记录是
// 「运行面」——两者的读写频率、保留策略、故障影响完全不同，
// 合成一个接口只会让技能列表接口被迫依赖执行表。
type RuntimeStore interface {
	// —— Worker 实例（注册中心） ——
	SaveInstance(ctx context.Context, in Instance) (*Instance, error)
	ListInstances(ctx context.Context, skillName string) ([]Instance, error)
	GetInstance(ctx context.Context, skillName, id string) (*Instance, error)
	DeleteInstance(ctx context.Context, skillName, id string) error
	// Heartbeat 更新心跳与在途数（实例不存在时返回 ErrNoWorker）
	Heartbeat(ctx context.Context, hb InstanceHeartbeat) error
	// RecordResult 记录一次调用结果：成功清零连续失败并更新 EWMA 延迟，
	// 失败累加连续失败次数（熔断依据）
	RecordResult(ctx context.Context, skillName, instanceID string, ok bool, latencyMS int64, errMsg string) error

	// —— 执行记录（账本 + 异步队列） ——
	CreateExecution(ctx context.Context, e *Execution) error
	GetExecution(ctx context.Context, id string) (*Execution, error)
	UpdateExecution(ctx context.Context, e *Execution) error
	ListExecutions(ctx context.Context, f ExecutionFilter) ([]Execution, error)
	// FindExecutionByKey 幂等键查已完成记录（只回成功的）。
	// tenantID 必须参与过滤：幂等键由调用方给，两个租户撞同一个键
	// （比如都填 "1"）时不能把别家的结果回放出去。
	FindExecutionByKey(ctx context.Context, tenantID, skillName, key string) (*Execution, error)
	// ClaimQueued 原子领取待执行任务（多副本安全：SKIP LOCKED + 租约）
	ClaimQueued(ctx context.Context, limit int, lease time.Duration) ([]Execution, error)
	// RequeueStale 把租约过期的 running 记录退回 queued（派发进程崩溃的兜底）
	RequeueStale(ctx context.Context, before time.Time, limit int) (int, error)
	// PurgeExecutions 清理过期终态记录（保留策略）
	PurgeExecutions(ctx context.Context, before time.Time, limit int) (int, error)
}

// WorkerClient Skill Worker 客户端（HTTP 实现，位于 data/third_service/skillworker）。
//
// 只认「实例 + 标准信封」，不认具体语言：Python / Go / Java 的 Worker
// 只要实现 POST /execute 就能接入，运行时不需要为每种语言写适配器。
type WorkerClient interface {
	Execute(ctx context.Context, inst Instance, req WorkerRequest) (*WorkerResponse, error)
}
