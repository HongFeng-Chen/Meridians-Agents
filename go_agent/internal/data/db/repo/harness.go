// harness.go 评测 Harness MySQL 仓储：用例集、用例、运行与逐例结果。
//
// 运行行是「进度行」：worker 每完成一个用例就全量 UPDATE 一次（列数少，不做局部更新）；
// 逐例结果只增不改，重跑前由 biz 调 DeleteItemsOfRun 清空。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/harness"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// HarnessRepo 评测仓库
type HarnessRepo struct {
	db *gorm.DB
}

var _ harness.Repo = (*HarnessRepo)(nil)

// NewHarnessRepo 创建仓库；auto_migrate 打开时自动建表
func NewHarnessRepo(c *conf.Data, d *db.Data) (harness.Repo, error) {
	r := &HarnessRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(
			&models.HarnessSetPO{},
			&models.HarnessCasePO{},
			&models.HarnessRunPO{},
			&models.HarnessRunItemPO{},
		); err != nil {
			return nil, fmt.Errorf("迁移 harness 表: %w", err)
		}
	}
	return r, nil
}

// —— 用例集 ——

// ListSets 列出用例集
func (r *HarnessRepo) ListSets(ctx context.Context, f harness.SetFilter) ([]harness.Set, error) {
	q := r.db.WithContext(ctx).Model(&models.HarnessSetPO{})
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("name LIKE ? OR description LIKE ? OR target LIKE ?", like, like, like)
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var pos []models.HarnessSetPO
	if err := q.Order("id DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]harness.Set, 0, len(pos))
	for i := range pos {
		out = append(out, *setToBiz(&pos[i]))
	}
	return out, nil
}

// GetSet 按 ID 查询
func (r *HarnessRepo) GetSet(ctx context.Context, id uint64) (*harness.Set, error) {
	var po models.HarnessSetPO
	if err := r.firstSet(ctx, id, &po); err != nil {
		return nil, err
	}
	return setToBiz(&po), nil
}

func (r *HarnessRepo) firstSet(ctx context.Context, id uint64, po *models.HarnessSetPO) error {
	err := r.db.WithContext(ctx).Where("id = ?", id).First(po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return harness.ErrSetNotFound
	}
	return err
}

// CreateSet 新建用例集
func (r *HarnessRepo) CreateSet(ctx context.Context, s *harness.Set) error {
	po := setToPO(s)
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		return err
	}
	s.ID, s.CreatedAt, s.UpdatedAt = po.ID, po.CreatedAt, po.UpdatedAt
	return nil
}

// SaveSet 全量更新（created_at 不动）
func (r *HarnessRepo) SaveSet(ctx context.Context, s *harness.Set) error {
	res := r.db.WithContext(ctx).Model(&models.HarnessSetPO{}).
		Where("id = ?", s.ID).
		Updates(map[string]any{
			"name":        s.Name,
			"description": s.Description,
			"target":      s.Target,
			"tags":        marshalStrings(s.Tags),
			"is_enabled":  s.Enabled,
			"version":     s.Version,
			"checksum":    s.Checksum,
			"updated_at":  s.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return harness.ErrSetNotFound
	}
	return nil
}

// DeleteSet 删除用例集（级联删用例）
func (r *HarnessRepo) DeleteSet(ctx context.Context, id uint64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("set_id = ?", id).Delete(&models.HarnessCasePO{}).Error; err != nil {
			return err
		}
		res := tx.Where("id = ?", id).Delete(&models.HarnessSetPO{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return harness.ErrSetNotFound
		}
		return nil
	})
}

// CountCases 统计用例数（enabledOnly = 只数启用用例）
func (r *HarnessRepo) CountCases(ctx context.Context, setID uint64, enabledOnly bool) (int, error) {
	q := r.db.WithContext(ctx).Model(&models.HarnessCasePO{}).Where("set_id = ?", setID)
	if enabledOnly {
		q = q.Where("is_enabled = ?", true)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}

// —— 用例 ——

// ListCases 列出某用例集的用例
func (r *HarnessRepo) ListCases(ctx context.Context, setID uint64, enabledOnly bool) ([]harness.Case, error) {
	q := r.db.WithContext(ctx).Model(&models.HarnessCasePO{}).Where("set_id = ?", setID)
	if enabledOnly {
		q = q.Where("is_enabled = ?", true)
	}
	var pos []models.HarnessCasePO
	if err := q.Order("id ASC").Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]harness.Case, 0, len(pos))
	for i := range pos {
		out = append(out, *caseToBiz(&pos[i]))
	}
	return out, nil
}

// GetCase 按 ID 查询
func (r *HarnessRepo) GetCase(ctx context.Context, id uint64) (*harness.Case, error) {
	var po models.HarnessCasePO
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, harness.ErrCaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return caseToBiz(&po), nil
}

// CreateCase 新建用例
func (r *HarnessRepo) CreateCase(ctx context.Context, c *harness.Case) error {
	po := caseToPO(c)
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		return err
	}
	c.ID, c.CreatedAt, c.UpdatedAt = po.ID, po.CreatedAt, po.UpdatedAt
	return nil
}

// SaveCase 全量更新（created_at 不动）
func (r *HarnessRepo) SaveCase(ctx context.Context, c *harness.Case) error {
	res := r.db.WithContext(ctx).Model(&models.HarnessCasePO{}).
		Where("id = ?", c.ID).
		Updates(map[string]any{
			"name":       c.Name,
			"input":      c.Input,
			"context":    c.Context,
			"expect":     c.Expect,
			"checks":     marshalJSON(c.Checks),
			"tags":       marshalStrings(c.Tags),
			"is_enabled": c.Enabled,
			"updated_at": c.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return harness.ErrCaseNotFound
	}
	return nil
}

// DeleteCase 删除用例
func (r *HarnessRepo) DeleteCase(ctx context.Context, id uint64) error {
	res := r.db.WithContext(ctx).Where("id = ?", id).Delete(&models.HarnessCasePO{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return harness.ErrCaseNotFound
	}
	return nil
}

// —— 运行 ——

// CreateRun 新建运行行
func (r *HarnessRepo) CreateRun(ctx context.Context, run *harness.Run) error {
	po := runToPO(run)
	po.CreatedAt = time.Now()
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		return err
	}
	run.ID, run.CreatedAt = po.ID, po.CreatedAt
	return nil
}

// SaveRun 全量保存（进度推进即调用）
func (r *HarnessRepo) SaveRun(ctx context.Context, run *harness.Run) error {
	res := r.db.WithContext(ctx).Model(&models.HarnessRunPO{}).
		Where("id = ?", run.ID).
		Updates(map[string]any{
			"status":  run.Status,
			"total":   run.Total,
			"done":    run.Done,
			"passed":  run.Passed,
			"failed":  run.Failed,
			"errored": run.Errored,
			"score":   run.Score,
			"cost_ms": run.CostMS,
			"error":   run.Error,
			// finished_at 必须走 nilIfZero：biz 层是值类型 time.Time，
			// 未结束时是零值，直接写会变成 '0000-00-00' 被 MySQL 的
			// NO_ZERO_DATE 整条拒掉（进度/状态都不会落库，前端一直显示 pending）
			"finished_at": nilIfZero(run.FinishedAt),
			// 版本绑定列：创建后不再变化，重复写入是为了让「先跑后补」的路径也能落库
			"snapshot_id":      run.SnapshotID,
			"snapshot_version": run.SnapshotVersion,
			"dataset_version":  run.DatasetVersion,
			"dataset_checksum": run.DatasetChecksum,
			"model":            run.Model,
			"trace_id":         run.TraceID,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return harness.ErrRunNotFound
	}
	return nil
}

// GetRun 按 ID 查询
func (r *HarnessRepo) GetRun(ctx context.Context, id uint64) (*harness.Run, error) {
	var po models.HarnessRunPO
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, harness.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return runToBiz(&po), nil
}

// ListRuns 运行列表
func (r *HarnessRepo) ListRuns(ctx context.Context, f harness.RunFilter) ([]harness.Run, error) {
	q := r.db.WithContext(ctx).Model(&models.HarnessRunPO{})
	if f.SetID > 0 {
		q = q.Where("set_id = ?", f.SetID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var pos []models.HarnessRunPO
	if err := q.Order("id DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]harness.Run, 0, len(pos))
	for i := range pos {
		out = append(out, *runToBiz(&pos[i]))
	}
	return out, nil
}

// DeleteRun 删除运行与逐例结果
func (r *HarnessRepo) DeleteRun(ctx context.Context, id uint64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("run_id = ?", id).Delete(&models.HarnessRunItemPO{}).Error; err != nil {
			return err
		}
		res := tx.Where("id = ?", id).Delete(&models.HarnessRunPO{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return harness.ErrRunNotFound
		}
		return nil
	})
}

// TouchRun 续心跳。
//
// 条件里带 status='running'：终态行不再续跳，避免「运行已结束但心跳还在动」
// 让它看起来像活着的。终态与心跳的并发竞争最多让这次续跳影响 0 行，无害。
func (r *HarnessRepo) TouchRun(ctx context.Context, runID uint64, at time.Time) error {
	return r.db.WithContext(ctx).Model(&models.HarnessRunPO{}).
		Where("id = ? AND status = ?", runID, harness.RunRunning).
		Update("heartbeat_at", at).Error
}

// ReapRuns 回收僵尸运行：心跳（未开始时退化为创建时间）早于 before 的
// pending/running 行收为 failed。
//
// 不标记为 canceled：canceled 表示「人主动停的」，这里是执行体失联，
// error 里写清原因，报告页能直接看出是基础设施问题而不是用例失败。
func (r *HarnessRepo) ReapRuns(ctx context.Context, before time.Time, reason string) (int64, error) {
	res := r.db.WithContext(ctx).Model(&models.HarnessRunPO{}).
		Where("status IN ?", []string{harness.RunPending, harness.RunRunning}).
		Where("COALESCE(heartbeat_at, created_at) < ?", before).
		Updates(map[string]any{
			"status":      harness.RunFailed,
			"error":       reason,
			"finished_at": time.Now(),
		})
	return res.RowsAffected, res.Error
}

// —— 逐例结果 ——

// CreateItem 写入单例结果
func (r *HarnessRepo) CreateItem(ctx context.Context, it *harness.RunItem) error {
	po := &models.HarnessRunItemPO{
		RunID:            it.RunID,
		CaseID:           it.CaseID,
		CaseName:         it.CaseName,
		Output:           it.Output,
		Checks:           marshalJSON(it.Checks),
		Passed:           it.Passed,
		Score:            it.Score,
		LatencyMS:        it.LatencyMS,
		PromptTokens:     it.PromptTokens,
		CompletionTokens: it.CompletionTokens,
		Error:            it.Error,
		CreatedAt:        it.CreatedAt,
	}
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		return err
	}
	it.ID = po.ID
	return nil
}

// ListItems 某次运行的全部逐例结果（按用例顺序）
func (r *HarnessRepo) ListItems(ctx context.Context, runID uint64) ([]harness.RunItem, error) {
	var pos []models.HarnessRunItemPO
	if err := r.db.WithContext(ctx).Where("run_id = ?", runID).Order("id ASC").Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]harness.RunItem, 0, len(pos))
	for i := range pos {
		po := &pos[i]
		it := &harness.RunItem{
			ID:               po.ID,
			RunID:            po.RunID,
			CaseID:           po.CaseID,
			CaseName:         po.CaseName,
			Output:           po.Output,
			Passed:           po.Passed,
			Score:            po.Score,
			LatencyMS:        po.LatencyMS,
			PromptTokens:     po.PromptTokens,
			CompletionTokens: po.CompletionTokens,
			Error:            po.Error,
			CreatedAt:        po.CreatedAt,
		}
		_ = json.Unmarshal([]byte(po.Checks), &it.Checks)
		out = append(out, *it)
	}
	return out, nil
}

// DeleteItemsOfRun 清空某次运行的逐例结果（重跑前调用）
func (r *HarnessRepo) DeleteItemsOfRun(ctx context.Context, runID uint64) error {
	return r.db.WithContext(ctx).Where("run_id = ?", runID).Delete(&models.HarnessRunItemPO{}).Error
}

// —— 转换 ——

func setToPO(s *harness.Set) *models.HarnessSetPO {
	return &models.HarnessSetPO{
		ID:          s.ID,
		Name:        s.Name,
		Description: s.Description,
		Target:      s.Target,
		Tags:        marshalStrings(s.Tags),
		Enabled:     s.Enabled,
		Version:     s.Version,
		Checksum:    s.Checksum,
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
	}
}

func setToBiz(po *models.HarnessSetPO) *harness.Set {
	s := &harness.Set{
		ID:          po.ID,
		Name:        po.Name,
		Description: po.Description,
		Target:      po.Target,
		Enabled:     po.Enabled,
		Version:     po.Version,
		Checksum:    po.Checksum,
		CreatedAt:   po.CreatedAt,
		UpdatedAt:   po.UpdatedAt,
	}
	_ = json.Unmarshal([]byte(po.Tags), &s.Tags)
	return s
}

func caseToPO(c *harness.Case) *models.HarnessCasePO {
	return &models.HarnessCasePO{
		ID:        c.ID,
		SetID:     c.SetID,
		Name:      c.Name,
		Input:     c.Input,
		Context:   c.Context,
		Expect:    c.Expect,
		Checks:    marshalJSON(c.Checks),
		Tags:      marshalStrings(c.Tags),
		Enabled:   c.Enabled,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
}

func caseToBiz(po *models.HarnessCasePO) *harness.Case {
	c := &harness.Case{
		ID:        po.ID,
		SetID:     po.SetID,
		Name:      po.Name,
		Input:     po.Input,
		Context:   po.Context,
		Expect:    po.Expect,
		Tags:      nil,
		Enabled:   po.Enabled,
		CreatedAt: po.CreatedAt,
		UpdatedAt: po.UpdatedAt,
	}
	_ = json.Unmarshal([]byte(po.Checks), &c.Checks)
	_ = json.Unmarshal([]byte(po.Tags), &c.Tags)
	return c
}

func runToPO(run *harness.Run) *models.HarnessRunPO {
	return &models.HarnessRunPO{
		ID:         run.ID,
		SetID:      run.SetID,
		SetName:    run.SetName,
		Target:     run.Target,
		Status:     run.Status,
		Total:      run.Total,
		Done:       run.Done,
		Passed:     run.Passed,
		Failed:     run.Failed,
		Errored:    run.Errored,
		Score:      run.Score,
		CostMS:     run.CostMS,
		Config:     marshalJSON(run.Config),
		Error:      run.Error,
		CreatedAt:  run.CreatedAt,
		FinishedAt: nilIfZero(run.FinishedAt),

		SnapshotID:      run.SnapshotID,
		SnapshotVersion: run.SnapshotVersion,
		DatasetVersion:  run.DatasetVersion,
		DatasetChecksum: run.DatasetChecksum,
		Model:           run.Model,
		TraceID:         run.TraceID,
		// 存活字段只在创建时写；之后由 TouchRun / ReapRuns 独立维护，
		// SaveRun 不碰它们（否则进度推进会把心跳写回旧时间）
		RunnerID:    run.RunnerID,
		HeartbeatAt: nilIfZero(run.HeartbeatAt),
	}
}

func runToBiz(po *models.HarnessRunPO) *harness.Run {
	run := &harness.Run{
		ID:        po.ID,
		SetID:     po.SetID,
		SetName:   po.SetName,
		Target:    po.Target,
		Status:    po.Status,
		Total:     po.Total,
		Done:      po.Done,
		Passed:    po.Passed,
		Failed:    po.Failed,
		Errored:   po.Errored,
		Score:     po.Score,
		CostMS:    po.CostMS,
		Error:     po.Error,
		CreatedAt: po.CreatedAt,

		SnapshotID:      po.SnapshotID,
		SnapshotVersion: po.SnapshotVersion,
		DatasetVersion:  po.DatasetVersion,
		DatasetChecksum: po.DatasetChecksum,
		Model:           po.Model,
		TraceID:         po.TraceID,
		RunnerID:        po.RunnerID,

		HeartbeatAt: func() time.Time {
			if po.HeartbeatAt != nil {
				return *po.HeartbeatAt
			}
			return time.Time{}
		}(),

		FinishedAt: func() time.Time {
			if po.FinishedAt != nil {
				return *po.FinishedAt
			}
			return time.Time{}
		}(),
	}
	_ = json.Unmarshal([]byte(po.Config), &run.Config)
	return run
}

// marshalJSON 序列化辅助：nil 切片落 "[]"，避免 MySQL JSON 列歧义
func marshalJSON(v any) string {
	if v == nil {
		return "[]"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func marshalStrings(ts []string) string {
	return marshalJSON(ts)
}
