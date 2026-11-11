package models

import "time"

// DocumentPO 文档元数据表映射（rag_documents）
type DocumentPO struct {
	ID    uint64 `gorm:"primaryKey;autoIncrement"`
	DocID string `gorm:"column:doc_id;size:191;not null;uniqueIndex:uk_rag_documents_doc_id"`
	// UserID 文档归属用户（隔离键）：检索时按 doc_id 白名单做向量层过滤，
	// 未经改造的历史文档默认 'default'（认证未启用时与开发态同作用域）。
	UserID string `gorm:"column:user_id;size:128;not null;default:'default';index:idx_rag_documents_user"`
	// TenantID 租户隔离键：同名校验只要求「租户内唯一」的语义，先于 user_id 生效。
	// 默认 'public'：加列时 MySQL 会用默认值回填历史行，不会把旧数据变成「谁都能看」。
	TenantID string `gorm:"column:tenant_id;size:128;not null;default:'public';index:idx_rag_documents_tenant"`
	// Scope 知识作用域：system（平台通用，全租户可见）/ tenant（同租户可见）/
	// user（个人私有）。默认 user：加列时历史行自动落到「个人私有」，
	// 不会因为新增字段而突然对更多人可见。
	Scope string `gorm:"column:scope;size:16;not null;default:'user';index:idx_rag_documents_scope"`
	// SpaceID 所属知识空间（V2）：与 scope 双写，迁移期为空表示尚未迁移。
	// 检索白名单优先按空间授权计算，空间集合之外再回落到三层作用域。
	SpaceID   string    `gorm:"column:space_id;size:64;not null;default:'';index:idx_rag_documents_space"`
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
