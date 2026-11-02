package llm

import "context"

// ProviderRepo 数据源仓库（MySQL 实现，位于 data/db/repo）。
type ProviderRepo interface {
	List(ctx context.Context) ([]Provider, error)
	// Get 按名称查询；不存在返回 ErrProviderNotFound
	Get(ctx context.Context, name string) (*Provider, error)
	Create(ctx context.Context, p *Provider) error
	Update(ctx context.Context, p *Provider) error
	Delete(ctx context.Context, name string) error
	// SetDefault 把默认标记切到指定数据源（同一事务内清掉其它默认）
	SetDefault(ctx context.Context, name string) error
	Stats(ctx context.Context) (Stats, error)
}

// CallRepo LLM 调用历史仓库（MySQL 实现）。写路径异步，任何错误只告警不断业务。
type CallRepo interface {
	Insert(ctx context.Context, rec CallRecord) error
	List(ctx context.Context, f CallFilter) ([]CallRecord, int64, error)
	Stats(ctx context.Context) (CallStats, error)
	// Breakdown token 用量下钻：按 dim（agent_key/scene/purpose/model）聚合排行
	Breakdown(ctx context.Context, dim string, days int) ([]UsageRow, error)
}
