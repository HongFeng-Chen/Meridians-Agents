package models

import "time"

// AIPlatformPO 外部 AI 平台数据源（ai_platforms）。
//
// 一个平台 = 一份「怎么连它」的配置：网关地址、鉴权、路径模板。
// 千问/百炼共用 https://dashscope.aliyuncs.com，MCP 服务地址形如
// /api/v1/mcps/{code}/mcp（Streamable HTTP）或 /sse。
//
// APIKey 与 skill_hubs.token 一样是明文存储、接口只回 has_key：
// 这类平台凭据是「本机服务凭据」，不做加密存储，部署时靠库权限与网络隔离保护。
type AIPlatformPO struct {
	ID   uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	Name string `gorm:"column:name;size:64;not null;uniqueIndex:uk_ai_platforms_name" json:"name"`
	Kind string `gorm:"column:kind;size:32;not null;default:'generic'" json:"kind"`

	BaseURL    string `gorm:"column:base_url;size:512;not null;default:''" json:"base_url"`
	APIKey     string `gorm:"column:api_key;type:text" json:"-"`
	AuthScheme string `gorm:"column:auth_scheme;size:16;not null;default:'bearer'" json:"auth_scheme"`
	AuthHeader string `gorm:"column:auth_header;size:64;not null;default:''" json:"auth_header"`
	Headers    string `gorm:"column:headers;type:json" json:"headers"`

	CatalogPath    string `gorm:"column:catalog_path;size:512;not null;default:''" json:"catalog_path"`
	StreamablePath string `gorm:"column:streamable_path;size:512;not null;default:''" json:"streamable_path"`
	SSEPath        string `gorm:"column:sse_path;size:512;not null;default:''" json:"sse_path"`

	SkillBaseURL string `gorm:"column:skill_base_url;size:512;not null;default:''" json:"skill_base_url"`
	SkillHub     string `gorm:"column:skill_hub;size:64;not null;default:''" json:"skill_hub"`

	Description string `gorm:"column:description;size:512;not null;default:''" json:"description"`
	Enabled     bool   `gorm:"column:is_enabled;not null;default:true;index:idx_ai_platforms_enabled" json:"is_enabled"`
	IsDefault   bool   `gorm:"column:is_default;not null;default:false;index:idx_ai_platforms_default" json:"is_default"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 表名
func (AIPlatformPO) TableName() string { return "ai_platforms" }
