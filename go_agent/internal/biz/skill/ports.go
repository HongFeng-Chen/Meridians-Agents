package skill

import "context"

// Repo 技能仓库（MySQL 实现，位于 data/db/repo）。
// Save 为 upsert：技能主表按 name 覆盖，文件表整体替换。
type Repo interface {
	List(ctx context.Context, f Filter) ([]Skill, error)
	// Get 返回完整技能（含 content 与 files）
	Get(ctx context.Context, name string) (*Skill, error)
	Save(ctx context.Context, s *Skill) error
	Delete(ctx context.Context, name string) error
	SetEnabled(ctx context.Context, name string, enabled bool) error
	Stats(ctx context.Context) (Stats, error)

	ListHubs(ctx context.Context) ([]Hub, error)
	GetHub(ctx context.Context, name string) (*Hub, error)
	SaveHub(ctx context.Context, h *Hub) error
	DeleteHub(ctx context.Context, name string) error
	SetDefaultHub(ctx context.Context, name string) error
}

// HubClient 技能市场客户端（HTTP 实现，位于 data/third_service/skillhub）。
type HubClient interface {
	Search(ctx context.Context, hub Hub, keyword string, limit int) ([]HubSkill, error)
	// Fetch 拉取技能包；version 为空表示最新版
	Fetch(ctx context.Context, hub Hub, name, version string) (*Skill, error)
	// FetchURL 直接从 URL 拉技能包（zip / SKILL.md / JSON 包），用于「千问技能包」这类
	// 没有市场 API、只给分享链接的分发方式；token 为空表示不带鉴权。
	FetchURL(ctx context.Context, rawURL, token string) (*Skill, error)
}
