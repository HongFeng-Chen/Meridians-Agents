// audit.go 审计事件仓储：audit_events 表的批量写入与条件检索。
//
// 写入路径是异步批量的（biz/audit 的 UseCase 负责攒批），所以这里只做
// 「一次事务插一批」；查询路径按时间倒序走 idx_audit_events_ts。
package repo

import (
	"context"
	"fmt"
	"time"

	"go_agent/internal/biz/audit"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// AuditRepo 审计仓储
type AuditRepo struct {
	db *gorm.DB
}

var _ audit.Repo = (*AuditRepo)(nil)

// NewAuditRepo 创建仓储；auto_migrate 打开时自动建表
func NewAuditRepo(c *conf.Data, d *db.Data) (audit.Repo, error) {
	r := &AuditRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.AuditEventPO{}); err != nil {
			return nil, fmt.Errorf("迁移 audit_events 表: %w", err)
		}
	}
	return r, nil
}

// BatchInsert 批量写入（单条 INSERT，减少往返）
func (r *AuditRepo) BatchInsert(ctx context.Context, records []audit.Record) error {
	if len(records) == 0 {
		return nil
	}
	rows := make([]models.AuditEventPO, 0, len(records))
	for _, rec := range records {
		rows = append(rows, models.AuditEventPO{
			Ts:         rec.Ts,
			Action:     rec.Action,
			Result:     rec.Result,
			Resource:   rec.Resource,
			ResourceID: rec.ResourceID,
			Risk:       rec.Risk,
			UserID:     rec.UserID,
			TenantID:   rec.TenantID,
			AuthType:   rec.AuthType,
			Admin:      rec.Admin,
			Local:      rec.Local,
			IP:         rec.IP,
			Detail:     rec.Detail,
			TraceID:    rec.TraceID,
			LatencyMS:  rec.LatencyMS,
		})
	}
	return r.db.WithContext(ctx).CreateInBatches(&rows, 200).Error
}

// List 条件检索（时间倒序）
func (r *AuditRepo) List(ctx context.Context, f audit.Filter) ([]audit.Record, error) {
	q := r.db.WithContext(ctx).Model(&models.AuditEventPO{})
	if f.Action != "" {
		q = q.Where("action = ?", f.Action)
	}
	if f.Result != "" {
		q = q.Where("result = ?", f.Result)
	}
	if f.Resource != "" {
		q = q.Where("resource = ?", f.Resource)
	}
	if f.UserID != "" {
		q = q.Where("user_id = ?", f.UserID)
	}
	if !f.Since.IsZero() {
		q = q.Where("ts >= ?", f.Since)
	}
	if !f.Until.IsZero() {
		q = q.Where("ts <= ?", f.Until)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []models.AuditEventPO
	if err := q.Order("ts DESC, id DESC").Limit(limit).Offset(f.Offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]audit.Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, audit.Record{
			ID:         row.ID,
			Ts:         row.Ts,
			Action:     row.Action,
			Result:     row.Result,
			Resource:   row.Resource,
			ResourceID: row.ResourceID,
			Risk:       row.Risk,
			UserID:     row.UserID,
			TenantID:   row.TenantID,
			AuthType:   row.AuthType,
			Admin:      row.Admin,
			Local:      row.Local,
			IP:         row.IP,
			Detail:     row.Detail,
			TraceID:    row.TraceID,
			LatencyMS:  row.LatencyMS,
		})
	}
	return out, nil
}

// Purge 删除 before 之前的事件（保留策略由 biz/audit 控制）
func (r *AuditRepo) Purge(ctx context.Context, before time.Time) (int64, error) {
	res := r.db.WithContext(ctx).
		Where("ts < ?", before).
		Delete(&models.AuditEventPO{})
	return res.RowsAffected, res.Error
}
