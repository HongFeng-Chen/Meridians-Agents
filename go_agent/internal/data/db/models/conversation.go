package models

// conversations 会话目录：会话 ID 复用任务 ID（agent 会话）或自动生成；
// title / agent_key / 最近消息预览冗余存储，列表页免联查。
import "time"

// ConversationPO 会话（conversations）
type ConversationPO struct {
	ID string `gorm:"column:id;primaryKey;size:64"`
	// UserID 会话归属用户（多租户隔离键）：列表/详情只对归属人可见。
	// 默认 'default' 兼容认证未启用时的单机开发态与历史数据。
	UserID       string `gorm:"column:user_id;size:128;not null;default:'default';index:idx_conversations_user"`
	Title        string `gorm:"column:title;size:128;not null;default:'';index:idx_conversations_title"`
	AgentKey     string `gorm:"column:agent_key;size:64;not null;default:''"`
	MessageCount int64  `gorm:"column:message_count;not null;default:0"`
	// LastMessage 最新一条消息预览（截断），列表展示用
	LastMessage string    `gorm:"column:last_message;size:255;not null;default:''"`
	CreatedAt   time.Time `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null;index:idx_conversations_updated"`
}

// TableName 表名
func (ConversationPO) TableName() string { return "conversations" }

// ConversationMessagePO 会话消息（conversation_messages）
type ConversationMessagePO struct {
	ID             int64  `gorm:"primaryKey;autoIncrement"`
	ConversationID string `gorm:"column:conversation_id;size:64;not null;index:idx_conv_messages_conv"`
	Role           string `gorm:"column:role;size:16;not null"`
	// Speaker 发言者标识：普通会话为空（user/assistant 已表达）；辩论会话为角色名 / 主持人
	Speaker string `gorm:"column:speaker;size:64;not null;default:''"`
	Content string `gorm:"column:content;type:mediumtext;not null"`
	// Meta 能力调用链路 JSON：{op:"agent",task_id} / {op:"rag",kind,hits} / {op:"mcp",tool,ok} …
	Meta string `gorm:"column:meta;type:text"`
	// Attachments 消息附件 JSON：[{name,url,mime,size}]（空数组/空串 = 无附件）
	Attachments string    `gorm:"column:attachments;type:text"`
	TaskID      uint64    `gorm:"column:task_id;not null;default:0"`
	CreatedAt   time.Time `gorm:"column:created_at;not null;index:idx_conv_messages_created"`
}

// TableName 表名
func (ConversationMessagePO) TableName() string { return "conversation_messages" }

// DebatePO 辩论会话（debates）：一个会话同一时间至多一个进行中的辩论
type DebatePO struct {
	ID             int64  `gorm:"primaryKey;autoIncrement"`
	ConversationID string `gorm:"column:conversation_id;size:64;not null;index:idx_debates_conv"`
	// Topic 初始议题
	Topic string `gorm:"column:topic;type:text;not null"`
	// Status running | stopping | finished | stopped
	Status    string `gorm:"column:status;size:16;not null;default:'running'"`
	Round     int    `gorm:"column:round;not null;default:0"`
	MaxRounds int    `gorm:"column:max_rounds;not null;default:4"`
	// EndRule rounds=跑满轮次上限；consensus=每轮后由 LLM 判定是否提前收敛
	EndRule string `gorm:"column:end_rule;size:16;not null;default:'rounds'"`
	// Roles 角色 JSON：[{name,persona,stance,style}]
	Roles      string     `gorm:"column:roles;type:text;not null"`
	CreatedAt  time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt  time.Time  `gorm:"column:updated_at;not null"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
}

// TableName 表名
func (DebatePO) TableName() string { return "debates" }
