package models

import "time"

// SkillPO 技能主表（skills）：content 存 SKILL.md 正文
type SkillPO struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement"`
	Name        string    `gorm:"column:name;size:64;not null;uniqueIndex:uk_skills_name"`
	Version     string    `gorm:"column:version;size:32;not null;default:'1.0.0'"`
	Title       string    `gorm:"column:title;size:191;not null;default:''"`
	Description string    `gorm:"column:description;size:1024;not null;default:''"`
	Category    string    `gorm:"column:category;size:64;not null;default:'';index:idx_skills_category"`
	Tags        string    `gorm:"column:tags;type:text"`
	Source      string    `gorm:"column:source;size:16;not null;default:'upload';index:idx_skills_source"`
	SourceURL   string    `gorm:"column:source_url;size:512;not null;default:''"`
	Enabled     bool      `gorm:"column:enabled;not null;default:true;index:idx_skills_enabled"`
	Checksum    string    `gorm:"column:checksum;size:64;not null;default:''"`
	Content     string    `gorm:"column:content;type:mediumtext"`
	FileCount   int       `gorm:"column:file_count;not null;default:0"`
	Size        int       `gorm:"column:size;not null;default:0"`
	InstalledAt time.Time `gorm:"column:installed_at;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (SkillPO) TableName() string { return "skills" }

// SkillFilePO 技能文件表（skill_files）：按技能名 + 相对路径唯一
type SkillFilePO struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement"`
	SkillName string    `gorm:"column:skill_name;size:64;not null;uniqueIndex:uk_skill_files_name_path,priority:1;index:idx_skill_files_name"`
	Path      string    `gorm:"column:path;size:512;not null;uniqueIndex:uk_skill_files_name_path,priority:2"`
	Content   string    `gorm:"column:content;type:mediumtext"`
	Size      int       `gorm:"column:size;not null;default:0"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

// TableName 表名
func (SkillFilePO) TableName() string { return "skill_files" }

// SkillHubPO 技能市场数据源（skill_hubs）
type SkillHubPO struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement"`
	Name        string    `gorm:"column:name;size:64;not null;uniqueIndex:uk_skill_hubs_name"`
	BaseURL     string    `gorm:"column:base_url;size:512;not null"`
	Token       string    `gorm:"column:token;type:text"`
	Enabled     bool      `gorm:"column:enabled;not null;default:true"`
	IsDefault   bool      `gorm:"column:is_default;not null;default:false;index:idx_skill_hubs_default"`
	Description string    `gorm:"column:description;size:512;not null;default:''"`
	CreatedAt   time.Time `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (SkillHubPO) TableName() string { return "skill_hubs" }
