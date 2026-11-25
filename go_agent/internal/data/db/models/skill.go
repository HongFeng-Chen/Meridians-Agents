package models

import "time"

// SkillPO 技能主表（skills）：content 存 SKILL.md 正文
type SkillPO struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement"`
	Name        string `gorm:"column:name;size:64;not null;uniqueIndex:uk_skills_name"`
	Version     string `gorm:"column:version;size:32;not null;default:'1.0.0'"`
	Title       string `gorm:"column:title;size:191;not null;default:''"`
	Description string `gorm:"column:description;size:1024;not null;default:''"`
	Category    string `gorm:"column:category;size:64;not null;default:'';index:idx_skills_category"`
	Tags        string `gorm:"column:tags;type:text"`
	Source      string `gorm:"column:source;size:16;not null;default:'upload';index:idx_skills_source"`
	SourceURL   string `gorm:"column:source_url;size:512;not null;default:''"`
	Enabled     bool   `gorm:"column:enabled;not null;default:true;index:idx_skills_enabled"`
	Checksum    string `gorm:"column:checksum;size:64;not null;default:''"`
	Content     string `gorm:"column:content;type:mediumtext"`
	// ExecutionSpec 执行配置（JSON）：local / remote / async、超时、重试、参数 Schema。
	// 单独一列而不是塞进 tags：它是运行时路由依据，需要能索引/查询。
	ExecutionSpec string    `gorm:"column:execution_spec;type:text"`
	FileCount     int       `gorm:"column:file_count;not null;default:0"`
	Size          int       `gorm:"column:size;not null;default:0"`
	InstalledAt   time.Time `gorm:"column:installed_at;not null"`
	UpdatedAt     time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (SkillPO) TableName() string { return "skills" }

// SkillFilePO 技能文件表（skill_files）：按技能名 + 相对路径唯一
type SkillFilePO struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement"`
	SkillName string    `gorm:"column:skill_name;size:64;not null;uniqueIndex:uk_skill_files_name_path,priority:1;index:idx_skill_files_name"`
	Path      string    `gorm:"column:path;size:512;not null;uniqueIndex:uk_skill_files_name_path,priority:2"`
	Content   string    `gorm:"column:content;type:mediumtext"`
	Size      int       `gorm:"column:size;not null;default:0"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

// TableName 表名
func (SkillFilePO) TableName() string { return "skill_files" }

// SkillHubPO 技能市场数据源（skill_hubs）
type SkillHubPO struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement"`
	Name        string    `gorm:"column:name;size:64;not null;uniqueIndex:uk_skill_hubs_name"`
	BaseURL     string    `gorm:"column:base_url;size:512;not null"`
	Token       string    `gorm:"column:token;type:text"`
	Enabled     bool      `gorm:"column:enabled;not null;default:true"`
	IsDefault   bool      `gorm:"column:is_default;not null;default:false;index:idx_skill_hubs_default"`
	Description string    `gorm:"column:description;size:512;not null;default:''"`
	CreatedAt   time.Time `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (SkillHubPO) TableName() string { return "skill_hubs" }

// SkillInstancePO Skill Worker 实例（skill_instances）：注册中心的一行。
//
// 一个技能可以有多个实例；实例由 Worker 启动时自注册、周期心跳。
// 心跳过期的实例保留在表里（便于排查「为什么没流量」），由调度器判定离线。
type SkillInstancePO struct {
	ID             uint64    `gorm:"primaryKey;autoIncrement"`
	SkillName      string    `gorm:"column:skill_name;size:64;not null;uniqueIndex:uk_skill_instances_name_id,priority:1;index:idx_skill_instances_name"`
	InstanceID     string    `gorm:"column:instance_id;size:191;not null;uniqueIndex:uk_skill_instances_name_id,priority:2"`
	Endpoint       string    `gorm:"column:endpoint;size:512;not null"`
	Version        string    `gorm:"column:version;size:32;not null;default:''"`
	Region         string    `gorm:"column:region;size:64;not null;default:''"`
	Labels         string    `gorm:"column:labels;type:text"`
	Weight         int       `gorm:"column:weight;not null;default:1"`
	MaxConcurrency int       `gorm:"column:max_concurrency;not null;default:4"`
	Inflight       int       `gorm:"column:inflight;not null;default:0"`
	Status         string    `gorm:"column:status;size:16;not null;default:'online';index:idx_skill_instances_status"`
	LatencyMS      int64     `gorm:"column:latency_ms;not null;default:0"`
	Failures       int       `gorm:"column:failures;not null;default:0"`
	Successes      int64     `gorm:"column:successes;not null;default:0"`
	LastError      string    `gorm:"column:last_error;size:512;not null;default:''"`
	LastHeartbeat  time.Time `gorm:"column:last_heartbeat;not null;index:idx_skill_instances_heartbeat"`
	CreatedAt      time.Time `gorm:"column:created_at;not null"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (SkillInstancePO) TableName() string { return "skill_instances" }

// SkillExecutionPO 技能执行记录（skill_executions）：账本 + 异步队列。
//
// 同一张表承担两个角色：同步调用是流水账，异步调用是待办队列（status=queued）。
// 合成一张表是刻意的：任务状态与账本同源，不需要「队列里有、库里没有」的对账。
type SkillExecutionPO struct {
	ID             string     `gorm:"column:id;size:32;primaryKey"`
	TaskID         string     `gorm:"column:task_id;size:64;not null;default:'';index:idx_skill_exec_task"`
	SkillName      string     `gorm:"column:skill_name;size:64;not null;index:idx_skill_exec_skill"`
	SkillVersion   string     `gorm:"column:skill_version;size:32;not null;default:''"`
	Mode           string     `gorm:"column:mode;size:16;not null;default:'remote'"`
	Status         string     `gorm:"column:status;size:16;not null;index:idx_skill_exec_status,priority:1"`
	TenantID       string     `gorm:"column:tenant_id;size:64;not null;default:'public';index:idx_skill_exec_tenant"`
	UserID         string     `gorm:"column:user_id;size:64;not null;default:''"`
	AgentKey       string     `gorm:"column:agent_key;size:64;not null;default:''"`
	SessionID      string     `gorm:"column:session_id;size:64;not null;default:''"`
	IdempotencyKey string     `gorm:"column:idempotency_key;size:191;not null;default:'';index:idx_skill_exec_idem,priority:1"`
	Input          string     `gorm:"column:input;type:text"`
	Output         string     `gorm:"column:output;type:mediumtext"`
	Error          string     `gorm:"column:error;size:1024;not null;default:''"`
	WorkerID       string     `gorm:"column:worker_id;size:191;not null;default:''"`
	Attempts       int        `gorm:"column:attempts;not null;default:0"`
	LatencyMS      int64      `gorm:"column:latency_ms;not null;default:0"`
	TraceID        string     `gorm:"column:trace_id;size:64;not null;default:''"`
	LeaseUntil     *time.Time `gorm:"column:lease_until;index:idx_skill_exec_lease"`
	QueuedAt       *time.Time `gorm:"column:queued_at"`
	StartedAt      *time.Time `gorm:"column:started_at"`
	FinishedAt     *time.Time `gorm:"column:finished_at;index:idx_skill_exec_finished"`
	CreatedAt      time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (SkillExecutionPO) TableName() string { return "skill_executions" }
