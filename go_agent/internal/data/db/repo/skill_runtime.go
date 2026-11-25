// skill_runtime.go 技能运行时的持久化：Worker 实例注册表 + 执行记录（账本 / 异步队列）。
//
// 与 skill.go 分开的原因：技能定义是配置面（低频写、读多），实例与执行记录是
// 运行面（高频写、有保留策略、会随流量增长）。同一个仓储结构体、不同的文件，
// 便于按需阅读与替换实现（换成 Redis 队列时只需替换本文件的 ClaimQueued）。
package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go_agent/internal/biz/skill"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SkillRuntimeRepo 技能运行时仓储
type SkillRuntimeRepo struct {
	db *gorm.DB
}

var _ skill.RuntimeStore = (*SkillRuntimeRepo)(nil)

// NewSkillRuntimeRepo 创建运行时仓储；auto_migrate 打开时自动建表
func NewSkillRuntimeRepo(c *conf.Data, d *db.Data) (skill.RuntimeStore, error) {
	r := &SkillRuntimeRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.SkillInstancePO{}, &models.SkillExecutionPO{}); err != nil {
			return nil, fmt.Errorf("迁移 skill 运行时表: %w", err)
		}
	}
	return r, nil
}

// dispatcherID 本进程标识：执行记录的 worker_id 列，用来判断「哪台机器领走了任务」
var dispatcherID = func() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}()

// —— Worker 实例 ——

// SaveInstance 注册/更新实例（同名同 ID 覆盖地址与容量，保留累计指标）
func (r *SkillRuntimeRepo) SaveInstance(ctx context.Context, in skill.Instance) (*skill.Instance, error) {
	now := time.Now()
	hb := in.LastHeartbeat
	if hb.IsZero() {
		hb = now
	}
	status := in.Status
	if status == "" {
		status = skill.InstanceOnline
	}
	po := &models.SkillInstancePO{
		SkillName:      in.SkillName,
		InstanceID:     in.ID,
		Endpoint:       in.Endpoint,
		Version:        in.Version,
		Region:         in.Region,
		Labels:         encodeTags(in.Labels),
		Weight:         in.Weight,
		MaxConcurrency: in.MaxConcurrency,
		Inflight:       in.Inflight,
		Status:         status,
		LastHeartbeat:  hb,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "skill_name"}, {Name: "instance_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"endpoint", "version", "region", "labels", "weight",
			"max_concurrency", "status", "last_heartbeat", "updated_at",
		}),
	}).Create(po).Error
	if err != nil {
		return nil, err
	}
	return r.GetInstance(ctx, in.SkillName, in.ID)
}

// ListInstances 实例列表（权重高的在前，便于前端展示主备关系）
func (r *SkillRuntimeRepo) ListInstances(ctx context.Context, skillName string) ([]skill.Instance, error) {
	var pos []models.SkillInstancePO
	if err := r.db.WithContext(ctx).Where("skill_name = ?", skillName).
		Order("weight DESC, latency_ms ASC, instance_id ASC").Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]skill.Instance, 0, len(pos))
	for i := range pos {
		out = append(out, instanceFromPO(&pos[i]))
	}
	return out, nil
}

// GetInstance 单个实例
func (r *SkillRuntimeRepo) GetInstance(ctx context.Context, skillName, id string) (*skill.Instance, error) {
	var po models.SkillInstancePO
	err := r.db.WithContext(ctx).Where("skill_name = ? AND instance_id = ?", skillName, id).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, skill.ErrNoWorker
	}
	if err != nil {
		return nil, err
	}
	out := instanceFromPO(&po)
	return &out, nil
}

// DeleteInstance 摘除实例
func (r *SkillRuntimeRepo) DeleteInstance(ctx context.Context, skillName, id string) error {
	res := r.db.WithContext(ctx).
		Where("skill_name = ? AND instance_id = ?", skillName, id).
		Delete(&models.SkillInstancePO{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return skill.ErrNoWorker
	}
	return nil
}

// Heartbeat 心跳：在途数 / 状态 / 存活时间
func (r *SkillRuntimeRepo) Heartbeat(ctx context.Context, hb skill.InstanceHeartbeat) error {
	now := time.Now()
	status := strings.TrimSpace(hb.Status)
	updates := map[string]any{
		"inflight":       hb.Inflight,
		"last_heartbeat": now,
		"updated_at":     now,
	}
	if status != "" {
		updates["status"] = status
	}
	if hb.LatencyMS > 0 {
		updates["latency_ms"] = hb.LatencyMS
	}
	res := r.db.WithContext(ctx).Model(&models.SkillInstancePO{}).
		Where("skill_name = ? AND instance_id = ?", hb.SkillName, hb.ID).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 心跳打到一个没注册的实例：让 Worker 侧知道要重新注册，而不是静默丢心跳
		return skill.ErrNoWorker
	}
	return nil
}

// RecordResult 调用结果 → 实例健康度。
//
// 成功：连续失败清零、成功数 +1、延迟做 EWMA（0.7 旧 + 0.3 新，抗抖动）；
// 失败：连续失败 +1 并记原因，达到阈值后调度器会摘掉它（熔断）。
func (r *SkillRuntimeRepo) RecordResult(ctx context.Context, skillName, instanceID string, ok bool, latencyMS int64, errMsg string) error {
	updates := map[string]any{"updated_at": time.Now()}
	if ok {
		updates["failures"] = 0
		updates["successes"] = gorm.Expr("successes + 1")
		updates["last_error"] = ""
		if latencyMS > 0 {
			updates["latency_ms"] = gorm.Expr("ROUND(latency_ms * 0.7 + ? * 0.3)", latencyMS)
		}
	} else {
		updates["failures"] = gorm.Expr("failures + 1")
		updates["last_error"] = clip(errMsg, 500)
	}
	return r.db.WithContext(ctx).Model(&models.SkillInstancePO{}).
		Where("skill_name = ? AND instance_id = ?", skillName, instanceID).
		Updates(updates).Error
}

// —— 执行记录 ——

// CreateExecution 落一条执行记录
func (r *SkillRuntimeRepo) CreateExecution(ctx context.Context, e *skill.Execution) error {
	now := time.Now()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	return r.db.WithContext(ctx).Create(executionToPO(e)).Error
}

// GetExecution 按 ID 查询
func (r *SkillRuntimeRepo) GetExecution(ctx context.Context, id string) (*skill.Execution, error) {
	var po models.SkillExecutionPO
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, skill.ErrExecutionNotFound
	}
	if err != nil {
		return nil, err
	}
	return executionFromPO(&po), nil
}

// UpdateExecution 更新执行状态（只更新运行期字段，不动 input/created_at）
func (r *SkillRuntimeRepo) UpdateExecution(ctx context.Context, e *skill.Execution) error {
	if e == nil || e.ID == "" {
		return skill.ErrExecutionNotFound
	}
	res := r.db.WithContext(ctx).Model(&models.SkillExecutionPO{}).Where("id = ?", e.ID).
		Updates(map[string]any{
			"status":      e.Status,
			"output":      clip(e.Output, 256<<10),
			"error":       clip(e.Error, 1024),
			"worker_id":   e.WorkerID,
			"attempts":    e.Attempts,
			"latency_ms":  e.LatencyMS,
			"lease_until": e.LeaseUntil,
			"started_at":  e.StartedAt,
			"finished_at": e.FinishedAt,
			"updated_at":  time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return skill.ErrExecutionNotFound
	}
	e.UpdatedAt = time.Now()
	return nil
}

// ListExecutions 执行记录列表
func (r *SkillRuntimeRepo) ListExecutions(ctx context.Context, f skill.ExecutionFilter) ([]skill.Execution, error) {
	q := r.db.WithContext(ctx).Model(&models.SkillExecutionPO{})
	if f.SkillName != "" {
		q = q.Where("skill_name = ?", f.SkillName)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.TaskID != "" {
		q = q.Where("task_id = ?", f.TaskID)
	}
	if f.TenantID != "" {
		q = q.Where("tenant_id = ?", f.TenantID)
	}
	if f.UserID != "" {
		q = q.Where("user_id = ?", f.UserID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var pos []models.SkillExecutionPO
	if err := q.Order("created_at DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]skill.Execution, 0, len(pos))
	for i := range pos {
		out = append(out, *executionFromPO(&pos[i]))
	}
	return out, nil
}

// FindExecutionByKey 幂等键查历史成功记录（回放用）。
//
// 查询带上 tenant_id：幂等键是调用方给的字符串，租户间撞键（都填 "1"、
// 都用同一个业务单号）完全可能，少了这一条就会把 A 租户的结果回放给 B。
func (r *SkillRuntimeRepo) FindExecutionByKey(ctx context.Context, tenantID, skillName, key string) (*skill.Execution, error) {
	if key == "" {
		return nil, skill.ErrExecutionNotFound
	}
	var po models.SkillExecutionPO
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND skill_name = ? AND idempotency_key = ? AND status = ?",
			tenantOrPublic(tenantID), skillName, key, skill.ExecSucceeded).
		Order("created_at DESC").First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, skill.ErrExecutionNotFound
	}
	if err != nil {
		return nil, err
	}
	return executionFromPO(&po), nil
}

// ClaimQueued 原子领取待执行任务。
//
// FOR UPDATE SKIP LOCKED：多副本同时轮询时各自领到不同的行，不会出现
// 「两个进程同时跑同一条任务」；租约是二重保险（进程在领取后崩溃，
// 租约过期由 RequeueStale 退回 queued）。
func (r *SkillRuntimeRepo) ClaimQueued(ctx context.Context, limit int, lease time.Duration) ([]skill.Execution, error) {
	if limit <= 0 {
		limit = 1
	}
	var claimed []models.SkillExecutionPO
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pos []models.SkillExecutionPO
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", skill.ExecQueued).
			Order("created_at ASC").Limit(limit).Find(&pos).Error
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return nil
		}
		ids := make([]string, 0, len(pos))
		for i := range pos {
			ids = append(ids, pos[i].ID)
		}
		now := time.Now()
		leaseUntil := now.Add(lease)
		if err := tx.Model(&models.SkillExecutionPO{}).Where("id IN ?", ids).
			Updates(map[string]any{
				"status":      skill.ExecRunning,
				"started_at":  now,
				"lease_until": leaseUntil,
				"worker_id":   dispatcherID,
				"updated_at":  now,
			}).Error; err != nil {
			return err
		}
		claimed = pos
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]skill.Execution, 0, len(claimed))
	now := time.Now()
	for i := range claimed {
		e := executionFromPO(&claimed[i])
		e.Status = skill.ExecRunning
		e.StartedAt = &now
		e.WorkerID = dispatcherID
		out = append(out, *e)
	}
	return out, nil
}

// RequeueStale 租约过期的 running 退回 queued（派发进程崩溃兜底）
func (r *SkillRuntimeRepo) RequeueStale(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	var ids []string
	if err := r.db.WithContext(ctx).Model(&models.SkillExecutionPO{}).
		Where("status = ? AND lease_until IS NOT NULL AND lease_until < ?", skill.ExecRunning, before).
		Order("lease_until ASC").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	now := time.Now()
	err := r.db.WithContext(ctx).Model(&models.SkillExecutionPO{}).Where("id IN ?", ids).
		Updates(map[string]any{
			"status":      skill.ExecQueued,
			"lease_until": nil,
			"worker_id":   "",
			"updated_at":  now,
		}).Error
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// PurgeExecutions 清理终态且过期的执行记录（保留策略）
func (r *SkillRuntimeRepo) PurgeExecutions(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	var ids []string
	if err := r.db.WithContext(ctx).Model(&models.SkillExecutionPO{}).
		Where("status IN ? AND finished_at IS NOT NULL AND finished_at < ?",
			[]string{skill.ExecSucceeded, skill.ExecFailed, skill.ExecTimeout, skill.ExecCanceled}, before).
		Order("finished_at ASC").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := r.db.WithContext(ctx).Where("id IN ?", ids).Delete(&models.SkillExecutionPO{}).Error; err != nil {
		return 0, err
	}
	return len(ids), nil
}

// —— 转换 ——

func instanceFromPO(po *models.SkillInstancePO) skill.Instance {
	return skill.Instance{
		ID:             po.InstanceID,
		SkillName:      po.SkillName,
		Endpoint:       po.Endpoint,
		Version:        po.Version,
		Region:         po.Region,
		Labels:         decodeList(po.Labels),
		Weight:         po.Weight,
		MaxConcurrency: po.MaxConcurrency,
		Inflight:       po.Inflight,
		Status:         po.Status,
		LastHeartbeat:  po.LastHeartbeat,
		LatencyMS:      po.LatencyMS,
		Failures:       po.Failures,
		Successes:      po.Successes,
		LastError:      po.LastError,
		CreatedAt:      po.CreatedAt,
		UpdatedAt:      po.UpdatedAt,
	}
}

func executionToPO(e *skill.Execution) *models.SkillExecutionPO {
	return &models.SkillExecutionPO{
		ID:             e.ID,
		TaskID:         e.TaskID,
		SkillName:      e.SkillName,
		SkillVersion:   e.SkillVersion,
		Mode:           e.Mode,
		Status:         e.Status,
		TenantID:       e.TenantID,
		UserID:         e.UserID,
		AgentKey:       e.AgentKey,
		SessionID:      e.SessionID,
		IdempotencyKey: e.IdempotencyKey,
		Input:          clip(e.Input, 64<<10),
		Output:         clip(e.Output, 256<<10),
		Error:          clip(e.Error, 1024),
		WorkerID:       e.WorkerID,
		Attempts:       e.Attempts,
		LatencyMS:      e.LatencyMS,
		TraceID:        e.TraceID,
		LeaseUntil:     e.LeaseUntil,
		QueuedAt:       e.QueuedAt,
		StartedAt:      e.StartedAt,
		FinishedAt:     e.FinishedAt,
		CreatedAt:      e.CreatedAt,
		UpdatedAt:      e.UpdatedAt,
	}
}

func executionFromPO(po *models.SkillExecutionPO) *skill.Execution {
	return &skill.Execution{
		ID:             po.ID,
		TaskID:         po.TaskID,
		SkillName:      po.SkillName,
		SkillVersion:   po.SkillVersion,
		Mode:           po.Mode,
		Status:         po.Status,
		TenantID:       po.TenantID,
		UserID:         po.UserID,
		AgentKey:       po.AgentKey,
		SessionID:      po.SessionID,
		IdempotencyKey: po.IdempotencyKey,
		Input:          po.Input,
		Output:         po.Output,
		Error:          po.Error,
		WorkerID:       po.WorkerID,
		Attempts:       po.Attempts,
		LatencyMS:      po.LatencyMS,
		TraceID:        po.TraceID,
		LeaseUntil:     po.LeaseUntil,
		QueuedAt:       po.QueuedAt,
		StartedAt:      po.StartedAt,
		FinishedAt:     po.FinishedAt,
		CreatedAt:      po.CreatedAt,
		UpdatedAt:      po.UpdatedAt,
	}
}

// tenantOrPublic 空租户归到 public（与 skill_executions.tenant_id 的列默认值一致）
func tenantOrPublic(tenantID string) string {
	if t := strings.TrimSpace(tenantID); t != "" {
		return t
	}
	return "public"
}
