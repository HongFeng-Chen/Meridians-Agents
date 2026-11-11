// resource.go 统一资源 MySQL 仓储：resource_metadata / resource_permissions / resource_versions。
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go_agent/internal/biz/resource"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ResourceRepo 资源元数据仓库
type ResourceRepo struct {
	db *gorm.DB
}

var _ resource.Repo = (*ResourceRepo)(nil)

// NewResourceRepo 创建仓库；auto_migrate 打开时建表。
func NewResourceRepo(c *conf.Data, d *db.Data) (resource.Repo, error) {
	r := &ResourceRepo{db: d.DB()}
	if !c.GetDatabase().GetAutoMigrate() {
		return r, nil
	}
	if err := r.db.AutoMigrate(
		&models.ResourceMetadataPO{},
		&models.ResourcePermissionPO{},
		&models.ResourceVersionPO{},
	); err != nil {
		return nil, fmt.Errorf("迁移 resource_* 表: %w", err)
	}
	return r, nil
}

// Create 创建资源元数据。
func (r *ResourceRepo) Create(ctx context.Context, res *resource.Resource) error {
	po := resourceToPO(res)
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		if isDuplicate(err) {
			return fmt.Errorf("%w: %s", resource.ErrConflict, res.ResourceID)
		}
		return err
	}
	return nil
}

// Update 更新资源元数据（resource_id / resource_type / tenant_id / owner_id 不变）。
func (r *ResourceRepo) Update(ctx context.Context, res *resource.Resource) error {
	res2 := r.db.WithContext(ctx).Model(&models.ResourceMetadataPO{}).
		Where("resource_id = ? AND resource_type = ?", res.ResourceID, res.ResourceType).
		Updates(map[string]any{
			"space_id":    res.SpaceID,
			"name":        res.Name,
			"description": res.Description,
			"visibility":  res.Visibility,
			"version":     res.Version,
			"status":      res.Status,
			"updated_at":  res.UpdatedAt,
		})
	if res2.Error != nil {
		return res2.Error
	}
	if res2.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", resource.ErrNotFound, res.ResourceID)
	}
	return nil
}

// Get 按 resource_id 查询（跨类型取第一条：调用方应保证 ID 全局唯一）。
func (r *ResourceRepo) Get(ctx context.Context, resourceID string) (*resource.Resource, error) {
	var po models.ResourceMetadataPO
	err := r.db.WithContext(ctx).Where("resource_id = ?", strings.TrimSpace(resourceID)).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %s", resource.ErrNotFound, resourceID)
	}
	if err != nil {
		return nil, err
	}
	return resourceToBiz(&po), nil
}

// List 按条件列出资源。
func (r *ResourceRepo) List(ctx context.Context, f resource.Filter) ([]resource.Resource, error) {
	q := r.db.WithContext(ctx).Model(&models.ResourceMetadataPO{})
	if f.TenantID != "" {
		q = q.Where("tenant_id = ?", f.TenantID)
	}
	if f.SpaceID != "" {
		q = q.Where("space_id = ?", f.SpaceID)
	}
	if f.ResourceType != "" {
		q = q.Where("resource_type = ?", f.ResourceType)
	}
	if f.OwnerID != "" {
		q = q.Where("owner_id = ?", f.OwnerID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var rows []models.ResourceMetadataPO
	if err := q.Order("updated_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]resource.Resource, 0, len(rows))
	for i := range rows {
		out = append(out, *resourceToBiz(&rows[i]))
	}
	return out, nil
}

// Delete 删除资源元数据与授权（版本历史保留，审计可追溯）。
func (r *ResourceRepo) Delete(ctx context.Context, resourceID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Where("resource_id = ?", resourceID).Delete(&models.ResourceMetadataPO{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: %s", resource.ErrNotFound, resourceID)
		}
		return tx.Where("resource_id = ?", resourceID).Delete(&models.ResourcePermissionPO{}).Error
	})
}

// AddVersion 追加不可变版本。
func (r *ResourceRepo) AddVersion(ctx context.Context, v *resource.Version) error {
	po := &models.ResourceVersionPO{
		ResourceID: v.ResourceID,
		Version:    v.Version,
		Snapshot:   v.Snapshot,
		Checksum:   v.Checksum,
		CreatedBy:  v.CreatedBy,
		CreatedAt:  v.CreatedAt,
	}
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		if isDuplicate(err) {
			return fmt.Errorf("%w: 版本 %d 已存在（历史快照不可覆盖）", resource.ErrConflict, v.Version)
		}
		return err
	}
	return nil
}

// ListVersions 版本历史（倒序）。
func (r *ResourceRepo) ListVersions(ctx context.Context, resourceID string) ([]resource.Version, error) {
	var rows []models.ResourceVersionPO
	if err := r.db.WithContext(ctx).
		Where("resource_id = ?", resourceID).
		Order("version DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]resource.Version, 0, len(rows))
	for _, po := range rows {
		out = append(out, resource.Version{
			ResourceID: po.ResourceID,
			Version:    po.Version,
			Snapshot:   po.Snapshot,
			Checksum:   po.Checksum,
			CreatedBy:  po.CreatedBy,
			CreatedAt:  po.CreatedAt,
		})
	}
	return out, nil
}

// Grants 资源上的全部显式授权。
func (r *ResourceRepo) Grants(ctx context.Context, resourceID string) ([]resource.Grant, error) {
	var rows []models.ResourcePermissionPO
	if err := r.db.WithContext(ctx).
		Where("resource_id = ?", resourceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]resource.Grant, 0, len(rows))
	for _, po := range rows {
		out = append(out, resource.Grant{
			ID:            po.ID,
			ResourceID:    po.ResourceID,
			PrincipalType: po.PrincipalType,
			PrincipalID:   po.PrincipalID,
			Action:        po.Action,
			Effect:        po.Effect,
			CreatedAt:     po.CreatedAt,
		})
	}
	return out, nil
}

// AddGrant 幂等写入：同主体同动作再次授权时更新 effect（allow ↔ deny）。
func (r *ResourceRepo) AddGrant(ctx context.Context, g *resource.Grant) error {
	po := &models.ResourcePermissionPO{
		ResourceID:    g.ResourceID,
		PrincipalType: g.PrincipalType,
		PrincipalID:   g.PrincipalID,
		Action:        g.Action,
		Effect:        g.Effect,
		CreatedAt:     g.CreatedAt,
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "resource_id"}, {Name: "principal_type"}, {Name: "principal_id"}, {Name: "action"}},
		DoUpdates: clause.AssignmentColumns([]string{"effect"}),
	}).Create(po).Error
	if err != nil {
		return err
	}
	g.ID = po.ID
	return nil
}

// DeleteGrant 撤销单个动作的授权。
func (r *ResourceRepo) DeleteGrant(ctx context.Context, resourceID, principalType, principalID, action string) error {
	return r.db.WithContext(ctx).
		Where("resource_id = ? AND principal_type = ? AND principal_id = ? AND action = ?",
			resourceID, principalType, principalID, action).
		Delete(&models.ResourcePermissionPO{}).Error
}

func resourceToPO(res *resource.Resource) *models.ResourceMetadataPO {
	return &models.ResourceMetadataPO{
		ResourceID:   res.ResourceID,
		ResourceType: res.ResourceType,
		TenantID:     res.TenantID,
		SpaceID:      res.SpaceID,
		OwnerID:      res.OwnerID,
		Name:         res.Name,
		Description:  res.Description,
		Visibility:   res.Visibility,
		Version:      res.Version,
		Status:       res.Status,
		CreatedAt:    res.CreatedAt,
		UpdatedAt:    res.UpdatedAt,
	}
}

func resourceToBiz(po *models.ResourceMetadataPO) *resource.Resource {
	return &resource.Resource{
		ResourceID:   po.ResourceID,
		ResourceType: po.ResourceType,
		TenantID:     po.TenantID,
		SpaceID:      po.SpaceID,
		OwnerID:      po.OwnerID,
		Name:         po.Name,
		Description:  po.Description,
		Visibility:   po.Visibility,
		Version:      po.Version,
		Status:       po.Status,
		CreatedAt:    po.CreatedAt,
		UpdatedAt:    po.UpdatedAt,
	}
}
