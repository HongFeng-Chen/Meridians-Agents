// Package image_generations 图片生成持久化与厂商适配。
//
// repo：image_generations 表（一次请求一行，同步接口落库即终态）；
// client：OpenAI 兼容的 /images 协议族——
//   - 文生图：JSON POST {base}/images/generations（OpenAI / SiliconFlow / 火山 Ark 等通用）
//   - 图生图：openai kind 走 multipart /images/edits（gpt-image-1）；
//     其它 kind 在 generations 请求里带 image 参考图字段（SiliconFlow Kolors 风格）
//
// 厂商产物（b64 / 临时 URL）一律转存本地 data/uploads/gen/，
// 记录里只存 /api/files/ 稳定地址。
package image_generations

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

// ImageRepo 图片生成记录仓储
type ImageRepo struct {
	db *db.Data
}

var _ llm.ImageGenRepo = (*ImageRepo)(nil)

// NewImageRepo 创建仓储；auto_migrate 打开时自动建表
func NewImageRepo(c *conf.Data, d *db.Data) (llm.ImageGenRepo, error) {
	r := &ImageRepo{db: d}
	if c.GetDatabase().GetAutoMigrate() {
		if err := d.DB().AutoMigrate(&models.MediaImageGenerationPO{}); err != nil {
			return nil, fmt.Errorf("迁移 image_generations 表: %w", err)
		}
	}
	return r, nil
}

func (r *ImageRepo) Insert(ctx context.Context, rec *llm.ImageGenRecord) error {
	po := imageToPO(rec)
	if err := r.db.DB().WithContext(ctx).Create(po).Error; err != nil {
		return err
	}
	rec.ID = po.ID
	rec.CreatedAt = po.CreatedAt
	rec.UpdatedAt = po.UpdatedAt
	return nil
}

func (r *ImageRepo) Update(ctx context.Context, rec *llm.ImageGenRecord) error {
	po := imageToPO(rec)
	po.ID = rec.ID
	po.CreatedAt = rec.CreatedAt
	return r.db.DB().WithContext(ctx).Save(po).Error
}

func (r *ImageRepo) Get(ctx context.Context, id uint64) (*llm.ImageGenRecord, error) {
	var po models.MediaImageGenerationPO
	err := r.db.DB().WithContext(ctx).First(&po, "id = ?", id).Error
	if mediadb.IsNotFound(err) {
		return nil, llm.ErrMediaNotFound
	}
	if err != nil {
		return nil, err
	}
	return imageToBiz(&po), nil
}

func (r *ImageRepo) List(ctx context.Context, f llm.MediaFilter) ([]*llm.ImageGenRecord, int64, error) {
	q := mediadb.ApplyFilter(r.db.DB().WithContext(ctx).Model(&models.MediaImageGenerationPO{}), f)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	var pos []models.MediaImageGenerationPO
	if err := q.Order("id DESC").
		Limit(mediadb.ClampLimit(f.Limit)).Offset(offset).
		Find(&pos).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*llm.ImageGenRecord, 0, len(pos))
	for i := range pos {
		out = append(out, imageToBiz(&pos[i]))
	}
	return out, total, nil
}

func imageToPO(rec *llm.ImageGenRecord) *models.MediaImageGenerationPO {
	params := map[string]any{}
	if rec.Size != "" {
		params["size"] = rec.Size
	}
	if rec.Count > 0 {
		params["count"] = rec.Count
	}
	paramsJSON, _ := json.Marshal(params)
	return &models.MediaImageGenerationPO{
		ID:         rec.ID,
		TenantID:   rec.TenantID,
		UserID:     rec.UserID,
		Provider:   rec.Provider,
		Model:      rec.Model,
		Mode:       rec.Mode,
		Prompt:     rec.Prompt,
		Params:     string(paramsJSON),
		Inputs:     mediadb.EncodeList(rec.Inputs),
		Outputs:    mediadb.EncodeList(rec.Outputs),
		OK:         rec.OK,
		Error:      rec.Error,
		DurationMS: rec.DurationMS,
		CreatedAt:  rec.CreatedAt,
		UpdatedAt:  time.Now(),
	}
}

func imageToBiz(po *models.MediaImageGenerationPO) *llm.ImageGenRecord {
	rec := &llm.ImageGenRecord{
		ID:         po.ID,
		TenantID:   po.TenantID,
		UserID:     po.UserID,
		Provider:   po.Provider,
		Model:      po.Model,
		Mode:       po.Mode,
		Prompt:     po.Prompt,
		Inputs:     mediadb.DecodeList(po.Inputs),
		Outputs:    mediadb.DecodeList(po.Outputs),
		OK:         po.OK,
		Error:      po.Error,
		DurationMS: po.DurationMS,
		CreatedAt:  po.CreatedAt,
		UpdatedAt:  po.UpdatedAt,
	}
	// size/count 藏在 params JSON 里（与请求原样对应，字段增减不用改表）
	var params struct {
		Size  string `json:"size"`
		Count int    `json:"count"`
	}
	if json.Unmarshal([]byte(po.Params), &params) == nil {
		rec.Size, rec.Count = params.Size, params.Count
	}
	return rec
}
