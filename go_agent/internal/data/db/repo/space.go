// space.go 知识空间 MySQL 仓储：knowledge_spaces / knowledge_space_shares。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go_agent/internal/biz/space"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// SpaceRepo 知识空间仓库
type SpaceRepo struct {
	db *gorm.DB
}

var _ space.Repo = (*SpaceRepo)(nil)

// NewSpaceRepo 创建仓库；auto_migrate 打开时建表。
func NewSpaceRepo(c *conf.Data, d *db.Data) (space.Repo, error) {
	r := &SpaceRepo{db: d.DB()}
	if !c.GetDatabase().GetAutoMigrate() {
		return r, nil
	}
	if err := r.db.AutoMigrate(&models.KnowledgeSpacePO{}, &models.KnowledgeSpaceSharePO{}); err != nil {
		return nil, fmt.Errorf("迁移 knowledge_spaces 表: %w", err)
	}
	return r, nil
}

// Create 创建空间；space_id 冲突转成领域错误，便于上层返回 409。
func (r *SpaceRepo) Create(ctx context.Context, s *space.Space) error {
	po := &models.KnowledgeSpacePO{
		SpaceID:         s.SpaceID,
		ParentSpaceID:   s.ParentSpaceID,
		TenantID:        s.TenantID,
		OwnerID:         s.OwnerID,
		Name:            s.Name,
		Description:     s.Description,
		SpaceType:       s.SpaceType,
		InheritanceMode: s.InheritanceMode,
		Path:            s.Path,
		Depth:           s.Depth,
		Status:          s.Status,
		CreatedAt:       s.CreatedAt,
		UpdatedAt:       s.UpdatedAt,
	}
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		if isDuplicate(err) {
			return fmt.Errorf("%w: %s", space.ErrConflict, s.SpaceID)
		}
		return err
	}
	return nil
}

// Get 按 space_id 查询。
func (r *SpaceRepo) Get(ctx context.Context, spaceID string) (*space.Space, error) {
	var po models.KnowledgeSpacePO
	err := r.db.WithContext(ctx).Where("space_id = ?", strings.TrimSpace(spaceID)).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %s", space.ErrNotFound, spaceID)
	}
	if err != nil {
		return nil, err
	}
	return spaceToBiz(&po), nil
}

// Update 更新元数据（不改 space_id / tenant_id）。
func (r *SpaceRepo) Update(ctx context.Context, s *space.Space) error {
	res := r.db.WithContext(ctx).Model(&models.KnowledgeSpacePO{}).
		Where("space_id = ?", s.SpaceID).
		Updates(map[string]any{
			"parent_space_id":  s.ParentSpaceID,
			"name":             s.Name,
			"description":      s.Description,
			"inheritance_mode": s.InheritanceMode,
			"path":             s.Path,
			"depth":            s.Depth,
			"status":           s.Status,
			"updated_at":       s.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", space.ErrNotFound, s.SpaceID)
	}
	return nil
}

// Delete 物理删除（调用方已保证没有子空间）。
func (r *SpaceRepo) Delete(ctx context.Context, spaceID string) error {
	res := r.db.WithContext(ctx).Where("space_id = ?", spaceID).Delete(&models.KnowledgeSpacePO{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", space.ErrNotFound, spaceID)
	}
	return nil
}

// ListByTenant 列出某租户的空间；tenantID 为空表示平台空间。
func (r *SpaceRepo) ListByTenant(ctx context.Context, tenantID string) ([]space.Space, error) {
	var rows []models.KnowledgeSpacePO
	q := r.db.WithContext(ctx).Model(&models.KnowledgeSpacePO{})
	if strings.TrimSpace(tenantID) == "" {
		q = q.Where("tenant_id = ''")
	} else {
		q = q.Where("tenant_id = ?", tenantID)
	}
	if err := q.Order("depth ASC, space_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]space.Space, 0, len(rows))
	for i := range rows {
		out = append(out, *spaceToBiz(&rows[i]))
	}
	return out, nil
}

// ListChildren 直接子节点。
func (r *SpaceRepo) ListChildren(ctx context.Context, parentSpaceID string) ([]space.Space, error) {
	var rows []models.KnowledgeSpacePO
	if err := r.db.WithContext(ctx).
		Where("parent_space_id = ?", parentSpaceID).
		Order("space_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]space.Space, 0, len(rows))
	for i := range rows {
		out = append(out, *spaceToBiz(&rows[i]))
	}
	return out, nil
}

// ListDescendants 按物化路径取整棵子树（不含自身）。
func (r *SpaceRepo) ListDescendants(ctx context.Context, spaceID string) ([]space.Space, error) {
	self, err := r.Get(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	prefix := self.SpaceID
	if self.Path != "" {
		prefix = self.Path + "/" + self.SpaceID
	}
	var rows []models.KnowledgeSpacePO
	if err := r.db.WithContext(ctx).
		Where("path = ? OR path LIKE ?", prefix, prefix+"/%").
		Order("depth ASC, space_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]space.Space, 0, len(rows))
	for i := range rows {
		out = append(out, *spaceToBiz(&rows[i]))
	}
	return out, nil
}

// UpdatePathPrefix 移动子树：把 oldPrefix 替换成 newPrefix，depth 同步加 delta。
//
// 用 SQL 的 SUBSTRING 做前缀替换，避免把整棵子树读进内存再逐行更新
// （深层空间可能有上百个节点，逐行写会放大锁竞争）。
func (r *SpaceRepo) UpdatePathPrefix(ctx context.Context, oldPrefix, newPrefix string, delta int) error {
	if oldPrefix == "" {
		return nil
	}
	return r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_spaces SET path = CONCAT(?, SUBSTRING(path, ?)), depth = depth + ? "+
			"WHERE path = ? OR path LIKE ?",
		newPrefix, len(oldPrefix)+1, delta, oldPrefix, oldPrefix+"/%",
	).Error
}

// AddShare 新增共享。
func (r *SpaceRepo) AddShare(ctx context.Context, sh *space.Share) error {
	raw, err := json.Marshal(sh.Actions)
	if err != nil {
		return err
	}
	po := &models.KnowledgeSpaceSharePO{
		SpaceID:       sh.SpaceID,
		PrincipalType: sh.PrincipalType,
		PrincipalID:   sh.PrincipalID,
		Actions:       string(raw),
		CreatedAt:     sh.CreatedAt,
	}
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		return err
	}
	sh.ID = po.ID
	return nil
}

// DeleteShare 按 ID 删除共享。
func (r *SpaceRepo) DeleteShare(ctx context.Context, id uint64) error {
	res := r.db.WithContext(ctx).Where("id = ?", id).Delete(&models.KnowledgeSpaceSharePO{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: share %d", space.ErrNotFound, id)
	}
	return nil
}

// GetShare 按 ID 查询共享。
func (r *SpaceRepo) GetShare(ctx context.Context, id uint64) (*space.Share, error) {
	var po models.KnowledgeSpaceSharePO
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: share %d", space.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return shareToBiz(&po), nil
}

// ListShares 某个空间的全部共享。
func (r *SpaceRepo) ListShares(ctx context.Context, spaceID string) ([]space.Share, error) {
	var rows []models.KnowledgeSpaceSharePO
	if err := r.db.WithContext(ctx).
		Where("space_id = ?", spaceID).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]space.Share, 0, len(rows))
	for i := range rows {
		out = append(out, *shareToBiz(&rows[i]))
	}
	return out, nil
}

// ListSharesForTenant 查询与本租户（含平台空间）主体相关的共享。
//
// 只按「主体匹配」捞候选，继承展开交给 biz 层：SQL 里做继承会把
// path 前缀、isolated 排除、restricted 截断三件事糅在一起，很难验证。
func (r *SpaceRepo) ListSharesForTenant(ctx context.Context, tenantID string, refs []space.PrincipalRef) ([]space.Share, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	clauses := make([]string, 0, len(refs))
	args := make([]any, 0, len(refs)*2)
	for _, ref := range refs {
		if strings.TrimSpace(ref.ID) == "" {
			continue
		}
		clauses = append(clauses, "(knowledge_space_shares.principal_type = ? AND knowledge_space_shares.principal_id = ?)")
		args = append(args, ref.Type, ref.ID)
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	var rows []models.KnowledgeSpaceSharePO
	err := r.db.WithContext(ctx).Model(&models.KnowledgeSpaceSharePO{}).
		Joins("JOIN knowledge_spaces s ON s.space_id = knowledge_space_shares.space_id").
		Where("(s.tenant_id = ? OR s.tenant_id = '')", tenantID).
		Where(strings.Join(clauses, " OR "), args...).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]space.Share, 0, len(rows))
	for i := range rows {
		out = append(out, *shareToBiz(&rows[i]))
	}
	return out, nil
}

func spaceToBiz(po *models.KnowledgeSpacePO) *space.Space {
	return &space.Space{
		SpaceID:         po.SpaceID,
		ParentSpaceID:   po.ParentSpaceID,
		TenantID:        po.TenantID,
		OwnerID:         po.OwnerID,
		Name:            po.Name,
		Description:     po.Description,
		SpaceType:       po.SpaceType,
		InheritanceMode: po.InheritanceMode,
		Path:            po.Path,
		Depth:           po.Depth,
		Status:          po.Status,
		CreatedAt:       po.CreatedAt,
		UpdatedAt:       po.UpdatedAt,
	}
}

func shareToBiz(po *models.KnowledgeSpaceSharePO) *space.Share {
	var actions []string
	if po.Actions != "" {
		_ = json.Unmarshal([]byte(po.Actions), &actions)
	}
	return &space.Share{
		ID:            po.ID,
		SpaceID:       po.SpaceID,
		PrincipalType: po.PrincipalType,
		PrincipalID:   po.PrincipalID,
		Actions:       actions,
		CreatedAt:     po.CreatedAt,
	}
}
