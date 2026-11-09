package history

import "context"

// Repo MySQL 持久化（op_histories 表，实现位于 data/db/repo）。
type Repo interface {
	Insert(ctx context.Context, rec *Record) error
	List(ctx context.Context, f Filter) ([]Record, int64, error)
	// Stats userID 非空时只统计该用户（空 = 全平台，管理员视图）
	// Stats 统计；tenantID / userID 非空时分别按租户、用户收窄（空 = 全平台，仅管理员）
	Stats(ctx context.Context, tenantID, userID string) (Stats, error)
}
