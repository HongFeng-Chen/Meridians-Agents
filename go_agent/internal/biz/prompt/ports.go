package prompt

import (
	"context"
	"time"
)

// Repo 模板仓库（MySQL 实现，位于 data/db/repo）。
//
// 版本只增不改：AddVersion 负责分配新的版本号，Activate 只改指针。
// 存储实现需保证「版本号分配」与「模板存在性校验」在同一事务内完成。
type Repo interface {
	// Create 建模板并写入 v1，同时置为生效版本（原子）
	Create(ctx context.Context, p *Prompt, v *Version) error
	Get(ctx context.Context, key string) (*Prompt, error)
	List(ctx context.Context, f ListFilter) ([]Prompt, error)
	// Update 只更新元信息（名称/场景/描述/标签）
	Update(ctx context.Context, p *Prompt) error
	// Delete 删除模板及其全部版本
	Delete(ctx context.Context, key string) error

	// AddVersion 追加版本并返回新版本号（不激活）
	AddVersion(ctx context.Context, v *Version) (int, error)
	GetVersion(ctx context.Context, key string, version int) (*Version, error)
	// ActiveVersion 返回当前生效版本
	ActiveVersion(ctx context.Context, key string) (*Version, error)
	// ListVersions 按版本号倒序返回
	ListVersions(ctx context.Context, key string, limit int) ([]Version, error)
	// Activate 把生效指针指向指定版本
	Activate(ctx context.Context, key string, version int) error

	Stats(ctx context.Context) (Stats, error)

	// InsertVersionScore 追加版本评测得分（harness 评测闭环回写）
	InsertVersionScore(ctx context.Context, s VersionScore) error
	// ListVersionScores 模板的版本得分列表（时间倒序）
	ListVersionScores(ctx context.Context, key string, limit int) ([]VersionScore, error)
}

// Cache 活跃版本缓存（Redis 实现，位于 data/redis）。
//
// 缓存的是「已解析的生效版本」，发布/激活/删除后必须失效，
// 否则线上会继续用旧提示词。
type Cache interface {
	GetVersion(ctx context.Context, key string) (*Version, bool)
	PutVersion(ctx context.Context, key string, v *Version, ttl time.Duration)
	Invalidate(ctx context.Context, key string) error
}
