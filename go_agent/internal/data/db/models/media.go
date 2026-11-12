package models

import "time"

// MediaImageGenerationPO 图片生成记录（image_generations）。
// 一次生成请求一行：同步接口，请求返回即终态（succeeded / failed）。
type MediaImageGenerationPO struct {
	ID       uint64 `gorm:"primaryKey;autoIncrement"`
	TenantID string `gorm:"column:tenant_id;size:64;not null;default:'';index:idx_media_image_tenant"`
	UserID   string `gorm:"column:user_id;size:64;not null;default:'';index:idx_media_image_user"`
	Provider string `gorm:"column:provider;size:64;not null;default:''"`
	Model    string `gorm:"column:model;size:191;not null;default:''"`
	// text2img | img2img
	Mode   string `gorm:"column:mode;size:16;not null;default:'text2img'"`
	Prompt string `gorm:"column:prompt;type:text"`
	// 请求参数（size / count / seed 等，JSON）
	Params string `gorm:"column:params;size:512;not null;default:''"`
	// 参考图（img2img，JSON 数组；data URI 截断只存前 256 字符防撑爆）
	Inputs string `gorm:"column:inputs;type:text"`
	// 生成产物本地地址（JSON 数组，/api/files/ 前缀）
	Outputs    string    `gorm:"column:outputs;type:text"`
	OK         bool      `gorm:"column:ok;not null;default:false"`
	Error      string    `gorm:"column:error;size:512;not null;default:''"`
	DurationMS int64     `gorm:"column:duration_ms;not null;default:0"`
	CreatedAt  time.Time `gorm:"column:created_at;not null;index:idx_media_image_created"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (MediaImageGenerationPO) TableName() string { return "image_generations" }

// MediaVideoGenerationPO 视频生成记录（video_generations）。
// 视频是异步任务：提交后停留 queued/running，由轮询器推进到终态。
type MediaVideoGenerationPO struct {
	ID       uint64 `gorm:"primaryKey;autoIncrement"`
	TenantID string `gorm:"column:tenant_id;size:64;not null;default:'';index:idx_media_video_tenant"`
	UserID   string `gorm:"column:user_id;size:64;not null;default:'';index:idx_media_video_user"`
	Provider string `gorm:"column:provider;size:64;not null;default:''"`
	Model    string `gorm:"column:model;size:191;not null;default:''"`
	Prompt   string `gorm:"column:prompt;type:text"`
	Params   string `gorm:"column:params;size:512;not null;default:''"`
	// 参考图（图生视频首帧，JSON 数组；data URI 脱敏存储）
	Inputs string `gorm:"column:inputs;type:text"`
	// queued | running | succeeded | failed
	Status string `gorm:"column:status;size:16;not null;default:'queued';index:idx_media_video_status"`
	// 厂商任务 ID（轮询凭据）
	ProviderTaskID string `gorm:"column:provider_task_id;size:191;not null;default:''"`
	// 成品本地地址（成功后落盘的 /api/files/ 地址）
	Output      string    `gorm:"column:output;size:512;not null;default:''"`
	Error       string    `gorm:"column:error;size:512;not null;default:''"`
	DurationMS  int64     `gorm:"column:duration_ms;not null;default:0"`
	SubmittedAt time.Time `gorm:"column:submitted_at;not null"`
	// 终态时间（零值 = 仍在进行）
	FinishedAt *time.Time `gorm:"column:finished_at"`
	CreatedAt  time.Time  `gorm:"column:created_at;not null;index:idx_media_video_created"`
	UpdatedAt  time.Time  `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (MediaVideoGenerationPO) TableName() string { return "video_generations" }
