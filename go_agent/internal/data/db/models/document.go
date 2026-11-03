package models

import "time"

// DocumentPO 文档元数据表映射（rag_documents）
type DocumentPO struct {
	ID    uint64 `gorm:"primaryKey;autoIncrement"`
	DocID string `gorm:"column:doc_id;size:191;not null;uniqueIndex:uk_rag_documents_doc_id"`
	// UserID 文档归属用户（隔离键）：检索时按 doc_id 白名单做向量层过滤，
	// 未经改造的历史文档默认 'default'（认证未启用时与开发态同作用域）。
	UserID    string    `gorm:"column:user_id;size:128;not null;default:'default';index:idx_rag_documents_user"`
	Source    string    `gorm:"column:source;size:512;not null;default:''"`
	Mode      string    `gorm:"column:mode;size:16;not null;default:'rag';index:idx_rag_documents_mode"`
	Status    string    `gorm:"column:status;size:16;not null;default:'ready';index:idx_rag_documents_status"`
	Chars     int       `gorm:"column:chars;not null;default:0"`
	Chunks    int       `gorm:"column:chunks;not null;default:0"`
	Entities  int       `gorm:"column:entities;not null;default:0"`
	Relations int       `gorm:"column:relations;not null;default:0"`
	Checksum  string    `gorm:"column:checksum;size:64;not null;default:''"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (DocumentPO) TableName() string { return "rag_documents" }
