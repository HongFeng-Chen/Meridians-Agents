// llmcall.go LLM 调用历史 MySQL 仓储：llm_calls 表。
// 写路径由 llm.Manager 异步调用（每次数据源尝试一条），错误只告警不断业务。
package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// LLMCallRepo LLM 调用历史仓库
type LLMCallRepo struct {
	db *gorm.DB
}

var _ llm.CallRepo = (*LLMCallRepo)(nil)

// NewLLMCallRepo 创建仓库；auto_migrate 打开时自动建表
func NewLLMCallRepo(c *conf.Data, d *db.Data) (*LLMCallRepo, error) {
	r := &LLMCallRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.LLMCallPO{}); err != nil {
			return nil, fmt.Errorf("迁移 llm_calls 表: %w", err)
		}
	}
	return r, nil
}

func (r *LLMCallRepo) Insert(ctx context.Context, rec llm.CallRecord) error {
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	row := models.LLMCallPO{
		TenantID:         rec.TenantID,
		UserID:           rec.UserID,
		Provider:         rec.Provider,
		Model:            rec.Model,
		Scene:            rec.Scene,
		Purpose:          rec.Purpose,
		TaskID:           rec.TaskID,
		AgentKey:         rec.AgentKey,
		Temperature:      rec.Temperature,
		DurationMS:       rec.DurationMS,
		OK:               rec.OK,
		ErrorMsg:         rec.ErrorMsg,
		PromptChars:      rec.PromptChars,
		ReplyChars:       rec.ReplyChars,
		PromptText:       rec.PromptText,
		ReplyText:        rec.ReplyText,
		PromptTokens:     rec.Usage.PromptTokens,
		CompletionTokens: rec.Usage.CompletionTokens,
		TotalTokens:      rec.Usage.TotalTokens,
		CreatedAt:        rec.CreatedAt,
	}
	return r.db.WithContext(ctx).Create(&row).Error
}

// List 调用历史（时间倒序 + 总数）
func (r *LLMCallRepo) List(ctx context.Context, f llm.CallFilter) ([]llm.CallRecord, int64, error) {
	q := r.db.WithContext(ctx).Model(&models.LLMCallPO{})
	if uid := strings.TrimSpace(f.UserID); uid != "" {
		q = q.Where("user_id = ?", uid)
	}
	if tid := strings.TrimSpace(f.TenantID); tid != "" {
		q = q.Where("tenant_id = ?", tid)
	}
	if f.TaskID > 0 {
		q = q.Where("task_id = ?", f.TaskID)
	}
	if f.Model != "" {
		q = q.Where("model = ?", f.Model)
	}
	if len(f.Models) > 0 {
		q = q.Where("model IN ?", f.Models)
	}
	if f.Provider != "" {
		q = q.Where("provider = ?", f.Provider)
	}
	if f.Purpose != "" {
		q = q.Where("purpose = ?", f.Purpose)
	}
	if f.OK != nil {
		q = q.Where("ok = ?", *f.OK)
	}
	if !f.Since.IsZero() {
		q = q.Where("created_at >= ?", f.Since)
	}
	if !f.Until.IsZero() {
		q = q.Where("created_at <= ?", f.Until)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 2000 {
		f.Limit = 2000
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	var pos []models.LLMCallPO
	if err := q.Order("created_at DESC, id DESC").Limit(f.Limit).Offset(f.Offset).Find(&pos).Error; err != nil {
		return nil, 0, err
	}
	out := make([]llm.CallRecord, 0, len(pos))
	for _, p := range pos {
		out = append(out, callToBiz(p))
	}
	return out, total, nil
}

// Stats 全局统计：总量 / token 三项 / 失败数 / 按模型聚合（TOP 10）
func (r *LLMCallRepo) Stats(ctx context.Context, userID string) (llm.CallStats, error) {
	var out llm.CallStats
	scope := func(q *gorm.DB) *gorm.DB {
		if uid := strings.TrimSpace(userID); uid != "" {
			return q.Where("user_id = ?", uid)
		}
		return q
	}
	var totals struct {
		Calls            int64 `gorm:"column:calls"`
		Failures         int64 `gorm:"column:failures"`
		PromptTokens     int64 `gorm:"column:prompt_tokens"`
		CompletionTokens int64 `gorm:"column:completion_tokens"`
		TotalTokens      int64 `gorm:"column:total_tokens"`
	}
	if err := scope(r.db.WithContext(ctx).Model(&models.LLMCallPO{})).Select(
		"COUNT(*) AS calls",
		"COALESCE(SUM(ok = false), 0) AS failures",
		"COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens",
		"COALESCE(SUM(completion_tokens), 0) AS completion_tokens",
		"COALESCE(SUM(total_tokens), 0) AS total_tokens",
	).Scan(&totals).Error; err != nil {
		return out, err
	}
	out.Calls = totals.Calls
	out.Failures = totals.Failures
	out.PromptTokens = totals.PromptTokens
	out.CompletionTokens = totals.CompletionTokens
	out.TotalTokens = totals.TotalTokens

	var byModel []struct {
		Model            string `gorm:"column:model"`
		Calls            int64  `gorm:"column:calls"`
		PromptTokens     int64  `gorm:"column:prompt_tokens"`
		CompletionTokens int64  `gorm:"column:completion_tokens"`
	}
	if err := scope(r.db.WithContext(ctx).Model(&models.LLMCallPO{})).Select(
		"model",
		"COUNT(*) AS calls",
		"COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens",
		"COALESCE(SUM(completion_tokens), 0) AS completion_tokens",
	).Group("model").Order("calls DESC").Limit(10).Scan(&byModel).Error; err != nil {
		return out, err
	}
	out.ByModel = make([]llm.ModelUsage, 0, len(byModel))
	for _, m := range byModel {
		out.ByModel = append(out.ByModel, llm.ModelUsage{
			Model:            m.Model,
			Calls:            m.Calls,
			PromptTokens:     m.PromptTokens,
			CompletionTokens: m.CompletionTokens,
		})
	}
	return out, nil
}

func callToBiz(p models.LLMCallPO) llm.CallRecord {
	return llm.CallRecord{
		ID:          p.ID,
		TenantID:    p.TenantID,
		UserID:      p.UserID,
		Provider:    p.Provider,
		Model:       p.Model,
		Scene:       p.Scene,
		Purpose:     p.Purpose,
		TaskID:      p.TaskID,
		AgentKey:    p.AgentKey,
		Temperature: p.Temperature,
		DurationMS:  p.DurationMS,
		OK:          p.OK,
		ErrorMsg:    p.ErrorMsg,
		PromptChars: p.PromptChars,
		ReplyChars:  p.ReplyChars,
		PromptText:  p.PromptText,
		ReplyText:   p.ReplyText,
		Usage: llm.Usage{
			PromptTokens:     p.PromptTokens,
			CompletionTokens: p.CompletionTokens,
			TotalTokens:      p.TotalTokens,
		},
		CreatedAt: p.CreatedAt,
	}
}

// Breakdown token 用量下钻：按维度聚合（total_tokens 降序 TOP 15）。
// dim 必须在白名单内（防注入），days > 0 时只统计最近 N 天。
func (r *LLMCallRepo) Breakdown(ctx context.Context, dim string, days int) ([]llm.UsageRow, error) {
	if !llm.BreakdownDims[dim] {
		dim = "agent_key"
	}
	q := r.db.WithContext(ctx).Model(&models.LLMCallPO{})
	if days > 0 {
		q = q.Where("created_at >= ?", time.Now().AddDate(0, 0, -days))
	}
	var rows []struct {
		Key              string `gorm:"column:dim_key"`
		Calls            int64  `gorm:"column:calls"`
		PromptTokens     int64  `gorm:"column:prompt_tokens"`
		CompletionTokens int64  `gorm:"column:completion_tokens"`
		TotalTokens      int64  `gorm:"column:total_tokens"`
		Failures         int64  `gorm:"column:failures"`
	}
	if err := q.Select(
		"COALESCE(NULLIF("+dim+", ''), '(未标注)') AS dim_key",
		"COUNT(*) AS calls",
		"COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens",
		"COALESCE(SUM(completion_tokens), 0) AS completion_tokens",
		"COALESCE(SUM(total_tokens), 0) AS total_tokens",
		"COALESCE(SUM(ok = false), 0) AS failures",
	).Group("dim_key").Order("total_tokens DESC").Limit(15).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]llm.UsageRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, llm.UsageRow{
			Key:              row.Key,
			Calls:            row.Calls,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			TotalTokens:      row.TotalTokens,
			Failures:         row.Failures,
		})
	}
	return out, nil
}
