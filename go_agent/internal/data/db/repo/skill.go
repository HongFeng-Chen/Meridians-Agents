// skill.go 技能与技能市场 MySQL 仓储：
// skills（主表 + SKILL.md 正文）、skill_files（多文件包）、skill_hubs（市场数据源）。
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/skill"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SkillRepo 技能仓库
type SkillRepo struct {
	db *gorm.DB
}

var _ skill.Repo = (*SkillRepo)(nil)

// NewSkillRepo 创建仓库；auto_migrate 打开时自动建表
func NewSkillRepo(c *conf.Data, d *db.Data) (skill.Repo, error) {
	r := &SkillRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.SkillPO{}, &models.SkillFilePO{}, &models.SkillHubPO{}); err != nil {
			return nil, fmt.Errorf("迁移 skill 表: %w", err)
		}
	}
	return r, nil
}

// —— 技能 ——

// List 技能列表（不返回文件正文与 SKILL.md，控制响应体积）
func (r *SkillRepo) List(ctx context.Context, f skill.Filter) ([]skill.Skill, error) {
	q := r.db.WithContext(ctx).Model(&models.SkillPO{}).
		Select("id, name, version, title, description, category, tags, source, source_url, enabled, checksum, file_count, size, installed_at, updated_at")
	if f.Keyword != "" {
		like := "%" + escapeLike(f.Keyword) + "%"
		q = q.Where("(name LIKE ? ESCAPE '\\\\' OR title LIKE ? ESCAPE '\\\\' OR description LIKE ? ESCAPE '\\\\')", like, like, like)
	}
	if f.Category != "" {
		q = q.Where("category = ?", strings.ToLower(strings.TrimSpace(f.Category)))
	}
	if f.Source != "" {
		q = q.Where("source = ?", strings.ToLower(strings.TrimSpace(f.Source)))
	}
	if f.Enabled != nil {
		q = q.Where("enabled = ?", *f.Enabled)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var pos []models.SkillPO
	if err := q.Order("updated_at DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]skill.Skill, 0, len(pos))
	for i := range pos {
		out = append(out, *skillSummaryToBiz(&pos[i]))
	}
	return out, nil
}

// Get 技能详情（含 SKILL.md 与全部文件）
func (r *SkillRepo) Get(ctx context.Context, name string) (*skill.Skill, error) {
	var po models.SkillPO
	err := r.db.WithContext(ctx).Where("name = ?", name).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, skill.ErrSkillNotFound
	}
	if err != nil {
		return nil, err
	}
	var files []models.SkillFilePO
	if err := r.db.WithContext(ctx).Where("skill_name = ?", name).
		Order("path ASC").Find(&files).Error; err != nil {
		return nil, err
	}
	out := skillSummaryToBiz(&po)
	out.Content = po.Content
	out.Files = make([]skill.File, 0, len(files))
	for i := range files {
		out.Files = append(out.Files, skill.File{
			Path:    files[i].Path,
			Content: files[i].Content,
			Size:    files[i].Size,
		})
	}
	return out, nil
}

// Save 技能 upsert：主表按 name 覆盖，文件表整体替换（同一事务）
func (r *SkillRepo) Save(ctx context.Context, s *skill.Skill) error {
	po := skillToPO(s)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(skillUpsert).Create(po).Error; err != nil {
			return err
		}
		if err := tx.Where("skill_name = ?", s.Name).Delete(&models.SkillFilePO{}).Error; err != nil {
			return err
		}
		if len(s.Files) == 0 {
			return nil
		}
		files := make([]models.SkillFilePO, 0, len(s.Files))
		for _, f := range s.Files {
			files = append(files, models.SkillFilePO{
				SkillName: s.Name,
				Path:      f.Path,
				Content:   f.Content,
				Size:      f.Size,
			})
		}
		return tx.Create(&files).Error
	})
}

// Delete 删除技能与全部文件
func (r *SkillRepo) Delete(ctx context.Context, name string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("skill_name = ?", name).Delete(&models.SkillFilePO{}).Error; err != nil {
			return err
		}
		res := tx.Where("name = ?", name).Delete(&models.SkillPO{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return skill.ErrSkillNotFound
		}
		return nil
	})
}

// SetEnabled 启用/停用
func (r *SkillRepo) SetEnabled(ctx context.Context, name string, enabled bool) error {
	res := r.db.WithContext(ctx).Model(&models.SkillPO{}).Where("name = ?", name).
		Updates(map[string]any{"enabled": enabled, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return skill.ErrSkillNotFound
	}
	return nil
}

// Stats 规模统计
func (r *SkillRepo) Stats(ctx context.Context) (skill.Stats, error) {
	var st skill.Stats
	if err := r.db.WithContext(ctx).Model(&models.SkillPO{}).Count(&st.Skills).Error; err != nil {
		return st, err
	}
	if err := r.db.WithContext(ctx).Model(&models.SkillPO{}).
		Where("enabled = ?", true).Count(&st.Enabled).Error; err != nil {
		return st, err
	}
	if err := r.db.WithContext(ctx).Model(&models.SkillHubPO{}).Count(&st.Hubs).Error; err != nil {
		return st, err
	}
	return st, nil
}

// —— 技能市场 ——

// ListHubs 市场列表（默认在前）
func (r *SkillRepo) ListHubs(ctx context.Context) ([]skill.Hub, error) {
	var pos []models.SkillHubPO
	if err := r.db.WithContext(ctx).Order("is_default DESC, name ASC").Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]skill.Hub, 0, len(pos))
	for i := range pos {
		out = append(out, *hubToBiz(&pos[i]))
	}
	return out, nil
}

// GetHub 按名称查询市场
func (r *SkillRepo) GetHub(ctx context.Context, name string) (*skill.Hub, error) {
	var po models.SkillHubPO
	err := r.db.WithContext(ctx).Where("name = ?", name).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, skill.ErrHubNotFound
	}
	if err != nil {
		return nil, err
	}
	return hubToBiz(&po), nil
}

// SaveHub 市场 upsert
func (r *SkillRepo) SaveHub(ctx context.Context, h *skill.Hub) error {
	po := &models.SkillHubPO{
		Name:        h.Name,
		BaseURL:     h.BaseURL,
		Token:       h.Token,
		Enabled:     h.Enabled,
		IsDefault:   h.IsDefault,
		Description: h.Description,
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "name"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"base_url", "token", "enabled", "is_default", "description", "updated_at",
			}),
		}).Create(po).Error
		if err != nil {
			return err
		}
		if h.IsDefault {
			return tx.Model(&models.SkillHubPO{}).
				Where("is_default = ? AND name <> ?", true, h.Name).
				Update("is_default", false).Error
		}
		return nil
	})
}

// DeleteHub 删除市场；删除默认市场时自动把第一个启用市场设为默认
func (r *SkillRepo) DeleteHub(ctx context.Context, name string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var po models.SkillHubPO
		err := tx.Where("name = ?", name).First(&po).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return skill.ErrHubNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.Where("name = ?", name).Delete(&models.SkillHubPO{}).Error; err != nil {
			return err
		}
		if !po.IsDefault {
			return nil
		}
		var next models.SkillHubPO
		if err := tx.Where("enabled = ?", true).Order("name ASC").First(&next).Error; err == nil {
			return tx.Model(&models.SkillHubPO{}).Where("name = ?", next.Name).
				Update("is_default", true).Error
		}
		return nil
	})
}

// SetDefaultHub 切换默认市场
func (r *SkillRepo) SetDefaultHub(ctx context.Context, name string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var po models.SkillHubPO
		err := tx.Where("name = ?", name).First(&po).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return skill.ErrHubNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.Model(&models.SkillHubPO{}).Where("is_default = ?", true).
			Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&models.SkillHubPO{}).Where("name = ?", name).
			Update("is_default", true).Error
	})
}

// —— 转换 ——

var skillUpsert = clause.OnConflict{
	Columns: []clause.Column{{Name: "name"}},
	DoUpdates: clause.AssignmentColumns([]string{
		"version", "title", "description", "category", "tags", "source", "source_url",
		"enabled", "checksum", "content", "file_count", "size", "updated_at",
	}),
}

func skillToPO(s *skill.Skill) *models.SkillPO {
	return &models.SkillPO{
		Name:        s.Name,
		Version:     s.Version,
		Title:       s.Title,
		Description: s.Description,
		Category:    s.Category,
		Tags:        encodeTags(s.Tags),
		Source:      s.Source,
		SourceURL:   s.SourceURL,
		Enabled:     s.Enabled,
		Checksum:    s.Checksum,
		Content:     s.Content,
		FileCount:   len(s.Files),
		Size:        s.Size,
		InstalledAt: time.Now(),
	}
}

func skillSummaryToBiz(po *models.SkillPO) *skill.Skill {
	return &skill.Skill{
		Name:        po.Name,
		Version:     po.Version,
		Title:       po.Title,
		Description: po.Description,
		Category:    po.Category,
		Tags:        decodeList(po.Tags),
		Source:      po.Source,
		SourceURL:   po.SourceURL,
		Enabled:     po.Enabled,
		Checksum:    po.Checksum,
		FileCount:   po.FileCount,
		Size:        po.Size,
		InstalledAt: po.InstalledAt,
		UpdatedAt:   po.UpdatedAt,
	}
}

func hubToBiz(po *models.SkillHubPO) *skill.Hub {
	return &skill.Hub{
		Name:        po.Name,
		BaseURL:     po.BaseURL,
		Token:       po.Token,
		HasToken:    po.Token != "",
		Enabled:     po.Enabled,
		IsDefault:   po.IsDefault,
		Description: po.Description,
		CreatedAt:   po.CreatedAt,
		UpdatedAt:   po.UpdatedAt,
	}
}
