// Package prompt 提示词工程域：模板登记、版本化发布、变量渲染与在线生效。
//
// 设计要点：
//   - Prompt 是模板的稳定标识（key），版本只增不改，避免线上行为漂移；
//   - 任一时刻只有一个 active 版本，回滚即把 active 指回旧版本；
//   - 渲染走「Redis 缓存 → MySQL 活跃版本 → 内置默认」三级回落，
//     存储异常时问答链路仍可用。
package prompt

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 内置模板 key：业务代码直接引用常量，避免拼写漂移。
const (
	KeyRAGSystem     = "rag.qa.system"
	KeyRAGUser       = "rag.qa.user"
	KeyGraphSystem   = "graph.answer.system"
	KeyGraphUser     = "graph.answer.user"
	KeyExtractSystem = "graph.extract.system"
	KeyExtractUser   = "graph.extract.user"
	KeyMemorySystem  = "memory.answer.system"
	KeyMemoryUser    = "memory.answer.user"
)

// 场景：管理端按场景分类筛选。
const (
	SceneRAG     = "rag"
	SceneGraph   = "graph"
	SceneExtract = "extract"
	SceneMemory  = "memory"
	SceneGeneral = "general"
)

// 领域错误：service 层据此映射 HTTP 状态码。
var (
	ErrNotFound         = errors.New("提示词不存在")
	ErrVersionNotFound  = errors.New("提示词版本不存在")
	ErrExists           = errors.New("提示词 key 已存在")
	ErrInvalidKey       = errors.New("提示词 key 不合法（字母开头，可含字母数字 _ . -，最长 128 字符）")
	ErrEmptyContent     = errors.New("提示词内容不能为空")
	ErrBuiltinProtected = errors.New("内置提示词不可删除，可用 reset 恢复默认内容")
	ErrNoPrevious       = errors.New("没有可回滚的历史版本")
)

// MissingVarsError 渲染时缺少模板变量
type MissingVarsError struct {
	Key   string
	Names []string
}

func (e *MissingVarsError) Error() string {
	return fmt.Sprintf("提示词 %s 缺少变量: %s", e.Key, strings.Join(e.Names, ", "))
}

// Prompt 模板元信息（DO）
type Prompt struct {
	Key           string    `json:"key"`
	Name          string    `json:"name"`
	Scene         string    `json:"scene"`
	Description   string    `json:"description"`
	Tags          []string  `json:"tags"`
	ActiveVersion int       `json:"active_version"`
	Builtin       bool      `json:"builtin"`
	VersionCount  int       `json:"version_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Version 不可变的版本快照（DO）
type Version struct {
	PromptKey   string    `json:"prompt_key"`
	Version     int       `json:"version"`
	Content     string    `json:"content"`
	Variables   []string  `json:"variables"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	Note        string    `json:"note"`
	Author      string    `json:"author"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
}

// Detail 详情：模板元信息 + 当前生效版本
type Detail struct {
	Prompt
	Active *Version `json:"active"`
}

// ListFilter 列表过滤条件
type ListFilter struct {
	Scene   string
	Keyword string
	Limit   int
}

// Stats 提示词规模
type Stats struct {
	Prompts  int64 `json:"prompts"`
	Versions int64 `json:"versions"`
}

// CreateInput 新建模板
type CreateInput struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Scene       string   `json:"scene"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Content     string   `json:"content"`
	Temperature float64  `json:"temperature"`
	MaxTokens   int      `json:"max_tokens"`
	Note        string   `json:"note"`
	Author      string   `json:"author"`
}

// UpdateInput 修改元信息（只改元信息，内容走版本发布）
type UpdateInput struct {
	Name        *string   `json:"name"`
	Scene       *string   `json:"scene"`
	Description *string   `json:"description"`
	Tags        *[]string `json:"tags"`
}

// PublishInput 发布新版本
type PublishInput struct {
	Content     string  `json:"content"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	Note        string  `json:"note"`
	Author      string  `json:"author"`
	// Activate 发布后是否立即生效；缺省 true（一次调用完成发布+上线），
	// 显式传 false 表示只存草稿，稍后再 activate。
	Activate *bool `json:"activate"`
}

// RenderInput 预览渲染请求
type RenderInput struct {
	Vars map[string]string `json:"vars"`
	// Version 为 0 时用当前生效版本
	Version int `json:"version"`
}

// RenderResult 预览渲染结果（缺变量不报错，直接列出来给前端高亮）
type RenderResult struct {
	Key       string   `json:"key"`
	Version   int      `json:"version"`
	Content   string   `json:"content"`
	Rendered  string   `json:"rendered"`
	Variables []string `json:"variables"`
	Missing   []string `json:"missing"`
}

// VersionScore 提示词版本的一次评测得分（harness 运行回写）
type VersionScore struct {
	PromptKey string    `json:"prompt_key"`
	Version   int       `json:"version"`
	RunID     uint64    `json:"run_id"`
	Score     float64   `json:"score"`
	Cases     int       `json:"cases"`
	CreatedAt time.Time `json:"created_at"`
}
