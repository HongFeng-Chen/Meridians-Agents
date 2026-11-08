package models

import "time"

// LLMProviderPO 大模型数据源（llm_providers）
//
// Models / Scenes 用 JSON 字符串存字符串数组，避免逗号分隔的歧义。
// APIKey 明文存储：MySQL 账号权限需收紧；接口输出一律脱敏。
type LLMProviderPO struct {
	ID           uint64    `gorm:"primaryKey;autoIncrement"`
	Name         string    `gorm:"column:name;size:64;not null;uniqueIndex:uk_llm_providers_name"`
	Kind         string    `gorm:"column:kind;size:32;not null;default:'custom'"`
	BaseURL      string    `gorm:"column:base_url;size:512;not null"`
	APIKey       string    `gorm:"column:api_key;type:text"`
	Models       string    `gorm:"column:models;type:text"`
	DefaultModel string    `gorm:"column:default_model;size:191;not null;default:''"`
	Temperature  float64   `gorm:"column:temperature;not null;default:0.3"`
	MaxTokens    int       `gorm:"column:max_tokens;not null;default:0"`
	TimeoutSec   int       `gorm:"column:timeout_seconds;not null;default:120"`
	Enabled      bool      `gorm:"column:enabled;not null;default:true;index:idx_llm_providers_enabled"`
	IsDefault    bool      `gorm:"column:is_default;not null;default:false;index:idx_llm_providers_default"`
	Priority     int       `gorm:"column:priority;not null;default:0"`
	Scenes       string    `gorm:"column:scenes;type:text"`
	Description  string    `gorm:"column:description;size:512;not null;default:''"`
	CreatedAt    time.Time `gorm:"column:created_at;not null"`
	UpdatedAt    time.Time `gorm:"column:updated_at;not null"`
}

// TableName 表名
func (LLMProviderPO) TableName() string { return "llm_providers" }

// LLMCallPO LLM 调用历史（llm_calls）：每次数据源尝试一条。
// 输入/输出内容截断落库（详情展示用）；task_id=0 表示与任务无关的调用。
type LLMCallPO struct {
	ID       uint64 `gorm:"primaryKey;autoIncrement"`
	Provider string `gorm:"column:provider;size:64;not null;default:'';index:idx_llm_calls_provider"`
	Model    string `gorm:"column:model;size:191;not null;default:'';index:idx_llm_calls_model"`
	Scene    string `gorm:"column:scene;size:32;not null;default:'general'"`
	Purpose  string `gorm:"column:purpose;size:64;not null;default:'';index:idx_llm_calls_purpose"`
	TaskID   uint64 `gorm:"column:task_id;not null;default:0;index:idx_llm_calls_task"`
	AgentKey string `gorm:"column:agent_key;size:64;not null;default:''"`
	// 归属：prompt/reply 原文属于用户数据，必须能按人过滤
	TenantID         string    `gorm:"column:tenant_id;size:64;not null;default:'';index:idx_llm_calls_tenant"`
	UserID           string    `gorm:"column:user_id;size:64;not null;default:'';index:idx_llm_calls_user"`
	Temperature      float64   `gorm:"column:temperature;not null;default:0"`
	DurationMS       int64     `gorm:"column:duration_ms;not null;default:0"`
	OK               bool      `gorm:"column:ok;not null;index:idx_llm_calls_ok"`
	ErrorMsg         string    `gorm:"column:error_msg;size:512;not null;default:''"`
	PromptChars      int       `gorm:"column:prompt_chars;not null;default:0"`
	ReplyChars       int       `gorm:"column:reply_chars;not null;default:0"`
	PromptText       string    `gorm:"column:prompt_text;type:mediumtext"`
	ReplyText        string    `gorm:"column:reply_text;type:mediumtext"`
	PromptTokens     int       `gorm:"column:prompt_tokens;not null;default:0"`
	CompletionTokens int       `gorm:"column:completion_tokens;not null;default:0"`
	TotalTokens      int       `gorm:"column:total_tokens;not null;default:0"`
	CreatedAt        time.Time `gorm:"column:created_at;not null;index:idx_llm_calls_created"`
}

// TableName 表名
func (LLMCallPO) TableName() string { return "llm_calls" }
