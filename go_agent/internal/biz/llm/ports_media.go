// ports_media.go 媒体生成（图像 / 视频）的端口与领域类型。
//
// 与对话（chat）同属 LLM 接入域：复用数据源（Provider）的 base_url / api_key，
// 但产物不是 token 而是文件，因此调用记录独立成 image_generations /
// video_generations 两张表，仓库与厂商适配由 internal/data 下的
// image_generations / video_generations 两个包实现。
package llm

import (
	"context"
	"errors"
	"time"
)

// 媒体生成的领域错误
var (
	ErrMediaNoProvider = errors.New("没有可用的媒体生成数据源（请先在数据源配置里填写图像或视频模型）")
	ErrMediaNoModel    = errors.New("该数据源没有配置可用的生成模型")
	ErrMediaNotFound   = errors.New("生成记录不存在")
	ErrMediaInvalid    = errors.New("媒体生成请求不合法")
)

// 生成模式 / 状态常量
const (
	ModeText2Img = "text2img"
	ModeImg2Img  = "img2img"

	MediaQueued    = "queued"
	MediaRunning   = "running"
	MediaSucceeded = "succeeded"
	MediaFailed    = "failed"
)

// MediaTarget 一次媒体调用的厂商目标快照（由用例层按数据源解析后传入）
type MediaTarget struct {
	Kind    string // 数据源 kind：决定协议细节（openai 的图生图走 multipart edits）
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

// —— 图片生成 ——

// ImageGenInput 一次图片生成请求
type ImageGenInput struct {
	// 指定数据源名；空 = 自动路由到第一个配置了图像模型的启用数据源
	Provider string `json:"provider"`
	// 指定模型名；空 = 数据源 image_models 首个
	Model string `json:"model"`
	// text2img（默认）| img2img
	Mode string `json:"mode"`
	// Prompt 生成提示词（img2img 时表示「改成什么样」）
	Prompt string `json:"prompt"`
	// Images img2img 参考图（https URL 或 data URI；text2img 时忽略）
	Images []string `json:"images"`
	// Size 分辨率（如 1024x1024；空 = 厂商默认）
	Size string `json:"size"`
	// Count 生成张数（1-4；空/0 = 1）
	Count int `json:"count"`
	// Seed 随机种子（nil = 不指定）
	Seed *int64 `json:"seed"`
}

// ImageGenRecord 一次图片生成的结果记录
type ImageGenRecord struct {
	ID         uint64    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	UserID     string    `json:"user_id"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Mode       string    `json:"mode"`
	Prompt     string    `json:"prompt"`
	Size       string    `json:"size"`
	Count      int       `json:"count"`
	Inputs     []string  `json:"inputs"`
	Outputs    []string  `json:"outputs"` // 本地 /api/files/ 地址
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ImageGenRepo 图片生成记录仓库
type ImageGenRepo interface {
	Insert(ctx context.Context, rec *ImageGenRecord) error
	Update(ctx context.Context, rec *ImageGenRecord) error
	Get(ctx context.Context, id uint64) (*ImageGenRecord, error)
	List(ctx context.Context, f MediaFilter) ([]*ImageGenRecord, int64, error)
}

// ImageGenClient 图片生成厂商适配（OpenAI 兼容 /images 协议族）。
// 返回值是已落盘的本地地址列表（厂商 b64 / 临时 URL 一律转存本地）。
type ImageGenClient interface {
	Generate(ctx context.Context, t MediaTarget, in *ImageGenInput) (outputs []string, err error)
}

// —— 视频生成 ——

// VideoGenInput 一次视频生成请求
type VideoGenInput struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Prompt   string `json:"prompt"`
	// Image 首帧参考图（图生视频，https URL 或 data URI；空 = 纯文生视频）
	Image       string `json:"image"`
	Size        string `json:"size"` // 如 1280x720；空 = 厂商默认
	DurationSec int    `json:"duration_seconds"`
	// Seed 随机种子（nil = 不指定）
	Seed *int64 `json:"seed"`
}

// VideoGenRecord 一次视频生成的任务记录
type VideoGenRecord struct {
	ID          uint64 `json:"id"`
	TenantID    string `json:"tenant_id"`
	UserID      string `json:"user_id"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Prompt      string `json:"prompt"`
	Size        string `json:"size"`
	DurationSec int    `json:"duration_seconds"`
	// Inputs 参考图（图生视频首帧；data URI 脱敏存储）
	Inputs         []string  `json:"inputs"`
	Status         string    `json:"status"`
	ProviderTaskID string    `json:"provider_task_id"`
	Output         string    `json:"output"`
	Error          string    `json:"error,omitempty"`
	DurationMS     int64     `json:"duration_ms"`
	SubmittedAt    time.Time `json:"submitted_at"`
	FinishedAt     time.Time `json:"finished_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// VideoGenRepo 视频生成记录仓库
type VideoGenRepo interface {
	Insert(ctx context.Context, rec *VideoGenRecord) error
	Update(ctx context.Context, rec *VideoGenRecord) error
	Get(ctx context.Context, id uint64) (*VideoGenRecord, error)
	List(ctx context.Context, f MediaFilter) ([]*VideoGenRecord, int64, error)
	// ListActive 状态在给定集合里的进行中任务（轮询器用，按提交时间正序）
	ListActive(ctx context.Context, statuses []string, submittedBefore time.Time, limit int) ([]*VideoGenRecord, error)
}

// VideoGenClient 视频生成厂商适配（各家的异步任务协议）。
// Submit 返回厂商任务 ID；Poll 查询状态，成功时负责把成品转存本地并返回本地地址。
type VideoGenClient interface {
	Submit(ctx context.Context, t MediaTarget, in *VideoGenInput) (taskID string, err error)
	Poll(ctx context.Context, t MediaTarget, taskID string) (status string, output string, err error)
}

// MediaFilter 生成记录过滤
type MediaFilter struct {
	// UserID 非空时只返回该用户的记录（服务层按登录身份设置；管理员传空 = 全平台）
	UserID   string
	TenantID string
	Provider string
	Model    string
	// ok 为 nil 时不按结果过滤
	OK     *bool
	Limit  int
	Offset int
}
