package aiplatform

import (
	"context"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/skill"
)

// Repo AI 平台仓库（MySQL 实现，位于 data/db/repo）
type Repo interface {
	List(ctx context.Context, f Filter) ([]Platform, error)
	Get(ctx context.Context, name string) (*Platform, error)
	// Save 按 name upsert（created_at 只写一次）
	Save(ctx context.Context, p *Platform) error
	Delete(ctx context.Context, name string) error
	SetDefault(ctx context.Context, name string) error
}

// Catalog 平台 MCP 目录客户端（HTTP 实现，位于 data/third_service/aiplatform）。
//
// 约定：只负责「问平台要一份列表」，不做业务判断（是否已接入、命名空间等由 UseCase 做）；
// 目录不可用时应返回包裹 ErrCatalogUnsupported 的错误，由上层给出「按编码接入」的替代路径。
type Catalog interface {
	Search(ctx context.Context, p Platform, keyword string, limit int) ([]RemoteMCP, error)
}

// MCPRegistry 本地 MCP 服务注册表（*mcp.UseCase 直接实现）。
//
// 只声明用得到的 5 个方法，避免 biz 之间耦合过宽；接入 = 建服务 → 连接 → 同步工具，
// 后续的向量化、能力目录、Redlock 等全部复用 mcp 模块既有实现。
type MCPRegistry interface {
	ListServers(ctx context.Context, f mcp.ServerFilter) ([]mcp.Server, error)
	CreateServer(ctx context.Context, in mcp.ServerInput) (*mcp.Server, error)
	DeleteServer(ctx context.Context, id uint64) error
	ConnectServer(ctx context.Context, id uint64) (mcp.Status, error)
	SyncTools(ctx context.Context, id uint64) (*mcp.SyncResult, error)
}

// SkillMarkets 技能市场（*skill.UseCase 直接实现）：平台可顺带登记技能源。
type SkillMarkets interface {
	ListHubs(ctx context.Context) ([]skill.Hub, error)
	CreateHub(ctx context.Context, in skill.HubInput) (*skill.Hub, error)
	UpdateHub(ctx context.Context, name string, in skill.HubPatch) (*skill.Hub, error)
}
