package models

import "time"

// MemoryPO 记忆表映射（memories）
type MemoryPO struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement"`
	MemoryID   string `gorm:"column:memory_id;size:64;not null;uniqueIndex:uk_memories_memory_id"`
	UserID     string `gorm:"column:user_id;size:128;not null;default:'default';index:idx_memories_user"`
	Kind       string `gorm:"column:kind;size:32;not null;default:'fact'"`
	Content    string `gorm:"column:content;type:text;not null"`
	Importance int    `gorm:"column:importance;not null;default:3"`
	Tags       string `gorm:"column:tags;size:2048;not null;default:'[]'"`
	Source     string `gorm:"column:source;size:512;not null;default:''"`
	RuleName   string `gorm:"column:rule_name;size:128;not null;default:''"` // 提取来源规则名
	Checksum   string `gorm:"column:checksum;size:64;not null;default:'';uniqueIndex:uk_memories_user_checksum,priority:2"`
	Entities   int    `gorm:"column:entities;not null;default:0"`
	Relations  int    `gorm:"column:relations;not null;default:0"`
	// 召回统计（检索历史联动）
	RecallCount    int        `gorm:"column:recall_count;not null;default:0"`
	LastRecalledAt *time.Time `gorm:"column:last_recalled_at"`
	ExpiresAt      *time.Time `gorm:"column:expires_at;index:idx_memories_expires"`
	CreatedAt      time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (MemoryPO) TableName() string { return "memories" }

// MemoryRulePO 记忆提取规则表映射（memory_rules）。
// 内置三种（builtin=true）：事实记忆 / 用户画像 / 技能记忆；custom 由用户自建。
type MemoryRulePO struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement"`
	Name        string    `gorm:"column:name;size:128;not null"`
	Type        string    `gorm:"column:type;size:16;not null;default:'custom'"` // fact|profile|skill|custom
	Kind        string    `gorm:"column:kind;size:32;not null;default:'fact'"`   // 提取出的记忆类型
	Instruction string    `gorm:"column:instruction;type:text;not null"`
	Builtin     bool      `gorm:"column:builtin;not null;default:false"`
	Enabled     bool      `gorm:"column:enabled;not null;default:true"`
	CreatedAt   time.Time `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (MemoryRulePO) TableName() string { return "memory_rules" }

// MemoryRecallLogPO 记忆检索历史表映射（memory_recall_logs）。
type MemoryRecallLogPO struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement"`
	UserID    string    `gorm:"column:user_id;size:128;not null;default:'default';index:idx_recall_logs_user"`
	Query     string    `gorm:"column:query;type:text;not null"`
	Source    string    `gorm:"column:source;size:32;not null;default:'api'"` // chat|api|answer
	TopK      int       `gorm:"column:top_k;not null;default:0"`
	Hits      int       `gorm:"column:hits;not null;default:0"`
	MemoryIDs string    `gorm:"column:memory_ids;size:2048;not null;default:''"` // 逗号分隔
	LatencyMS int64     `gorm:"column:latency_ms;not null;default:0"`
	CreatedAt time.Time `gorm:"column:created_at;not null;index:idx_recall_logs_created"`
}

// TableName 表名
func (MemoryRecallLogPO) TableName() string { return "memory_recall_logs" }
