package service

// 管理面闸门的回归用例：放行/拒绝的判定 + 审计留痕。
//
// 审计断言是这次改造的重点：以前只有日志，等于「查不到」。
// 现在每次判定都要落一条 audit_events（allowed 与 denied 都要）。

import (
	"context"
	"sync"
	"testing"
	"time"

	"go_agent/internal/biz/audit"
	"go_agent/internal/pkg/identity"

	kerrors "github.com/go-kratos/kratos/v3/errors"
)

type memRepo struct {
	mu   sync.Mutex
	rows []audit.Record
}

func (m *memRepo) BatchInsert(_ context.Context, records []audit.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, records...)
	return nil
}

func (m *memRepo) List(_ context.Context, _ audit.Filter) ([]audit.Record, error) { return nil, nil }
func (m *memRepo) Purge(_ context.Context, _ time.Time) (int64, error)            { return 0, nil }

func (m *memRepo) snapshot() []audit.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]audit.Record(nil), m.rows...)
}

// TestRequireAdmin 管理面闸门：只有显式管理员或「开发态 + 回环」才放行。
func TestRequireAdmin(t *testing.T) {
	cases := []struct {
		name string
		info identity.Info
		ok   bool
	}{
		{"JWT 管理员", identity.Info{UserID: "ops", Auth: "jwt", Admin: true}, true},
		{"静态令牌管理员", identity.Info{UserID: "ops", Auth: "static", Admin: true}, true},
		{"开发态本机", identity.Info{UserID: "dev", Auth: "dev", Local: true}, true},
		{"开发态远程匿名", identity.Info{UserID: "dev", Auth: "dev"}, false},
		{"已登录普通用户", identity.Info{UserID: "u1", Auth: "jwt"}, false},
		{"无身份", identity.Info{}, false},
	}
	for _, c := range cases {
		err := requireAdmin(identity.With(context.Background(), c.info), "mcp.server.create")
		if c.ok && err != nil {
			t.Errorf("%s: 期望放行, got %v", c.name, err)
		}
		if !c.ok {
			if err == nil {
				t.Errorf("%s: 期望 403, got nil", c.name)
				continue
			}
			if code := kerrors.FromError(err).Code; code != 403 {
				t.Errorf("%s: 期望 403, got %d", c.name, code)
			}
		}
	}
}

// TestRequireAdminAudit 放行与拒绝都要落审计事件，且字段能回答
// 「谁、什么动作、结果如何、为什么」。
func TestRequireAdminAudit(t *testing.T) {
	repo := &memRepo{}
	uc := audit.New(repo)
	t.Cleanup(func() {
		uc.Close()
		audit.SetDefault(nil)
	})

	// 管理员放行
	adminCtx := identity.With(context.Background(), identity.Info{
		UserID: "ops", TenantID: "t1", Auth: "jwt", Admin: true,
	})
	if err := requireAdmin(adminCtx, "skill.install"); err != nil {
		t.Fatalf("管理员应放行: %v", err)
	}
	// 普通用户被拒
	userCtx := identity.With(context.Background(), identity.Info{
		UserID: "u1", TenantID: "t1", Auth: "jwt",
	})
	if err := requireAdmin(userCtx, "skill.install"); err == nil {
		t.Fatal("普通用户应被拒")
	}

	// 等后台批量写入（异步，最多 2s flush 一次）
	deadline := time.Now().Add(5 * time.Second)
	for len(repo.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	rows := repo.snapshot()
	if len(rows) < 2 {
		t.Fatalf("期望 2 条审计事件，实际 %d", len(rows))
	}
	var allowed, denied bool
	for _, r := range rows {
		if r.Action != "skill.install" {
			continue
		}
		switch r.Result {
		case audit.ResultAllowed:
			allowed = true
			if r.UserID != "ops" {
				t.Errorf("放行事件的用户不对: %+v", r)
			}
		case audit.ResultDenied:
			denied = true
			if r.UserID != "u1" {
				t.Errorf("拒绝事件的用户不对: %+v", r)
			}
		}
	}
	if !allowed || !denied {
		t.Fatalf("审计事件缺失：allowed=%v denied=%v（rows=%+v）", allowed, denied, rows)
	}
}
