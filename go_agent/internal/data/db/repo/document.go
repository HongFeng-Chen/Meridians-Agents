// document.go RAG 文档元数据仓储：
// 文档列表、内容校验和（去重）、入库状态，是文档管理接口的权威数据源。
package repo

import (
	"context"
	"errors"
	"fmt"

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
		DoUpdates: clause.AssignmentColumns([]string{
			"source", "mode", "status", "chars", "chunks", "entities", "relations", "checksum", "updated_at",
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

// List 按更新时间倒序返回文档列表
func (r *DocumentRepo) List(ctx context.Context, limit int) ([]rag.Document, error) {
	if limit <= 0 {
		limit = 200
	}
	var pos []models.DocumentPO
	if err := r.db.WithContext(ctx).Order("updated_at DESC").Limit(limit).Find(&pos).Error; err != nil {
		return nil, err
	}
	out := make([]rag.Document, 0, len(pos))
	for i := range pos {
		out = append(out, *documentToBiz(&pos[i]))
	}
	return out, nil
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
