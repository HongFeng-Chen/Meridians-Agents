package models

import "time"

// TemplatePO 模板元信息表（prompt_templates）
type TemplatePO struct {
	ID            uint64    `gorm:"primaryKey;autoIncrement"`
	PromptKey     string    `gorm:"column:prompt_key;size:191;not null;uniqueIndex:uk_prompt_templates_key"`
	Name          string    `gorm:"column:name;size:191;not null;default:''"`
	Scene         string    `gorm:"column:scene;size:32;not null;default:'general';index:idx_prompt_templates_scene"`
	Description   string    `gorm:"column:description;size:512;not null;default:''"`
	Tags          string    `gorm:"column:tags;type:text"`
	ActiveVersion int       `gorm:"column:active_version;not null;default:0"`
	Builtin       bool      `gorm:"column:builtin;not null;default:false"`
	CreatedAt     time.Time `gorm:"column:created_at;not null"`
	UpdatedAt     time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (TemplatePO) TableName() string { return "prompt_templates" }

// VersionPO 版本快照表：(prompt_key, version) 唯一，写入后不再修改（prompt_versions）
type VersionPO struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement"`
	PromptKey   string    `gorm:"column:prompt_key;size:191;not null;uniqueIndex:uk_prompt_versions_key_ver,priority:1"`
	Version     int       `gorm:"column:version;not null;uniqueIndex:uk_prompt_versions_key_ver,priority:2"`
	Content     string    `gorm:"column:content;type:mediumtext;not null"`
	Variables   string    `gorm:"column:variables;type:text"`
	Temperature float64   `gorm:"column:temperature;not null;default:0"`
	MaxTokens   int       `gorm:"column:max_tokens;not null;default:0"`
	Note        string    `gorm:"column:note;size:512;not null;default:''"`
	Author      string    `gorm:"column:author;size:64;not null;default:''"`
	CreatedAt   time.Time `gorm:"column:created_at;not null"`
}

// TableName 表名
func (VersionPO) TableName() string { return "prompt_versions" }

// PromptVersionScorePO 提示词版本评测得分（harness 运行完成后回写，闭环
// 「提示词优化 → 评测验证」）。一个版本可多次评测，查询时取最新一条。
type PromptVersionScorePO struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	PromptKey string    `gorm:"column:prompt_key;size:191;index:idx_pvs_key" json:"prompt_key"`
	Version   int       `gorm:"column:version;index:idx_pvs_key" json:"version"`
	RunID     uint64    `gorm:"column:run_id" json:"run_id"`
	Score     float64   `gorm:"column:score" json:"score"`
	Cases     int       `gorm:"column:cases" json:"cases"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime" json:"created_at"`
}

// TableName 表名
func (PromptVersionScorePO) TableName() string { return "prompt_version_scores" }
