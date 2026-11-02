// memory.go 长期记忆 MySQL 仓储：权威数据源。
// 职责：内容 checksum 去重、过期时间（expires_at）判定、生命周期管理。
package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go_agent/internal/biz/memory"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MemoryRepo 长期记忆仓库
type MemoryRepo struct {
	db *gorm.DB
}

var _ memory.LongTermRepo = (*MemoryRepo)(nil)

// gormClauseUpsert 按 memory_id 冲突时更新全部业务列
var gormClauseUpsert = clause.OnConflict{
	Columns: []clause.Column{{Name: "memory_id"}},
	DoUpdates: clause.AssignmentColumns([]string{
		"user_id", "kind", "content", "importance", "tags", "source", "rule_name",
		"checksum", "entities", "relations", "expires_at", "updated_at",
	}),
}

// NewMemoryRepo 创建仓库；auto_migrate 打开时自动建表
func NewMemoryRepo(c *conf.Data, d *db.Data) (memory.LongTermRepo, error) {
	r := &MemoryRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.MemoryPO{}, &models.MemoryRulePO{}, &models.MemoryRecallLogPO{}); err != nil {
			return nil, fmt.Errorf("迁移记忆相关表: %w", err)
		}
	}
	return r, nil
}

// Save 写入或更新（按 memory_id 覆盖）
func (r *MemoryRepo) Save(ctx context.Context, m *memory.LongTermMemory) error {
	po := &models.MemoryPO{
		MemoryID:   m.ID,
		UserID:     m.UserID,
		Kind:       m.Kind,
		Content:    m.Content,
		Importance: m.Importance,
		Tags:       marshalTags(m.Tags),
		Source:     clip(m.Source, 512),
		RuleName:   clip(m.RuleName, 128),
		Checksum:   m.Checksum,
		Entities:   m.Entities,
		Relations:  m.Relations,
		ExpiresAt:  m.ExpiresAt,
	}
	err := r.db.WithContext(ctx).
		Clauses(gormClauseUpsert).
		Create(po).Error
	if err != nil {
		return fmt.Errorf("写入记忆: %w", err)
	}
	m.CreatedAt, m.UpdatedAt = po.CreatedAt, po.UpdatedAt
	return nil
}

// Get 按 memory_id 查询；不存在返回 (nil, nil)
func (r *MemoryRepo) Get(ctx context.Context, id string) (*memory.LongTermMemory, error) {
	var po models.MemoryPO
	err := r.db.WithContext(ctx).Where("memory_id = ?", id).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return memoryToBiz(&po), nil
}

// GetByIDs 批量取回（按传入顺序返回，缺失的跳过）
func (r *MemoryRepo) GetByIDs(ctx context.Context, ids []string) ([]*memory.LongTermMemory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var pos []models.MemoryPO
	if err := r.db.WithContext(ctx).Where("memory_id IN ?", ids).Find(&pos).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]*memory.LongTermMemory, len(pos))
	for i := range pos {
		m := memoryToBiz(&pos[i])
		byID[m.ID] = m
	}
	out := make([]*memory.LongTermMemory, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// FindByChecksum 按 (user_id, checksum) 查找
func (r *MemoryRepo) FindByChecksum(ctx context.Context, userID, checksum string) (*memory.LongTermMemory, error) {
	var po models.MemoryPO
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND checksum = ?", userID, checksum).
		First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return memoryToBiz(&po), nil
}

// List 记忆列表（新->旧）；userID 为空表示全部；includeExpired 控制过期记录
func (r *MemoryRepo) List(ctx context.Context, userID string, includeExpired bool, limit int) ([]*memory.LongTermMemory, error) {
	if limit <= 0 {
		limit = 100
	}
	q := r.db.WithContext(ctx).Model(&models.MemoryPO{})
	if userID != "" {
		q = q.Where("user_id = ?", userID)
	}
	if !includeExpired {
		q = q.Where("expires_at IS NULL OR expires_at > ?", time.Now())
	}
	var pos []models.MemoryPO
	if err := q.Order("updated_at DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]*memory.LongTermMemory, 0, len(pos))
	for i := range pos {
		out = append(out, memoryToBiz(&pos[i]))
	}
	return out, nil
}

// ListExpired 已过失效时间的记录（清理任务用）
func (r *MemoryRepo) ListExpired(ctx context.Context, limit int) ([]*memory.LongTermMemory, error) {
	if limit <= 0 {
		limit = 500
	}
	var pos []models.MemoryPO
	if err := r.db.WithContext(ctx).
		Where("expires_at IS NOT NULL AND expires_at <= ?", time.Now()).
		Order("expires_at ASC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]*memory.LongTermMemory, 0, len(pos))
	for i := range pos {
		out = append(out, memoryToBiz(&pos[i]))
	}
	return out, nil
}

// Delete 删除记录
func (r *MemoryRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Where("memory_id = ?", id).Delete(&models.MemoryPO{}).Error
}

// Count 总数与已过期数
func (r *MemoryRepo) Count(ctx context.Context) (int64, int64, error) {
	var total, expired int64
	if err := r.db.WithContext(ctx).Model(&models.MemoryPO{}).Count(&total).Error; err != nil {
		return 0, 0, err
	}
	if err := r.db.WithContext(ctx).Model(&models.MemoryPO{}).
		Where("expires_at IS NOT NULL AND expires_at <= ?", time.Now()).
		Count(&expired).Error; err != nil {
		return 0, 0, err
	}
	return total, expired, nil
}

func memoryToBiz(po *models.MemoryPO) *memory.LongTermMemory {
	return &memory.LongTermMemory{
		ID:             po.MemoryID,
		UserID:         po.UserID,
		Kind:           po.Kind,
		Content:        po.Content,
		Importance:     po.Importance,
		Tags:           unmarshalTags(po.Tags),
		Source:         po.Source,
		RuleName:       po.RuleName,
		Entities:       po.Entities,
		Relations:      po.Relations,
		RecallCount:    po.RecallCount,
		LastRecalledAt: po.LastRecalledAt,
		ExpiresAt:      po.ExpiresAt,
		CreatedAt:      po.CreatedAt,
		UpdatedAt:      po.UpdatedAt,
	}
}
