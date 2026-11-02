// aiplatform.go 外部 AI 平台（千问 / 百炼 / 自建 MCP 广场）MySQL 仓储。
//
// 只存配置，不存目录缓存：平台目录每次搜索实时拉取，避免「库里看着有、平台已下架」。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/aiplatform"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AIPlatformRepo 平台仓库
type AIPlatformRepo struct {
	db *gorm.DB
}

var _ aiplatform.Repo = (*AIPlatformRepo)(nil)

// NewAIPlatformRepo 创建仓库；auto_migrate 打开时自动建表
func NewAIPlatformRepo(c *conf.Data, d *db.Data) (aiplatform.Repo, error) {
	r := &AIPlatformRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.AIPlatformPO{}); err != nil {
			return nil, fmt.Errorf("迁移 ai_platforms 表: %w", err)
		}
	}
	return r, nil
}

// List 平台列表
func (r *AIPlatformRepo) List(ctx context.Context, f aiplatform.Filter) ([]aiplatform.Platform, error) {
	q := r.db.WithContext(ctx).Model(&models.AIPlatformPO{})
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("name LIKE ? OR description LIKE ? OR base_url LIKE ?", like, like, like)
	}
	if f.Enabled != nil {
		q = q.Where("is_enabled = ?", *f.Enabled)
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var pos []models.AIPlatformPO
	if err := q.Order("is_default DESC, id ASC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]aiplatform.Platform, 0, len(pos))
	for i := range pos {
		out = append(out, *platformToBiz(&pos[i]))
	}
	return out, nil
}

// Get 按名称查询
func (r *AIPlatformRepo) Get(ctx context.Context, name string) (*aiplatform.Platform, error) {
	var po models.AIPlatformPO
	err := r.db.WithContext(ctx).Where("name = ?", strings.TrimSpace(name)).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, aiplatform.ErrPlatformNotFound
	}
	if err != nil {
		return nil, err
	}
	return platformToBiz(&po), nil
}

// Save 按 name upsert（created_at 只写一次）
func (r *AIPlatformRepo) Save(ctx context.Context, p *aiplatform.Platform) error {
	headers := "{}"
	if len(p.Headers) > 0 {
		if raw, err := json.Marshal(p.Headers); err == nil {
			headers = string(raw)
		}
	}
	now := time.Now()
	po := &models.AIPlatformPO{
		Name:           p.Name,
		Kind:           p.Kind,
		BaseURL:        p.BaseURL,
		APIKey:         p.APIKey,
		AuthScheme:     p.AuthScheme,
		AuthHeader:     p.AuthHeader,
		Headers:        headers,
		CatalogPath:    p.CatalogPath,
		StreamablePath: p.StreamablePath,
		SSEPath:        p.SSEPath,
		SkillBaseURL:   p.SkillBaseURL,
		SkillHub:       p.SkillHub,
		Description:    p.Description,
		Enabled:        p.Enabled,
		IsDefault:      p.IsDefault,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"kind", "base_url", "api_key", "auth_scheme", "auth_header", "headers",
			"catalog_path", "streamable_path", "sse_path",
			"skill_base_url", "skill_hub", "description", "is_enabled", "is_default", "updated_at",
		}),
	}).Create(po).Error
	if err != nil {
		return err
	}
	// 回填 id / 时间，供上层直接使用
	var saved models.AIPlatformPO
	if err := r.db.WithContext(ctx).Where("name = ?", p.Name).First(&saved).Error; err == nil {
		p.ID = saved.ID
		p.CreatedAt = saved.CreatedAt
		p.UpdatedAt = saved.UpdatedAt
	}
	// 默认平台唯一：同事务外再做一次收敛，避免并发保存出现两个默认
	if p.IsDefault {
		return r.SetDefault(ctx, p.Name)
	}
	return nil
}

// Delete 物理删除（不级联删除已接入的 MCP 服务）
func (r *AIPlatformRepo) Delete(ctx context.Context, name string) error {
	res := r.db.WithContext(ctx).Where("name = ?", strings.TrimSpace(name)).Delete(&models.AIPlatformPO{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return aiplatform.ErrPlatformNotFound
	}
	return nil
}

// SetDefault 设为默认（其余降级），单事务
func (r *AIPlatformRepo) SetDefault(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var po models.AIPlatformPO
		if err := tx.Where("name = ?", name).First(&po).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return aiplatform.ErrPlatformNotFound
			}
			return err
		}
		if err := tx.Model(&models.AIPlatformPO{}).
			Where("is_default = ? AND name <> ?", true, name).
			Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&models.AIPlatformPO{}).
			Where("name = ?", name).
			Update("is_default", true).Error
	})
}

func platformToBiz(po *models.AIPlatformPO) *aiplatform.Platform {
	out := &aiplatform.Platform{
		ID:             po.ID,
		Name:           po.Name,
		Kind:           po.Kind,
		BaseURL:        po.BaseURL,
		APIKey:         po.APIKey,
		HasKey:         strings.TrimSpace(po.APIKey) != "",
		AuthScheme:     po.AuthScheme,
		AuthHeader:     po.AuthHeader,
		CatalogPath:    po.CatalogPath,
		StreamablePath: po.StreamablePath,
		SSEPath:        po.SSEPath,
		SkillBaseURL:   po.SkillBaseURL,
		SkillHub:       po.SkillHub,
		Description:    po.Description,
		Enabled:        po.Enabled,
		IsDefault:      po.IsDefault,
		CreatedAt:      po.CreatedAt,
		UpdatedAt:      po.UpdatedAt,
	}
	if raw := strings.TrimSpace(po.Headers); raw != "" && raw != "{}" {
		_ = json.Unmarshal([]byte(raw), &out.Headers)
	}
	return out
}
