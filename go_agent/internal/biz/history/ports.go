package history

import "context"

// Repo MySQL 持久化（op_histories 表，实现位于 data/db/repo）。
type Repo interface {
	Insert(ctx context.Context, rec *Record) error
	List(ctx context.Context, f Filter) ([]Record, int64, error)
	// Stats userID 非空时只统计该用户（空 = 全平台，管理员视图）
	Stats(ctx context.Context, userID string) (Stats, error)
}
