// Package skill 技能管理域：技能清单存 MySQL，支持在线启停、多文件包、
// 从技能市场（skillhub）搜索与下载安装，并可把启用技能落盘供运行时读取。
//
// 设计要点：
//   - 技能以 name 为稳定标识，重复安装视为升级（保留启用状态由入参决定）；
//   - 技能文件路径必须在技能目录内（拒绝绝对路径与 ..）；
//   - skillhub 地址存 MySQL（skill_hubs），conf 只提供默认兜底；
//   - 落盘是可选能力：sync_dir 为空时只存数据库。
package skill

import (
	"errors"
	"strings"
	"time"
)

// 来源
const (
	SourceHub     = "hub"
	SourceUpload  = "upload"
	SourceURL     = "url"
	SourceBuiltin = "builtin"
)

// 领域错误
var (
	ErrSkillNotFound = errors.New("技能不存在")
	ErrHubNotFound   = errors.New("技能市场不存在")
	ErrNoHub         = errors.New("没有可用的技能市场，请先在 skill_hubs 配置或设置 data.skill.hubBaseURL")
	ErrInvalidName   = errors.New("技能名称不合法（小写字母或数字开头，可含 . _ -，最长 64 字符）")
	ErrEmptyContent  = errors.New("技能内容不能为空（SKILL.md 内容或 files 至少一项）")
	ErrInvalidFile   = errors.New("技能文件路径不合法或超出大小限制")
	ErrHubResponse   = errors.New("技能市场返回内容不合法")
	ErrInvalidURL    = errors.New("技能包地址不合法（只支持 http/https）")
)

// Skill 技能（DO）
type Skill struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Tags        []string  `json:"tags"`
	Source      string    `json:"source"`
	SourceURL   string    `json:"source_url"`
	Enabled     bool      `json:"enabled"`
	Checksum    string    `json:"checksum"`
	Content     string    `json:"content,omitempty"` // SKILL.md
	Files       []File    `json:"files,omitempty"`
	FileCount   int       `json:"file_count"`
	Size        int       `json:"size"`
	InstalledAt time.Time `json:"installed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// File 技能附带文件（文本）
type File struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Size    int    `json:"size"`
}

// Hub 技能市场数据源（DO）
type Hub struct {
	Name        string    `json:"name"`
	BaseURL     string    `json:"base_url"`
	Token       string    `json:"-"`
	HasToken    bool      `json:"has_token"`
	Enabled     bool      `json:"enabled"`
	IsDefault   bool      `json:"is_default"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Masked 脱敏副本（token 只显示末 4 位）
func (h Hub) Masked() Hub {
	h.HasToken = strings.TrimSpace(h.Token) != ""
	h.Token = ""
	return h
}

// HubSkill 市场里的技能条目（搜索结果）
type HubSkill struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Downloads   int      `json:"downloads"`
	Installed   bool     `json:"installed"`
	Hub         string   `json:"hub"`
}

// Filter 列表过滤
type Filter struct {
	Keyword  string
	Category string
	Source   string
	Enabled  *bool
	Limit    int
}

// InstallInput 手动安装/上传技能
type InstallInput struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Content     string   `json:"content"`
	Files       []File   `json:"files"`
	Enabled     *bool    `json:"enabled"`
}

// ImportFromURLInput 从技能包 URL 安装（zip / SKILL.md / JSON 包）
type ImportFromURLInput struct {
	URL     string `json:"url"`
	Name    string `json:"name"` // 包内没有名字时必填
	Version string `json:"version"`
	Token   string `json:"token"` // 私有包：Authorization: Bearer <token>
	Enable  *bool  `json:"enable"`
}

// InstallFromHubInput 从技能市场安装
type InstallFromHubInput struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Hub     string `json:"hub"`
	Enable  *bool  `json:"enable"`
}

// HubInput 新建/修改技能市场
type HubInput struct {
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	Token       string `json:"token"`
	Enabled     *bool  `json:"enabled"`
	IsDefault   bool   `json:"is_default"`
	Description string `json:"description"`
}

// HubPatch 修改技能市场（指针字段为空表示不改）
type HubPatch struct {
	BaseURL     *string `json:"base_url"`
	Token       *string `json:"token"`
	Enabled     *bool   `json:"enabled"`
	IsDefault   *bool   `json:"is_default"`
	Description *string `json:"description"`
}

// SyncResult 落盘结果
type SyncResult struct {
	Dir     string   `json:"dir"`
	Synced  []string `json:"synced"`
	Removed []string `json:"removed"`
}

// Stats 规模统计
type Stats struct {
	Skills  int64 `json:"skills"`
	Enabled int64 `json:"enabled"`
	Hubs    int64 `json:"hubs"`
}
