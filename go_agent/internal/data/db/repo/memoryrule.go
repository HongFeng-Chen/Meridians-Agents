// memoryrule.go 记忆规则与检索历史的 MySQL 仓储实现（挂在 MemoryRepo 上）。
package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"go_agent/internal/biz/memory"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// ---------- 记忆规则 ----------

func ruleToBiz(po *models.MemoryRulePO) *memory.MemoryRule {
	return &memory.MemoryRule{
		ID:          po.ID,
		Name:        po.Name,
		Type:        po.Type,
		Kind:        po.Kind,
		Instruction: po.Instruction,
		Builtin:     po.Builtin,
		Enabled:     po.Enabled,
		CreatedAt:   po.CreatedAt,
		UpdatedAt:   po.UpdatedAt,
	}
}

// ListRules 规则列表（内置在前，其余按 id 正序）
func (r *MemoryRepo) ListRules(ctx context.Context) ([]*memory.MemoryRule, error) {
	var pos []models.MemoryRulePO
	if err := r.db.WithContext(ctx).
		Order("builtin DESC, id ASC").
		Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]*memory.MemoryRule, 0, len(pos))
	for i := range pos {
		out = append(out, ruleToBiz(&pos[i]))
	}
	return out, nil
}

// GetRule 单条规则；不存在返回 ErrNotFound
func (r *MemoryRepo) GetRule(ctx context.Context, id uint64) (*memory.MemoryRule, error) {
	var po models.MemoryRulePO
	if err := r.db.WithContext(ctx).First(&po, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, memory.ErrNotFound
		}
		return nil, err
	}
	return ruleToBiz(&po), nil
}

// SaveRule 新建（ID=0）或更新
func (r *MemoryRepo) SaveRule(ctx context.Context, m *memory.MemoryRule) error {
	if m.ID == 0 {
		po := models.MemoryRulePO{
			Name:        clip(m.Name, 128),
			Type:        clip(m.Type, 16),
			Kind:        clip(m.Kind, 32),
			Instruction: m.Instruction,
			Builtin:     m.Builtin,
			Enabled:     m.Enabled,
			CreatedAt:   m.CreatedAt,
			UpdatedAt:   m.UpdatedAt,
		}
		if err := r.db.WithContext(ctx).Create(&po).Error; err != nil {
			return err
		}
		m.ID = po.ID
		return nil
	}
	return r.db.WithContext(ctx).Model(&models.MemoryRulePO{}).Where("id = ?", m.ID).Updates(map[string]any{
		"name":        clip(m.Name, 128),
		"kind":        clip(m.Kind, 32),
		"instruction": m.Instruction,
		"enabled":     m.Enabled,
		"updated_at":  m.UpdatedAt,
	}).Error
}

// DeleteRule 删除规则
func (r *MemoryRepo) DeleteRule(ctx context.Context, id uint64) error {
	return r.db.WithContext(ctx).Delete(&models.MemoryRulePO{}, id).Error
}

// CountBuiltinRules 内置规则数
func (r *MemoryRepo) CountBuiltinRules(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&models.MemoryRulePO{}).Where("builtin = ?", true).Count(&n).Error
	return n, err
}

// ---------- 检索历史 ----------

// SaveRecallLog 追加召回流水
func (r *MemoryRepo) SaveRecallLog(ctx context.Context, l *memory.RecallLog) error {
	po := models.MemoryRecallLogPO{
		UserID:    clip(l.UserID, 128),
		Query:     l.Query,
		Source:    clip(l.Source, 32),
		TopK:      l.TopK,
		Hits:      l.Hits,
		MemoryIDs: strings.Join(l.MemoryIDs, ","),
		LatencyMS: l.LatencyMS,
		CreatedAt: l.CreatedAt,
	}
	if po.CreatedAt.IsZero() {
		po.CreatedAt = time.Now()
	}
	return r.db.WithContext(ctx).Create(&po).Error
}

// ListRecallLogs 检索历史（时间倒序）；userID 为空表示全部
func (r *MemoryRepo) ListRecallLogs(ctx context.Context, userID string, limit int) ([]*memory.RecallLog, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := r.db.WithContext(ctx).Model(&models.MemoryRecallLogPO{})
	if userID != "" {
		q = q.Where("user_id = ?", userID)
	}
	var pos []models.MemoryRecallLogPO
	if err := q.Order("id DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]*memory.RecallLog, 0, len(pos))
	for i := range pos {
		po := &pos[i]
		var ids []string
		if po.MemoryIDs != "" {
			ids = strings.Split(po.MemoryIDs, ",")
		}
		out = append(out, &memory.RecallLog{
			ID: po.ID, UserID: po.UserID, Query: po.Query, Source: po.Source,
			TopK: po.TopK, Hits: po.Hits, MemoryIDs: ids,
			LatencyMS: po.LatencyMS, CreatedAt: po.CreatedAt,
		})
	}
	return out, nil
}

// MarkRecalled 召回统计回填：recall_count+1、last_recalled_at
func (r *MemoryRepo) MarkRecalled(ctx context.Context, memoryIDs []string, at time.Time) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&models.MemoryPO{}).
		Where("memory_id IN ?", memoryIDs).
		Updates(map[string]any{
			"recall_count":     gorm.Expr("recall_count + 1"),
			"last_recalled_at": at,
		}).Error
}
