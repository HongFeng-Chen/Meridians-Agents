package history

// 操作历史的归属写入（SEC-009 的写入侧）
//
// 历史记录里有查询词、工具参数、召回明细，归属必须在写入时定型：
// 事后靠 task_id / conv_id 反推是不可靠的（手动调用两者都为空）。

import (
	"context"
	"sync"
	"testing"
	"time"

	"go_agent/internal/pkg/identity"
)

type captureRepo struct {
	mu   sync.Mutex
	recs []Record
}

func (c *captureRepo) Insert(_ context.Context, rec *Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, *rec)
	return nil
}

func (c *captureRepo) List(context.Context, Filter) ([]Record, int64, error) { return nil, 0, nil }

func (c *captureRepo) Stats(context.Context, string) (Stats, error) { return Stats{}, nil }

func (c *captureRepo) first(t *testing.T, timeout time.Duration) Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.recs) > 0 {
			r := c.recs[0]
			c.mu.Unlock()
			return r
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("历史记录未落库")
	return Record{}
}

func TestRecordFillsOwner(t *testing.T) {
	repo := &captureRepo{}
	uc := New(repo)

	ctx := identity.With(context.Background(), identity.Info{
		UserID: "alice", TenantID: "acme", Auth: "jwt",
	})
	uc.Record(ctx, &Record{Kind: KindMCPCall, Ref: "demo__calc"})

	got := repo.first(t, time.Second)
	if got.UserID != "alice" || got.TenantID != "acme" {
		t.Fatalf("归属未落库: user=%q tenant=%q", got.UserID, got.TenantID)
	}
}

// 显式指定归属时不被 ctx 覆盖（离线补录、跨用户运维场景）
func TestRecordKeepsExplicitOwner(t *testing.T) {
	repo := &captureRepo{}
	uc := New(repo)

	ctx := identity.With(context.Background(), identity.Info{UserID: "alice", TenantID: "acme", Auth: "jwt"})
	uc.Record(ctx, &Record{Kind: KindRagRecall, UserID: "bob", TenantID: "acme"})

	if got := repo.first(t, time.Second); got.UserID != "bob" {
		t.Fatalf("显式归属被覆盖: %q", got.UserID)
	}
}
