// document.go RAG 文档元数据仓储：
// 文档列表、内容校验和（去重）、入库状态，是文档管理接口的权威数据源。
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go_agent/internal/biz/rag"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DocumentRepo 文档元数据仓库
type DocumentRepo struct {
	db *gorm.DB
}

var _ rag.DocumentRepo = (*DocumentRepo)(nil)

// NewDocumentRepo 创建仓库；auto_migrate 打开时自动建表（开发环境便利项）
func NewDocumentRepo(c *conf.Data, d *db.Data) (rag.DocumentRepo, error) {
	r := &DocumentRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.DocumentPO{}); err != nil {
			return nil, fmt.Errorf("迁移 rag_documents 表: %w", err)
		}
	}
	return r, nil
}

// Upsert 按 doc_id 写入或更新（内容与状态变化都覆盖）
func (r *DocumentRepo) Upsert(ctx context.Context, doc *rag.Document) error {
	po := &models.DocumentPO{
		DocID:     doc.DocID,
		UserID:    ownerOr(doc.UserID),
		TenantID:  ownerOr(doc.TenantID),
		Scope:     scopeOr(doc.Scope),
		Source:    clip(doc.Source, 512),
		Mode:      doc.Mode,
		Status:    doc.Status,
		Chars:     doc.Chars,
		Chunks:    doc.Chunks,
		Entities:  doc.Entities,
		Relations: doc.Relations,
		Checksum:  doc.Checksum,
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "doc_id"}},
		// user_id / tenant_id 不参与更新：文档归属在首次入库时确定，重复入库不改归属。
		// scope 参与更新：作者可以把个人知识主动升为租户共享（或收回），
		// 权限在 biz 层按「文档既有作用域」校验，能走到这里的都已经过审。
		DoUpdates: clause.AssignmentColumns([]string{
			"source", "mode", "scope", "status", "chars", "chunks", "entities", "relations", "checksum", "updated_at",
		}),
	}).Create(po).Error
	if err != nil {
		return fmt.Errorf("写入文档元数据: %w", err)
	}
	return nil
}

// Get 按 doc_id 查询
func (r *DocumentRepo) Get(ctx context.Context, docID string) (*rag.Document, error) {
	var po models.DocumentPO
	err := r.db.WithContext(ctx).Where("doc_id = ?", docID).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return documentToBiz(&po), nil
}

// scopeOr 作用域缺省值：空 → user（个人私有）。库列有 default，这里再兜一层，
// 避免 GORM 显式写空串把默认值顶掉。
func scopeOr(v string) string {
	if strings.TrimSpace(v) == "" {
		return rag.ScopeUser
	}
	return v
}

// visibilityCond 知识可见性条件（与 biz/rag/scope.go 的 Visible 一一对应）：
//
//	scope = 'system'                                   平台通用知识，任何租户可见
//	scope = 'tenant' AND tenant_id = ?                 本租户共享知识
//	scope = 'user'   AND tenant_id = ? AND user_id = ? 个人私有知识（userID 为空时不限用户）
//
// 写成显式 SQL 而不是链式 Or()：这个条件同时承担「隔离」与「共享」两个职责，
// 出错就是越权读取，值得让人一眼能读出来。
func visibilityCond(tenantID, userID string) (string, []any) {
	tenant := ownerOr(tenantID)
	cond := "(scope = ? OR (scope = ? AND tenant_id = ?) OR (scope = ? AND tenant_id = ?"
	args := []any{rag.ScopeSystem, rag.ScopeTenant, tenant, rag.ScopeUser, tenant}
	if userID != "" {
		cond += " AND user_id = ?"
		args = append(args, userID)
	}
	cond += "))"
	return cond, args
}

// List 按更新时间倒序返回文档列表：系统知识 ∪ 本租户共享 ∪ 本人私有。
// scope 非空时再按作用域收窄（前端「只看系统知识」之类的筛选）。
func (r *DocumentRepo) List(ctx context.Context, tenantID, userID, scope string, limit int) ([]rag.Document, error) {
	if limit <= 0 {
		limit = 200
	}
	cond, args := visibilityCond(tenantID, userID)
	q := r.db.WithContext(ctx).Model(&models.DocumentPO{}).Where(cond, args...)
	if scope != "" {
		q = q.Where("scope = ?", scope)
	}
	var pos []models.DocumentPO
	if err := q.Order("updated_at DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]rag.Document, 0, len(pos))
	for i := range pos {
		out = append(out, *documentToBiz(&pos[i]))
	}
	return out, nil
}

// ListDocIDs 当前身份可见的全部文档 ID（向量层白名单 + 图谱召回边界）
func (r *DocumentRepo) ListDocIDs(ctx context.Context, tenantID, userID string) ([]string, error) {
	cond, args := visibilityCond(tenantID, userID)
	var ids []string
	if err := r.db.WithContext(ctx).Model(&models.DocumentPO{}).
		Where(cond, args...).Pluck("doc_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// Delete 删除元数据
func (r *DocumentRepo) Delete(ctx context.Context, docID string) error {
	return r.db.WithContext(ctx).Where("doc_id = ?", docID).Delete(&models.DocumentPO{}).Error
}

// Count 文档总数
func (r *DocumentRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&models.DocumentPO{}).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

func documentToBiz(po *models.DocumentPO) *rag.Document {
	return &rag.Document{
		DocID:     po.DocID,
		UserID:    po.UserID,
		TenantID:  po.TenantID,
		Scope:     po.Scope,
		Source:    po.Source,
		Mode:      po.Mode,
		Status:    po.Status,
		Chars:     po.Chars,
		Chunks:    po.Chunks,
		Entities:  po.Entities,
		Relations: po.Relations,
		Checksum:  po.Checksum,
		CreatedAt: po.CreatedAt,
		UpdatedAt: po.UpdatedAt,
	}
}
