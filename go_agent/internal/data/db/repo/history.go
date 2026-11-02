// history.go 统一操作历史 MySQL 仓储：op_histories 表。
// 写路径由各 biz（mcp / memory / rag / skill）经 history.Recorder 异步调用，
// 错误只告警不断业务（见 internal/biz/history）。
package repo

import (
	"context"
	"fmt"
	"strings"

	"go_agent/internal/biz/history"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// HistoryRepo 统一操作历史仓库
type HistoryRepo struct {
	db *gorm.DB
}

var _ history.Repo = (*HistoryRepo)(nil)

// NewHistoryRepo 创建仓库；auto_migrate 打开时自动建表
func NewHistoryRepo(c *conf.Data, d *db.Data) (*HistoryRepo, error) {
	r := &HistoryRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.OpHistoryPO{}); err != nil {
			return nil, fmt.Errorf("迁移 op_histories 表: %w", err)
		}
	}
	return r, nil
}

func (r *HistoryRepo) Insert(ctx context.Context, rec *history.Record) error {
	row := models.OpHistoryPO{
		Kind:      rec.Kind,
		TaskID:    rec.TaskID,
		AgentKey:  rec.AgentKey,
		ConvID:    rec.ConvID,
		Ref:       clip(rec.Ref, 191),
		Query:     clip(rec.Query, 1024),
		Summary:   clip(rec.Summary, 1024),
		Detail:    rec.Detail,
		OK:        rec.OK,
		LatencyMS: rec.LatencyMS,
		CreatedAt: rec.CreatedAt,
	}
	return r.db.WithContext(ctx).Create(&row).Error
}

// List 历史查询（时间倒序 + 总数）
func (r *HistoryRepo) List(ctx context.Context, f history.Filter) ([]history.Record, int64, error) {
	q := r.db.WithContext(ctx).Model(&models.OpHistoryPO{})
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.TaskID > 0 {
		q = q.Where("task_id = ?", f.TaskID)
	}
	if cid := strings.TrimSpace(f.ConvID); cid != "" {
		q = q.Where("conv_id = ?", cid)
	}
	if f.Ref != "" {
		q = q.Where("ref = ?", f.Ref)
	}
	if f.OK != nil {
		q = q.Where("ok = ?", *f.OK)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	var pos []models.OpHistoryPO
	if err := q.Order("created_at DESC, id DESC").Limit(f.Limit).Offset(f.Offset).Find(&pos).Error; err != nil {
		return nil, 0, err
	}
	out := make([]history.Record, 0, len(pos))
	for _, p := range pos {
		out = append(out, historyToBiz(p))
	}
	return out, total, nil
}

// Stats 全局统计：总量 + 按 kind 聚合
func (r *HistoryRepo) Stats(ctx context.Context) (history.Stats, error) {
	var out history.Stats
	if err := r.db.WithContext(ctx).Model(&models.OpHistoryPO{}).Select("COUNT(*) AS total").Scan(&out.Total).Error; err != nil {
		return out, err
	}
	var byKind []struct {
		Kind  string `gorm:"column:kind"`
		Count int64  `gorm:"column:count"`
	}
	if err := r.db.WithContext(ctx).Model(&models.OpHistoryPO{}).Select(
		"kind", "COUNT(*) AS count",
	).Group("kind").Order("count DESC").Scan(&byKind).Error; err != nil {
		return out, err
	}
	out.ByKind = make([]history.KindStat, 0, len(byKind))
	for _, k := range byKind {
		out.ByKind = append(out.ByKind, history.KindStat{Kind: k.Kind, Count: k.Count})
	}
	return out, nil
}

func historyToBiz(p models.OpHistoryPO) history.Record {
	return history.Record{
		ID:        p.ID,
		Kind:      p.Kind,
		TaskID:    p.TaskID,
		AgentKey:  p.AgentKey,
		ConvID:    p.ConvID,
		Ref:       p.Ref,
		Query:     p.Query,
		Summary:   p.Summary,
		Detail:    p.Detail,
		OK:        p.OK,
		LatencyMS: p.LatencyMS,
		CreatedAt: p.CreatedAt,
	}
}
