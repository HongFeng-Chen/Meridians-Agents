// llm.go 大模型数据源 MySQL 仓储：llm_providers 表。
// 默认标记在事务内保证唯一（切换默认时先清空再置位）。
package repo

import (
	"context"
	"errors"
	"fmt"

	"go_agent/internal/biz/llm"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// LLMProviderRepo 数据源仓库
type LLMProviderRepo struct {
	db *gorm.DB
}

var _ llm.ProviderRepo = (*LLMProviderRepo)(nil)

// NewLLMProviderRepo 创建仓库；auto_migrate 打开时自动建表
func NewLLMProviderRepo(c *conf.Data, d *db.Data) (llm.ProviderRepo, error) {
	r := &LLMProviderRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.LLMProviderPO{}); err != nil {
			return nil, fmt.Errorf("迁移 llm_providers 表: %w", err)
		}
	}
	return r, nil
}

// List 全部数据源（默认在前，其次优先级）
func (r *LLMProviderRepo) List(ctx context.Context) ([]llm.Provider, error) {
	var pos []models.LLMProviderPO
	if err := r.db.WithContext(ctx).
		Order("is_default DESC, priority DESC, name ASC").Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]llm.Provider, 0, len(pos))
	for i := range pos {
		out = append(out, *providerToBiz(&pos[i]))
	}
	return out, nil
}

// Get 按名称查询
func (r *LLMProviderRepo) Get(ctx context.Context, name string) (*llm.Provider, error) {
	var po models.LLMProviderPO
	err := r.db.WithContext(ctx).Where("name = ?", name).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, llm.ErrProviderNotFound
	}
	if err != nil {
		return nil, err
	}
	return providerToBiz(&po), nil
}

// Create 新增数据源
func (r *LLMProviderRepo) Create(ctx context.Context, p *llm.Provider) error {
	po := providerToPO(p)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(po).Error; err != nil {
			return mapLLMErr(err)
		}
		if p.IsDefault {
			if err := clearDefault(tx, po.Name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.ID = po.ID
	p.CreatedAt, p.UpdatedAt = po.CreatedAt, po.UpdatedAt
	return nil
}

// Update 全量更新业务字段（含默认标记）
func (r *LLMProviderRepo) Update(ctx context.Context, p *llm.Provider) error {
	po := providerToPO(p)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.LLMProviderPO{}).Where("name = ?", p.Name).
			Updates(map[string]any{
				"kind":            po.Kind,
				"base_url":        po.BaseURL,
				"api_key":         po.APIKey,
				"models":          po.Models,
				"default_model":   po.DefaultModel,
				"temperature":     po.Temperature,
				"max_tokens":      po.MaxTokens,
				"timeout_seconds": po.TimeoutSec,
				"enabled":         po.Enabled,
				"is_default":      po.IsDefault,
				"priority":        po.Priority,
				"scenes":          po.Scenes,
				"description":     po.Description,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return llm.ErrProviderNotFound
		}
		if po.IsDefault {
			return clearDefault(tx, po.Name)
		}
		return nil
	})
}

// Delete 删除数据源
func (r *LLMProviderRepo) Delete(ctx context.Context, name string) error {
	res := r.db.WithContext(ctx).Where("name = ?", name).Delete(&models.LLMProviderPO{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return llm.ErrProviderNotFound
	}
	return nil
}

// SetDefault 切换默认数据源（先全清再置位，保证唯一）
func (r *LLMProviderRepo) SetDefault(ctx context.Context, name string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var po models.LLMProviderPO
		err := tx.Where("name = ?", name).First(&po).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return llm.ErrProviderNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.Model(&models.LLMProviderPO{}).Where("is_default = ?", true).
			Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&models.LLMProviderPO{}).Where("name = ?", name).
			Update("is_default", true).Error
	})
}

// Stats 数据源统计
func (r *LLMProviderRepo) Stats(ctx context.Context) (llm.Stats, error) {
	var st llm.Stats
	if err := r.db.WithContext(ctx).Model(&models.LLMProviderPO{}).Count(&st.Providers).Error; err != nil {
		return st, err
	}
	if err := r.db.WithContext(ctx).Model(&models.LLMProviderPO{}).
		Where("enabled = ?", true).Count(&st.Enabled).Error; err != nil {
		return st, err
	}
	var po models.LLMProviderPO
	err := r.db.WithContext(ctx).Where("is_default = ?", true).First(&po).Error
	if err == nil {
		st.Default = po.Name
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return st, err
	}
	return st, nil
}

// clearDefault 把除 keep 之外的默认标记清空
func clearDefault(tx *gorm.DB, keep string) error {
	return tx.Model(&models.LLMProviderPO{}).
		Where("is_default = ? AND name <> ?", true, keep).
		Update("is_default", false).Error
}

func providerToPO(p *llm.Provider) *models.LLMProviderPO {
	return &models.LLMProviderPO{
		ID:           p.ID,
		Name:         p.Name,
		Kind:         p.Kind,
		BaseURL:      p.BaseURL,
		APIKey:       p.APIKey,
		Models:       encodeTags(p.Models),
		DefaultModel: p.DefaultModel,
		Temperature:  p.Temperature,
		MaxTokens:    p.MaxTokens,
		TimeoutSec:   p.TimeoutSec,
		Enabled:      p.Enabled,
		IsDefault:    p.IsDefault,
		Priority:     p.Priority,
		Scenes:       encodeTags(p.Scenes),
		Description:  p.Description,
	}
}

func providerToBiz(po *models.LLMProviderPO) *llm.Provider {
	return &llm.Provider{
		ID:           po.ID,
		Name:         po.Name,
		Kind:         po.Kind,
		BaseURL:      po.BaseURL,
		APIKey:       po.APIKey,
		HasKey:       po.APIKey != "",
		Models:       decodeList(po.Models),
		DefaultModel: po.DefaultModel,
		Temperature:  po.Temperature,
		MaxTokens:    po.MaxTokens,
		TimeoutSec:   po.TimeoutSec,
		Enabled:      po.Enabled,
		IsDefault:    po.IsDefault,
		Priority:     po.Priority,
		Scenes:       decodeList(po.Scenes),
		Description:  po.Description,
		CreatedAt:    po.CreatedAt,
		UpdatedAt:    po.UpdatedAt,
	}
}

// mapLLMErr 唯一索引冲突 → 领域错误
func mapLLMErr(err error) error {
	if err == nil {
		return nil
	}
	if isDuplicate(err) {
		return llm.ErrProviderExists
	}
	return err
}
