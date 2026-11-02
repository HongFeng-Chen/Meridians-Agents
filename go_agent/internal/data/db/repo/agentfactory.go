package repo

// agentfactory MySQL 仓储：Agent 定义 / 任务 / 事件。
//
// 约定同 mcp.go：JSON 列空集合必须写 `[]` / `{}`（MySQL JSON 列拒收空串）、
// NotFound 哨兵错误映射、软删除。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"
)

var _ agentfactory.Repo = (*AgentFactoryRepo)(nil)

type AgentFactoryRepo struct {
	db *gorm.DB
}

func NewAgentFactoryRepo(c *conf.Data, d *db.Data) (agentfactory.Repo, error) {
	r := &AgentFactoryRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(
			&models.AgentDefPO{},
			&models.AgentTaskPO{},
			&models.AgentTaskHistoryPO{},
			&models.AgentTaskEventPO{},
		); err != nil {
			return nil, fmt.Errorf("迁移 agentfactory 表: %w", err)
		}
	}
	// 一次性回填：把历史版本遗留在任务池中的终态任务迁入历史表（幂等）
	if err := r.backfillHistory(context.Background()); err != nil {
		return nil, fmt.Errorf("回填任务历史表: %w", err)
	}
	return r, nil
}

// ———— Agent 定义 ————

func (r *AgentFactoryRepo) ListDefs(ctx context.Context, f agentfactory.DefFilter) ([]agentfactory.AgentDef, error) {
	q := r.db.WithContext(ctx).Model(&models.AgentDefPO{})
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		q = q.Where("(agent_key LIKE ? ESCAPE '\\\\' OR name LIKE ? ESCAPE '\\\\' OR description LIKE ? ESCAPE '\\\\')", like, like, like)
	}
	if src := strings.TrimSpace(f.Source); src != "" {
		q = q.Where("source = ?", src)
	}
	if f.Enabled != nil {
		q = q.Where("is_enabled = ?", *f.Enabled)
	}
	if len(f.IDs) > 0 {
		q = q.Where("id IN ?", f.IDs)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var rows []models.AgentDefPO
	if err := q.Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]agentfactory.AgentDef, 0, len(rows))
	for _, row := range rows {
		out = append(out, defToBiz(row))
	}
	return out, nil
}

func (r *AgentFactoryRepo) GetDef(ctx context.Context, id uint64) (*agentfactory.AgentDef, error) {
	var row models.AgentDefPO
	err := r.db.WithContext(ctx).First(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, agentfactory.ErrDefNotFound
	}
	if err != nil {
		return nil, err
	}
	def := defToBiz(row)
	return &def, nil
}

func (r *AgentFactoryRepo) GetDefByKey(ctx context.Context, key string) (*agentfactory.AgentDef, error) {
	var row models.AgentDefPO
	err := r.db.WithContext(ctx).Where("agent_key = ?", strings.TrimSpace(strings.ToLower(key))).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil // 不存在不是错误，调用方决定语义
	}
	if err != nil {
		return nil, err
	}
	def := defToBiz(row)
	return &def, nil
}

func (r *AgentFactoryRepo) CountDefs(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&models.AgentDefPO{}).Count(&n).Error
	return n, err
}

func (r *AgentFactoryRepo) CreateDef(ctx context.Context, def *agentfactory.AgentDef) error {
	row := defFromBiz(*def)
	now := time.Now()
	row.CreatedAt = now
	row.UpdatedAt = now
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	*def = defToBiz(row)
	return nil
}

func (r *AgentFactoryRepo) UpdateDef(ctx context.Context, def *agentfactory.AgentDef) error {
	row := defFromBiz(*def)
	row.UpdatedAt = time.Now()
	res := r.db.WithContext(ctx).Model(&models.AgentDefPO{}).
		Where("id = ?", def.ID).
		Updates(map[string]any{
			"agent_key":         row.AgentKey,
			"name":              row.Name,
			"description":       row.Description,
			"system":            row.System,
			"llm_model":         row.LLMModel,
			"llm_source":        row.LLMSource,
			"graph_type":        row.GraphType,
			"capabilities":      row.Capabilities,
			"middlewares":       row.Middlewares,
			"max_turns":         row.MaxTurns,
			"role":              row.Role,
			"orchestration":     row.Orchestration,
			"members":           row.Members,
			"flow_json":         row.Flow, // 注意：PO 列名是 flow_json（biz 字段叫 Flow），map 更新不走 gorm tag 映射，写错即 1054
			"supervisor_key":    row.SupervisorKey,
			"supervision_scope": row.SupervisionScope,
			"risk_agent_key":    row.RiskAgentKey,
			"auto_run":          row.AutoRun,
			"cron_expr":         row.CronExpr,
			"cron_input":        row.CronInput,
			"next_run_at":       row.NextRunAt,
			"last_run_at":       row.LastRunAt,
			"is_sandbox":        row.Sandbox,
			"version":           row.Version,
			"is_vectorized":     row.Vectorized,
			"updated_at":        row.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return agentfactory.ErrDefNotFound
	}
	return nil
}

func (r *AgentFactoryRepo) DeleteDef(ctx context.Context, id uint64) error {
	res := r.db.WithContext(ctx).Delete(&models.AgentDefPO{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return agentfactory.ErrDefNotFound
	}
	return nil
}

func (r *AgentFactoryRepo) SetDefEnabled(ctx context.Context, id uint64, enabled bool) error {
	res := r.db.WithContext(ctx).Model(&models.AgentDefPO{}).
		Where("id = ?", id).
		Updates(map[string]any{"is_enabled": enabled, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return agentfactory.ErrDefNotFound
	}
	return nil
}

func (r *AgentFactoryRepo) MarkDefsVectorized(ctx context.Context, ids []uint64, ok bool) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&models.AgentDefPO{}).
		Where("id IN ?", ids).
		Update("is_vectorized", ok).Error
}

// ———— 任务 ————

func (r *AgentFactoryRepo) CreateTask(ctx context.Context, t *agentfactory.Task) error {
	row := taskFromBiz(*t)
	row.CreatedAt = time.Now()
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	*t = taskToBiz(row)
	return nil
}

// SaveTask 全量保存（状态/进度/结果；事件流另行 Append）。
// 终态任务不允许留在任务池：直接走 FinishTask 迁移到历史表。
func (r *AgentFactoryRepo) SaveTask(ctx context.Context, t *agentfactory.Task) error {
	if agentfactory.IsTerminalStatus(t.Status) {
		return r.FinishTask(ctx, t)
	}
	row := taskFromBiz(*t)
	res := r.db.WithContext(ctx).Model(&models.AgentTaskPO{}).
		Where("id = ?", t.ID).
		Updates(map[string]any{
			"status":      row.Status,
			"progress":    row.Progress,
			"result":      row.Result,
			"error_msg":   row.ErrorMsg,
			"started_at":  row.StartedAt,
			"finished_at": row.FinishedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return agentfactory.ErrTaskNotFound
	}
	return nil
}

// FinishTask 任务到达终态：同一事务内「更新任务池终态字段 → 复制进历史表 →
// 从任务池删除」，保证任一时刻任务只存在一张表中。
func (r *AgentFactoryRepo) FinishTask(ctx context.Context, t *agentfactory.Task) error {
	row := taskFromBiz(*t)
	finished := row.FinishedAt
	if finished == nil {
		now := time.Now()
		finished = &now
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先落任务池终态字段（库内数据为迁移源，避免依赖内存态）
		res := tx.Model(&models.AgentTaskPO{}).
			Where("id = ?", t.ID).
			Updates(map[string]any{
				"status":      row.Status,
				"progress":    row.Progress,
				"result":      row.Result,
				"error_msg":   row.ErrorMsg,
				"started_at":  row.StartedAt,
				"finished_at": finished,
			})
		if res.Error != nil {
			return res.Error
		}
		var src models.AgentTaskPO
		if err := tx.First(&src, t.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// 已迁移过（重试/并发终态写）：幂等成功
				return nil
			}
			return err
		}
		hist := taskToHistoryPO(src)
		hist.ArchivedAt = time.Now()
		if err := tx.Create(&hist).Error; err != nil {
			return err
		}
		// 物理删除：历史表已留全量副本，任务池只保留活跃任务保持精简
		return tx.Unscoped().Delete(&models.AgentTaskPO{}, t.ID).Error
	})
}

// ReviveTask 重试归档任务：同一事务内「插回任务池（重置 pending）+ 删历史」
func (r *AgentFactoryRepo) ReviveTask(ctx context.Context, t *agentfactory.Task) error {
	row := taskFromBiz(*t)
	row.Status = agentfactory.TaskPending
	row.Progress = 0
	row.Result = ""
	row.ErrorMsg = ""
	row.StartedAt = nil
	row.FinishedAt = nil
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 同 ID 插回任务池（沿用原 ID，trace 链路不断）
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		res := tx.Delete(&models.AgentTaskHistoryPO{}, t.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return agentfactory.ErrTaskNotFound // 历史表中已不存在（并发重试等）
		}
		return nil
	})
}

// ClaimTask 原子认领：pending → running（并发 worker 安全）
func (r *AgentFactoryRepo) ClaimTask(ctx context.Context, id uint64) (bool, error) {
	res := r.db.WithContext(ctx).Model(&models.AgentTaskPO{}).
		Where("id = ? AND status = ?", id, agentfactory.TaskPending).
		Updates(map[string]any{"status": agentfactory.TaskRunning, "started_at": time.Now()})
	return res.RowsAffected == 1, res.Error
}

// ListTasks 任务列表：状态过滤决定查任务池还是历史表；
// 未指定状态时两表合并（按 ID 倒序，取前 limit 条）。
func (r *AgentFactoryRepo) ListTasks(ctx context.Context, f agentfactory.TaskFilter) ([]agentfactory.Task, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	switch {
	case f.Status == agentfactory.TaskPending || f.Status == agentfactory.TaskRunning:
		return r.listPoolTasks(ctx, f, limit)
	case agentfactory.IsTerminalStatus(f.Status):
		return r.listHistoryTasks(ctx, f, limit)
	default:
		if f.Status != "" {
			return nil, fmt.Errorf("%w: 未知任务状态 %q", agentfactory.ErrInvalidTask, f.Status)
		}
		pool, err := r.listPoolTasks(ctx, f, limit)
		if err != nil {
			return nil, err
		}
		hist, err := r.listHistoryTasks(ctx, f, limit)
		if err != nil {
			return nil, err
		}
		merged := append(pool, hist...)
		sort.Slice(merged, func(i, j int) bool { return merged[i].ID > merged[j].ID })
		if len(merged) > limit {
			merged = merged[:limit]
		}
		return merged, nil
	}
}

func (r *AgentFactoryRepo) listPoolTasks(ctx context.Context, f agentfactory.TaskFilter, limit int) ([]agentfactory.Task, error) {
	q := r.db.WithContext(ctx).Model(&models.AgentTaskPO{})
	q = applyTaskFilter(q, f)
	var rows []models.AgentTaskPO
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]agentfactory.Task, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskToBiz(row))
	}
	return out, nil
}

func (r *AgentFactoryRepo) listHistoryTasks(ctx context.Context, f agentfactory.TaskFilter, limit int) ([]agentfactory.Task, error) {
	q := r.db.WithContext(ctx).Model(&models.AgentTaskHistoryPO{})
	q = applyTaskFilter(q, f)
	var rows []models.AgentTaskHistoryPO
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]agentfactory.Task, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskHistoryToBiz(row))
	}
	return out, nil
}

// applyTaskFilter 列表过滤条件（任务池与历史表同构列）
func applyTaskFilter(q *gorm.DB, f agentfactory.TaskFilter) *gorm.DB {
	if f.AgentDefID > 0 {
		q = q.Where("agent_def_id = ?", f.AgentDefID)
	}
	if key := strings.TrimSpace(f.AgentKey); key != "" {
		q = q.Where("agent_key = ?", strings.ToLower(key))
	}
	if f.ParentTaskID > 0 {
		q = q.Where("parent_task_id = ?", f.ParentTaskID)
	}
	if f.RootOnly {
		q = q.Where("parent_task_id = 0")
	}
	if cid := strings.TrimSpace(f.ConvID); cid != "" {
		q = q.Where("conv_id = ?", cid)
	}
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		q = q.Where("(title LIKE ? ESCAPE '\\\\' OR input LIKE ? ESCAPE '\\\\' OR agent_key LIKE ? ESCAPE '\\\\')", like, like, like)
	}
	return q
}

// GetTask 任务详情：先查任务池（活跃任务），未命中再查任务历史表
func (r *AgentFactoryRepo) GetTask(ctx context.Context, id uint64) (*agentfactory.Task, error) {
	var row models.AgentTaskPO
	err := r.db.WithContext(ctx).First(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 任务池未命中 → 历史表
		var hist models.AgentTaskHistoryPO
		err2 := r.db.WithContext(ctx).First(&hist, id).Error
		if errors.Is(err2, gorm.ErrRecordNotFound) {
			return nil, agentfactory.ErrTaskNotFound
		}
		if err2 != nil {
			return nil, err2
		}
		t := taskHistoryToBiz(hist)
		return &t, nil
	}
	if err != nil {
		return nil, err
	}
	t := taskToBiz(row)
	return &t, nil
}

// CountTasksByStatus 各状态任务数（任务池 + 任务历史合并）
func (r *AgentFactoryRepo) CountTasksByStatus(ctx context.Context) (map[string]int64, error) {
	var rows []struct {
		Status string `gorm:"column:status"`
		N      int64  `gorm:"column:n"`
	}
	err := r.db.WithContext(ctx).Model(&models.AgentTaskPO{}).
		Select("status, COUNT(*) AS n").
		Group("status").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	var histRows []struct {
		Status string `gorm:"column:status"`
		N      int64  `gorm:"column:n"`
	}
	err = r.db.WithContext(ctx).Model(&models.AgentTaskHistoryPO{}).
		Select("status, COUNT(*) AS n").
		Group("status").
		Scan(&histRows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows)+len(histRows))
	for _, row := range rows {
		out[row.Status] += row.N
	}
	for _, row := range histRows {
		out[row.Status] += row.N
	}
	return out, nil
}

// RequeueInterrupted 崩溃恢复：遗留 running 置回 pending；返回 pending 任务 ID（含未入队的）
func (r *AgentFactoryRepo) RequeueInterrupted(ctx context.Context) ([]uint64, error) {
	if err := r.db.WithContext(ctx).Model(&models.AgentTaskPO{}).
		Where("status = ?", agentfactory.TaskRunning).
		Updates(map[string]any{"status": agentfactory.TaskPending, "progress": 0}).Error; err != nil {
		return nil, err
	}
	var ids []uint64
	err := r.db.WithContext(ctx).Model(&models.AgentTaskPO{}).
		Where("status = ?", agentfactory.TaskPending).
		Order("id ASC").Limit(1000).
		Pluck("id", &ids).Error
	return ids, err
}

// ———— 事件（trace） ————

func (r *AgentFactoryRepo) AddEvent(ctx context.Context, e *agentfactory.TaskEvent) error {
	row := models.AgentTaskEventPO{
		TaskID:    e.TaskID,
		Turn:      e.Turn,
		Type:      e.Type,
		Payload:   e.Payload,
		CreatedAt: time.Now(),
	}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	e.ID = row.ID
	return nil
}

func (r *AgentFactoryRepo) ListEvents(ctx context.Context, taskID uint64, limit int) ([]agentfactory.TaskEvent, error) {
	var rows []models.AgentTaskEventPO
	if err := r.db.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("id ASC").Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]agentfactory.TaskEvent, 0, len(rows))
	for _, row := range rows {
		out = append(out, agentfactory.TaskEvent{
			ID:        row.ID,
			TaskID:    row.TaskID,
			Turn:      row.Turn,
			Type:      row.Type,
			Payload:   row.Payload,
			CreatedAt: row.CreatedAt,
		})
	}
	return out, nil
}

// ———— DO ↔ PO 转换 ————

// backfillHistory 启动回填：把任务池中遗留的终态任务迁入历史表。
// 历史版本（单表时代）的 succeeded/failed/canceled 任务在此归档，幂等。
func (r *AgentFactoryRepo) backfillHistory(ctx context.Context) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []models.AgentTaskPO
		if err := tx.Where("status IN ?", []string{
			agentfactory.TaskSucceeded, agentfactory.TaskFailed, agentfactory.TaskCanceled,
		}).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, src := range rows {
			hist := taskToHistoryPO(src)
			hist.ArchivedAt = time.Now()
			if err := tx.Create(&hist).Error; err != nil {
				return err
			}
			if err := tx.Unscoped().Delete(&models.AgentTaskPO{}, src.ID).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// taskToHistoryPO 任务池行 → 历史表行（字段同构拷贝）
func taskToHistoryPO(src models.AgentTaskPO) models.AgentTaskHistoryPO {
	return models.AgentTaskHistoryPO{
		ID:           src.ID,
		AgentDefID:   src.AgentDefID,
		AgentKey:     src.AgentKey,
		Title:        src.Title,
		Input:        src.Input,
		Status:       src.Status,
		Progress:     src.Progress,
		Result:       src.Result,
		ErrorMsg:     src.ErrorMsg,
		ParentTaskID: src.ParentTaskID,
		Depth:        src.Depth,
		ConvID:       src.ConvID,
		CreatedAt:    src.CreatedAt,
		StartedAt:    src.StartedAt,
		FinishedAt:   src.FinishedAt,
	}
}

// taskHistoryToBiz 历史表行 → DO（Archived 恒为 true）
func taskHistoryToBiz(row models.AgentTaskHistoryPO) agentfactory.Task {
	t := agentfactory.Task{
		ID:           row.ID,
		AgentDefID:   row.AgentDefID,
		AgentKey:     row.AgentKey,
		Title:        row.Title,
		Input:        row.Input,
		Status:       row.Status,
		Progress:     row.Progress,
		Result:       row.Result,
		ErrorMsg:     row.ErrorMsg,
		ParentTaskID: row.ParentTaskID,
		Depth:        row.Depth,
		ConvID:       row.ConvID,
		CreatedAt:    row.CreatedAt,
		Archived:     true,
	}
	if row.StartedAt != nil {
		t.StartedAt = *row.StartedAt
	}
	if row.FinishedAt != nil {
		t.FinishedAt = *row.FinishedAt
	}
	return t
}

func defToBiz(row models.AgentDefPO) agentfactory.AgentDef {
	return agentfactory.AgentDef{
		ID:               row.ID,
		AgentKey:         row.AgentKey,
		Name:             row.Name,
		Description:      row.Description,
		System:           row.System,
		LLMModel:         row.LLMModel,
		LLMSource:        row.LLMSource,
		GraphType:        row.GraphType,
		Capabilities:     decodeStrings(row.Capabilities),
		Middlewares:      decodeStrings(row.Middlewares),
		MaxTurns:         row.MaxTurns,
		Sandbox:          row.Sandbox,
		Source:           row.Source,
		Version:          row.Version,
		Enabled:          row.Enabled,
		Vectorized:       row.Vectorized,
		Role:             row.Role,
		Orchestration:    row.Orchestration,
		Members:          decodeStrings(row.Members),
		Flow:             row.Flow,
		SupervisorKey:    row.SupervisorKey,
		SupervisionScope: decodeStrings(row.SupervisionScope),
		RiskAgentKey:     row.RiskAgentKey,
		AutoRun:          row.AutoRun,
		CronExpr:         row.CronExpr,
		CronInput:        row.CronInput,
		NextRunAt:        timeOrZero(row.NextRunAt),
		LastRunAt:        timeOrZero(row.LastRunAt),
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

func defFromBiz(d agentfactory.AgentDef) models.AgentDefPO {
	return models.AgentDefPO{
		ID:               d.ID,
		AgentKey:         d.AgentKey,
		Name:             d.Name,
		Description:      d.Description,
		System:           d.System,
		LLMModel:         d.LLMModel,
		LLMSource:        d.LLMSource,
		GraphType:        d.GraphType,
		Capabilities:     encodeStrings(d.Capabilities),
		Middlewares:      encodeStrings(d.Middlewares),
		MaxTurns:         d.MaxTurns,
		Sandbox:          d.Sandbox,
		Source:           d.Source,
		Version:          d.Version,
		Enabled:          d.Enabled,
		Vectorized:       d.Vectorized,
		Role:             d.Role,
		Orchestration:    d.Orchestration,
		Members:          encodeStrings(d.Members),
		Flow:             d.Flow,
		SupervisorKey:    d.SupervisorKey,
		SupervisionScope: encodeStrings(d.SupervisionScope),
		RiskAgentKey:     d.RiskAgentKey,
		AutoRun:          d.AutoRun,
		CronExpr:         d.CronExpr,
		CronInput:        d.CronInput,
		NextRunAt:        nilIfZero(d.NextRunAt),
		LastRunAt:        nilIfZero(d.LastRunAt),
	}
}

func taskToBiz(row models.AgentTaskPO) agentfactory.Task {
	t := agentfactory.Task{
		ID:           row.ID,
		AgentDefID:   row.AgentDefID,
		AgentKey:     row.AgentKey,
		Title:        row.Title,
		Input:        row.Input,
		Status:       row.Status,
		Progress:     row.Progress,
		Result:       row.Result,
		ErrorMsg:     row.ErrorMsg,
		ParentTaskID: row.ParentTaskID,
		Depth:        row.Depth,
		ConvID:       row.ConvID,
		CreatedAt:    row.CreatedAt,
	}
	if row.StartedAt != nil {
		t.StartedAt = *row.StartedAt
	}
	if row.FinishedAt != nil {
		t.FinishedAt = *row.FinishedAt
	}
	return t
}

func taskFromBiz(t agentfactory.Task) models.AgentTaskPO {
	row := models.AgentTaskPO{
		ID:           t.ID,
		AgentDefID:   t.AgentDefID,
		AgentKey:     t.AgentKey,
		Title:        t.Title,
		Input:        t.Input,
		Status:       t.Status,
		Progress:     t.Progress,
		Result:       t.Result,
		ErrorMsg:     t.ErrorMsg,
		ParentTaskID: t.ParentTaskID,
		Depth:        t.Depth,
		ConvID:       t.ConvID,
		CreatedAt:    t.CreatedAt,
	}
	if !t.StartedAt.IsZero() {
		ts := t.StartedAt
		row.StartedAt = &ts
	}
	if !t.FinishedAt.IsZero() {
		tf := t.FinishedAt
		row.FinishedAt = &tf
	}
	return row
}

// encodeStrings JSON 字符串数组列编码：空集必须输出 "[]"（MySQL JSON 列拒收空串）
func encodeStrings(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// decodeStrings JSON 字符串数组列解码：空串/非法 JSON 一律按空数组降级
func decodeStrings(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// UpdateDefSchedule 只更新调度字段（下次/上次运行时间），不推进定义版本
func (r *AgentFactoryRepo) UpdateDefSchedule(ctx context.Context, id uint64, next, last time.Time) error {
	updates := map[string]any{"next_run_at": nil, "last_run_at": nil}
	if !next.IsZero() {
		updates["next_run_at"] = next
	}
	if !last.IsZero() {
		updates["last_run_at"] = last
	}
	return r.db.WithContext(ctx).Model(&models.AgentDefPO{}).
		Where("id = ?", id).
		Updates(updates).Error
}

// timeOrZero 数据库 NULL → 零值时间
func timeOrZero(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

// nilIfZero 零值时间 → 数据库 NULL（MySQL 拒收零值 datetime）
func nilIfZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
