// Package video_generations 视频生成持久化与厂商适配。
//
// repo：video_generations 表（异步任务：提交 → queued/running → 终态）；
// client：三家主流异步任务协议的适配——
//   - SiliconFlow：POST {base}/video/submit → GET {base}/video/status?requestId=
//   - 火山 Ark：  POST {base}/content/generation/tasks → GET 同路径/{id}
//   - OpenAI：    POST {base}/videos → GET {base}/videos/{id}（成品从 /videos/{id}/content 下载）
//
// 厂商成品 URL 通常几小时就过期，成功后一律转存本地 data/uploads/gen/。
package video_generations

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"
	"go_agent/internal/data/mediadb"
)

// VideoRepo 视频生成记录仓储
type VideoRepo struct {
	db *db.Data
}

var _ llm.VideoGenRepo = (*VideoRepo)(nil)

// NewVideoRepo 创建仓储；auto_migrate 打开时自动建表
func NewVideoRepo(c *conf.Data, d *db.Data) (llm.VideoGenRepo, error) {
	r := &VideoRepo{db: d}
	if c.GetDatabase().GetAutoMigrate() {
		if err := d.DB().AutoMigrate(&models.MediaVideoGenerationPO{}); err != nil {
			return nil, fmt.Errorf("迁移 video_generations 表: %w", err)
		}
	}
	return r, nil
}

func (r *VideoRepo) Insert(ctx context.Context, rec *llm.VideoGenRecord) error {
	po := videoToPO(rec)
	if err := r.db.DB().WithContext(ctx).Create(po).Error; err != nil {
		return err
	}
	rec.ID = po.ID
	rec.CreatedAt = po.CreatedAt
	rec.UpdatedAt = po.UpdatedAt
	return nil
}

func (r *VideoRepo) Update(ctx context.Context, rec *llm.VideoGenRecord) error {
	po := videoToPO(rec)
	po.ID = rec.ID
	po.CreatedAt = rec.CreatedAt
	return r.db.DB().WithContext(ctx).Save(po).Error
}

func (r *VideoRepo) Get(ctx context.Context, id uint64) (*llm.VideoGenRecord, error) {
	var po models.MediaVideoGenerationPO
	err := r.db.DB().WithContext(ctx).First(&po, "id = ?", id).Error
	if mediadb.IsNotFound(err) {
		return nil, llm.ErrMediaNotFound
	}
	if err != nil {
		return nil, err
	}
	return videoToBiz(&po), nil
}

func (r *VideoRepo) List(ctx context.Context, f llm.MediaFilter) ([]*llm.VideoGenRecord, int64, error) {
	q := mediadb.ApplyFilter(r.db.DB().WithContext(ctx).Model(&models.MediaVideoGenerationPO{}), f)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	var pos []models.MediaVideoGenerationPO
	if err := q.Order("id DESC").
		Limit(mediadb.ClampLimit(f.Limit)).Offset(offset).
		Find(&pos).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*llm.VideoGenRecord, 0, len(pos))
	for i := range pos {
		out = append(out, videoToBiz(&pos[i]))
	}
	return out, total, nil
}

func (r *VideoRepo) ListActive(ctx context.Context, statuses []string, submittedBefore time.Time, limit int) ([]*llm.VideoGenRecord, error) {
	if len(statuses) == 0 {
		statuses = []string{llm.MediaQueued, llm.MediaRunning}
	}
	var pos []models.MediaVideoGenerationPO
	if err := r.db.DB().WithContext(ctx).
		Where("status IN ?", statuses).
		Where("submitted_at >= ?", submittedBefore).
		Order("id ASC").
		Limit(limit).
		Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]*llm.VideoGenRecord, 0, len(pos))
	for i := range pos {
		out = append(out, videoToBiz(&pos[i]))
	}
	return out, nil
}

func videoToPO(rec *llm.VideoGenRecord) *models.MediaVideoGenerationPO {
	params := map[string]any{}
	if rec.Size != "" {
		params["size"] = rec.Size
	}
	if rec.DurationSec > 0 {
		params["duration_seconds"] = rec.DurationSec
	}
	paramsJSON, _ := json.Marshal(params)
	po := &models.MediaVideoGenerationPO{
		ID:             rec.ID,
		TenantID:       rec.TenantID,
		UserID:         rec.UserID,
		Provider:       rec.Provider,
		Model:          rec.Model,
		Prompt:         rec.Prompt,
		Params:         string(paramsJSON),
		Inputs:         mediadb.EncodeList(rec.Inputs),
		Status:         rec.Status,
		ProviderTaskID: rec.ProviderTaskID,
		Output:         rec.Output,
		Error:          rec.Error,
		DurationMS:     rec.DurationMS,
		SubmittedAt:    rec.SubmittedAt,
		CreatedAt:      rec.CreatedAt,
		UpdatedAt:      time.Now(),
	}
	if !rec.FinishedAt.IsZero() {
		t := rec.FinishedAt
		po.FinishedAt = &t
	}
	return po
}

func videoToBiz(po *models.MediaVideoGenerationPO) *llm.VideoGenRecord {
	rec := &llm.VideoGenRecord{
		ID:             po.ID,
		TenantID:       po.TenantID,
		UserID:         po.UserID,
		Provider:       po.Provider,
		Model:          po.Model,
		Prompt:         po.Prompt,
		Inputs:         mediadb.DecodeList(po.Inputs),
		Status:         po.Status,
		ProviderTaskID: po.ProviderTaskID,
		Output:         po.Output,
		Error:          po.Error,
		DurationMS:     po.DurationMS,
		SubmittedAt:    po.SubmittedAt,
		CreatedAt:      po.CreatedAt,
		UpdatedAt:      po.UpdatedAt,
	}
	if po.FinishedAt != nil {
		rec.FinishedAt = *po.FinishedAt
	}
	var params struct {
		Size        string `json:"size"`
		DurationSec int    `json:"duration_seconds"`
	}
	if json.Unmarshal([]byte(po.Params), &params) == nil {
		rec.Size, rec.DurationSec = params.Size, params.DurationSec
	}
	return rec
}
