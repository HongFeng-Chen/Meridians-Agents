// prompt.go 提示词模板与版本 MySQL 仓储：
// prompt_templates 存元信息与「当前生效版本指针」，prompt_versions 存只增不改的版本快照。
//
// 两张表放同一事务里写，保证「版本号分配 + 激活指针切换」不会出现
// 版本存在但指针悬空，或指针指向不存在版本的中间态。
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/prompt"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// PromptRepo 提示词仓库
type PromptRepo struct {
	db *gorm.DB
}

var _ prompt.Repo = (*PromptRepo)(nil)

// NewPromptRepo 创建仓库；auto_migrate 打开时自动建表
func NewPromptRepo(c *conf.Data, d *db.Data) (prompt.Repo, error) {
	r := &PromptRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.TemplatePO{}, &models.VersionPO{}, &models.PromptVersionScorePO{}); err != nil {
			return nil, fmt.Errorf("迁移 prompt 表: %w", err)
		}
	}
	return r, nil
}

// Create 建模板并写入 v1，同时置为生效版本
func (r *PromptRepo) Create(ctx context.Context, p *prompt.Prompt, v *prompt.Version) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tpl := &models.TemplatePO{
			PromptKey:     p.Key,
			Name:          clip(p.Name, 191),
			Scene:         p.Scene,
			Description:   clip(p.Description, 512),
			Tags:          encodeTags(p.Tags),
			ActiveVersion: 1,
			Builtin:       p.Builtin,
		}
		if err := tx.Create(tpl).Error; err != nil {
			return mapPromptErr(err)
		}
		po := newVersionPO(v)
		po.PromptKey = p.Key
		po.Version = 1
		if err := tx.Create(po).Error; err != nil {
			return mapPromptErr(err)
		}
		return nil
	})
}

// Get 查询模板元信息（含版本数量）
func (r *PromptRepo) Get(ctx context.Context, key string) (*prompt.Prompt, error) {
	var po models.TemplatePO
	err := r.db.WithContext(ctx).Where("prompt_key = ?", key).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, prompt.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := templateToBiz(&po)
	var n int64
	if err := r.db.WithContext(ctx).Model(&models.VersionPO{}).Where("prompt_key = ?", key).Count(&n).Error; err == nil {
		out.VersionCount = int(n)
	}
	return out, nil
}

// List 列表：可按场景与关键字过滤，按更新时间倒序
func (r *PromptRepo) List(ctx context.Context, f prompt.ListFilter) ([]prompt.Prompt, error) {
	q := r.db.WithContext(ctx).Model(&models.TemplatePO{})
	if f.Scene != "" {
		q = q.Where("scene = ?", f.Scene)
	}
	if f.Keyword != "" {
		like := "%" + escapeLike(f.Keyword) + "%"
		q = q.Where("(prompt_key LIKE ? ESCAPE '\\\\' OR name LIKE ? ESCAPE '\\\\' OR description LIKE ? ESCAPE '\\\\')", like, like, like)
	}
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	var pos []models.TemplatePO
	if err := q.Order("updated_at DESC").Find(&pos).Error; err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(pos))
	for i := range pos {
		keys = append(keys, pos[i].PromptKey)
	}
	counts := r.versionCounts(ctx, keys)

	out := make([]prompt.Prompt, 0, len(pos))
	for i := range pos {
		p := templateToBiz(&pos[i])
		p.VersionCount = counts[p.Key]
		out = append(out, *p)
	}
	return out, nil
}

// versionCounts 批量统计版本数，避免列表 N+1 查询
func (r *PromptRepo) versionCounts(ctx context.Context, keys []string) map[string]int {
	out := make(map[string]int, len(keys))
	if len(keys) == 0 {
		return out
	}
	var rows []struct {
		PromptKey string
		Total     int
	}
	// 先按 key 取版本数；MySQL 的 GROUP BY 结果列名不固定，这里显式起别名
	if err := r.db.WithContext(ctx).Model(&models.VersionPO{}).
		Select("prompt_key AS prompt_key, COUNT(*) AS total").
		Where("prompt_key IN ?", keys).
		Group("prompt_key").
		Scan(&rows).Error; err != nil {
		return out
	}
	for _, row := range rows {
		out[row.PromptKey] = row.Total
	}
	return out
}

// Update 只更新元信息
func (r *PromptRepo) Update(ctx context.Context, p *prompt.Prompt) error {
	res := r.db.WithContext(ctx).Model(&models.TemplatePO{}).Where("prompt_key = ?", p.Key).
		Updates(map[string]any{
			"name":        clip(p.Name, 191),
			"scene":       p.Scene,
			"description": clip(p.Description, 512),
			"tags":        encodeTags(p.Tags),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return prompt.ErrNotFound
	}
	return nil
}

// Delete 删除模板与全部版本
func (r *PromptRepo) Delete(ctx context.Context, key string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("prompt_key = ?", key).Delete(&models.VersionPO{}).Error; err != nil {
			return err
		}
		res := tx.Where("prompt_key = ?", key).Delete(&models.TemplatePO{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return prompt.ErrNotFound
		}
		return nil
	})
}

// AddVersion 追加版本并返回新版本号（不激活）。
// 唯一索引兜底并发：两个请求同时算出同一版本号时，后者会拿到 ErrExists，由调用方重试。
func (r *PromptRepo) AddVersion(ctx context.Context, v *prompt.Version) (int, error) {
	var created int
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var tpl models.TemplatePO
		err := tx.Where("prompt_key = ?", v.PromptKey).First(&tpl).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return prompt.ErrNotFound
		}
		if err != nil {
			return err
		}
		var maxVer int
		if err := tx.Model(&models.VersionPO{}).Where("prompt_key = ?", v.PromptKey).
			Select("COALESCE(MAX(version), 0)").Scan(&maxVer).Error; err != nil {
			return err
		}
		po := newVersionPO(v)
		po.PromptKey = v.PromptKey
		po.Version = maxVer + 1
		if err := tx.Create(po).Error; err != nil {
			return mapPromptErr(err)
		}
		created = po.Version
		return tx.Model(&models.TemplatePO{}).Where("prompt_key = ?", v.PromptKey).
			Update("updated_at", time.Now()).Error
	})
	if err != nil {
		return 0, err
	}
	return created, nil
}

// GetVersion 查询指定版本
func (r *PromptRepo) GetVersion(ctx context.Context, key string, version int) (*prompt.Version, error) {
	var po models.VersionPO
	err := r.db.WithContext(ctx).Where("prompt_key = ? AND version = ?", key, version).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, prompt.ErrVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	return versionToBiz(&po), nil
}

// ActiveVersion 查询当前生效版本
func (r *PromptRepo) ActiveVersion(ctx context.Context, key string) (*prompt.Version, error) {
	var tpl models.TemplatePO
	err := r.db.WithContext(ctx).Where("prompt_key = ?", key).First(&tpl).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, prompt.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if tpl.ActiveVersion <= 0 {
		return nil, prompt.ErrVersionNotFound
	}
	v, err := r.GetVersion(ctx, key, tpl.ActiveVersion)
	if err != nil {
		return nil, err
	}
	v.Active = true
	return v, nil
}

// ListVersions 版本列表（倒序）
func (r *PromptRepo) ListVersions(ctx context.Context, key string, limit int) ([]prompt.Version, error) {
	if limit <= 0 {
		limit = 200
	}
	var pos []models.VersionPO
	if err := r.db.WithContext(ctx).Where("prompt_key = ?", key).
		Order("version DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]prompt.Version, 0, len(pos))
	for i := range pos {
		out = append(out, *versionToBiz(&pos[i]))
	}
	return out, nil
}

// Activate 切换生效版本（先校验版本存在，避免指针悬空）
func (r *PromptRepo) Activate(ctx context.Context, key string, version int) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cnt int64
		if err := tx.Model(&models.VersionPO{}).Where("prompt_key = ? AND version = ?", key, version).
			Count(&cnt).Error; err != nil {
			return err
		}
		if cnt == 0 {
			return prompt.ErrVersionNotFound
		}
		res := tx.Model(&models.TemplatePO{}).Where("prompt_key = ?", key).
			Updates(map[string]any{"active_version": version, "updated_at": time.Now()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return prompt.ErrNotFound
		}
		return nil
	})
}

// Stats 模板数与版本数
func (r *PromptRepo) Stats(ctx context.Context) (prompt.Stats, error) {
	var st prompt.Stats
	if err := r.db.WithContext(ctx).Model(&models.TemplatePO{}).Count(&st.Prompts).Error; err != nil {
		return st, err
	}
	if err := r.db.WithContext(ctx).Model(&models.VersionPO{}).Count(&st.Versions).Error; err != nil {
		return st, err
	}
	return st, nil
}

func newVersionPO(v *prompt.Version) *models.VersionPO {
	return &models.VersionPO{
		PromptKey:   v.PromptKey,
		Version:     v.Version,
		Content:     v.Content,
		Variables:   encodeTags(v.Variables),
		Temperature: v.Temperature,
		MaxTokens:   v.MaxTokens,
		Note:        clip(v.Note, 512),
		Author:      clip(v.Author, 64),
	}
}

func templateToBiz(po *models.TemplatePO) *prompt.Prompt {
	return &prompt.Prompt{
		Key:           po.PromptKey,
		Name:          po.Name,
		Scene:         po.Scene,
		Description:   po.Description,
		Tags:          decodeList(po.Tags),
		ActiveVersion: po.ActiveVersion,
		Builtin:       po.Builtin,
		CreatedAt:     po.CreatedAt,
		UpdatedAt:     po.UpdatedAt,
	}
}

func versionToBiz(po *models.VersionPO) *prompt.Version {
	return &prompt.Version{
		PromptKey:   po.PromptKey,
		Version:     po.Version,
		Content:     po.Content,
		Variables:   decodeList(po.Variables),
		Temperature: po.Temperature,
		MaxTokens:   po.MaxTokens,
		Note:        po.Note,
		Author:      po.Author,
		CreatedAt:   po.CreatedAt,
	}
}

// mapPromptErr 把驱动错误映射为领域错误
func mapPromptErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "Duplicate entry") || strings.Contains(err.Error(), "1062") {
		return prompt.ErrExists
	}
	return err
}

// InsertVersionScore 追加一条版本评测得分
func (r *PromptRepo) InsertVersionScore(ctx context.Context, s prompt.VersionScore) error {
	return r.db.WithContext(ctx).Create(&models.PromptVersionScorePO{
		PromptKey: s.PromptKey,
		Version:   s.Version,
		RunID:     s.RunID,
		Score:     s.Score,
		Cases:     s.Cases,
	}).Error
}

// ListVersionScores 模板的全部版本得分（时间倒序）
func (r *PromptRepo) ListVersionScores(ctx context.Context, key string, limit int) ([]prompt.VersionScore, error) {
	if limit <= 0 {
		limit = 100
	}
	var pos []models.PromptVersionScorePO
	if err := r.db.WithContext(ctx).
		Where("prompt_key = ?", key).
		Order("id DESC").Limit(limit).
		Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]prompt.VersionScore, 0, len(pos))
	for _, po := range pos {
		out = append(out, prompt.VersionScore{
			PromptKey: po.PromptKey,
			Version:   po.Version,
			RunID:     po.RunID,
			Score:     po.Score,
			Cases:     po.Cases,
			CreatedAt: po.CreatedAt,
		})
	}
	return out, nil
}
