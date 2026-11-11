package repo

// worker MySQL 仓储：执行体注册表（worker_instances）。
//
// 表很小（每个进程一行），但承担两个判断：
//   - 谁还活着：expires_at 由心跳往后推，过期即失联；
//   - 谁手上压着活：running 由心跳带上，扩容/缩容看它。
//
// 心跳用 ON CONFLICT DO UPDATE 一条 SQL 完成：先查后写会在多副本同时启动时
// 撞唯一键（uk_worker_instances_key），表现为「新实例注册失败」而任务照跑，
// 之后谁也看不见这个实例。

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"
)

var _ agentfactory.WorkerRepo = (*WorkerRepo)(nil)

// WorkerRepo 执行体注册表仓储
type WorkerRepo struct {
	db *gorm.DB
}

// NewWorkerRepo 构造执行体注册表仓储（建表由 agentfactory 仓储的 AutoMigrate 统一负责）
func NewWorkerRepo(d *db.Data) (agentfactory.WorkerRepo, error) {
	return &WorkerRepo{db: d.DB()}, nil
}

// UpsertWorker 注册或刷新执行体。
// 冲突时只更新可变字段，started_at 保留首次注册时间：
// 否则同 key 重启会把「这个实例活了多久」抹掉，事故复盘时看不到重启次数。
func (r *WorkerRepo) UpsertWorker(ctx context.Context, w agentfactory.Worker) error {
	now := time.Now()
	row := models.WorkerInstancePO{
		WorkerKey:   w.Key,
		Host:        w.Host,
		Capacity:    w.Capacity,
		Running:     w.Running,
		Status:      orDefault(w.Status, agentfactory.WorkerOnline),
		StartedAt:   w.StartedAt,
		HeartbeatAt: now,
		ExpiresAt:   nilIfZero(w.ExpiresAt),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if row.StartedAt.IsZero() {
		row.StartedAt = now
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "worker_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"host", "capacity", "running", "status", "heartbeat_at", "expires_at", "updated_at",
		}),
	}).Create(&row).Error
}

// HeartbeatWorker 心跳：刷新到期时间与在执行任务数
func (r *WorkerRepo) HeartbeatWorker(ctx context.Context, key, status string, running int, expiresAt time.Time) error {
	return r.db.WithContext(ctx).Model(&models.WorkerInstancePO{}).
		Where("worker_key = ?", key).
		Updates(map[string]any{
			"status":       orDefault(status, agentfactory.WorkerOnline),
			"running":      running,
			"heartbeat_at": time.Now(),
			"expires_at":   expiresAt,
			"updated_at":   time.Now(),
		}).Error
}

// SetWorkerStatus 只改状态（draining / offline）
func (r *WorkerRepo) SetWorkerStatus(ctx context.Context, key, status string) error {
	if key == "" {
		return nil
	}
	return r.db.WithContext(ctx).Model(&models.WorkerInstancePO{}).
		Where("worker_key = ?", key).
		Updates(map[string]any{"status": status, "updated_at": time.Now()}).Error
}

// ListWorkers 全部执行体（含历史实例）：在线与否由 expires_at 判断，
// 保留离线实例是为了让看板能看到「这台机器曾经在跑」。
func (r *WorkerRepo) ListWorkers(ctx context.Context) ([]agentfactory.Worker, error) {
	var rows []models.WorkerInstancePO
	if err := r.db.WithContext(ctx).Order("started_at DESC").Limit(200).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]agentfactory.Worker, 0, len(rows))
	for _, row := range rows {
		w := agentfactory.Worker{
			Key:         row.WorkerKey,
			Host:        row.Host,
			Capacity:    row.Capacity,
			Running:     row.Running,
			Status:      row.Status,
			StartedAt:   row.StartedAt,
			HeartbeatAt: row.HeartbeatAt,
		}
		if row.ExpiresAt != nil {
			w.ExpiresAt = *row.ExpiresAt
		}
		out = append(out, w)
	}
	return out, nil
}

// MarkStaleWorkers 心跳过期且仍标记为在线/排空的执行体置 offline，返回本次下线数量。
// 不动已经是 offline 的行：否则每次清扫都会把它们再「下线」一次，日志刷屏。
func (r *WorkerRepo) MarkStaleWorkers(ctx context.Context, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Model(&models.WorkerInstancePO{}).
		Where("status <> ? AND expires_at IS NOT NULL AND expires_at < ?", agentfactory.WorkerOffline, now).
		Updates(map[string]any{"status": agentfactory.WorkerOffline, "updated_at": now})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}
