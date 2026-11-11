// usage.go 成本中心 MySQL 仓储：usage_records 追加与聚合。
package repo

import (
	"context"
	"fmt"

	"go_agent/internal/biz/usage"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// UsageRepo 用量记录仓库
type UsageRepo struct {
	db *gorm.DB
}

var _ usage.Repo = (*UsageRepo)(nil)

// NewUsageRepo 创建仓库；auto_migrate 打开时建表。
func NewUsageRepo(c *conf.Data, d *db.Data) (usage.Repo, error) {
	r := &UsageRepo{db: d.DB()}
	if !c.GetDatabase().GetAutoMigrate() {
		return r, nil
	}
	if err := r.db.AutoMigrate(&models.UsageRecordPO{}); err != nil {
		return nil, fmt.Errorf("迁移 usage_records 表: %w", err)
	}
	return r, nil
}

// Add 追加一条用量记录（永不更新历史行）。
func (r *UsageRepo) Add(ctx context.Context, rec *usage.Record) error {
	po := &models.UsageRecordPO{
		TenantID:     rec.TenantID,
		SubjectID:    rec.SubjectID,
		AgentID:      rec.AgentID,
		TaskID:       rec.TaskID,
		Provider:     rec.Provider,
		Model:        rec.Model,
		Kind:         rec.Kind,
		InputTokens:  rec.InputTokens,
		OutputTokens: rec.OutputTokens,
		TotalTokens:  rec.TotalTokens,
		CostMicros:   rec.CostMicros,
		Currency:     rec.Currency,
		PriceVersion: rec.PriceVersion,
		CreatedAt:    rec.CreatedAt,
	}
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		return err
	}
	rec.ID = po.ID
	return nil
}

// Summary 按维度聚合。
func (r *UsageRepo) Summary(ctx context.Context, f usage.Filter) ([]usage.Bucket, error) {
	key, ok := groupExpr[f.GroupBy]
	if !ok {
		key = "''"
	}
	q := r.db.WithContext(ctx).Model(&models.UsageRecordPO{}).
		Select(key + " AS bucket_key, " +
			"COALESCE(SUM(input_tokens),0) AS input_tokens, " +
			"COALESCE(SUM(output_tokens),0) AS output_tokens, " +
			"COALESCE(SUM(total_tokens),0) AS total_tokens, " +
			"COALESCE(SUM(cost_micros),0) AS cost_micros, " +
			"COUNT(*) AS calls")
	if f.TenantID != "" {
		q = q.Where("tenant_id = ?", f.TenantID)
	}
	if f.AgentID != "" {
		q = q.Where("agent_id = ?", f.AgentID)
	}
	if f.SubjectID != "" {
		q = q.Where("subject_id = ?", f.SubjectID)
	}
	if f.TaskID != "" {
		q = q.Where("task_id = ?", f.TaskID)
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if !f.Since.IsZero() {
		q = q.Where("created_at >= ?", f.Since)
	}
	if !f.Until.IsZero() {
		q = q.Where("created_at < ?", f.Until)
	}
	var rows []usage.Bucket
	if err := q.Group("bucket_key").Order("cost_micros DESC").Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// groupExpr 分组表达式白名单：避免拼接用户输入导致注入。
var groupExpr = map[string]string{
	"":        "''",
	"tenant":  "tenant_id",
	"subject": "subject_id",
	"agent":   "agent_id",
	"task":    "task_id",
	"model":   "model",
	"kind":    "kind",
	"day":     "DATE_FORMAT(created_at, '%Y-%m-%d')",
}
