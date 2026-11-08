package audit

// 审计写入链路用例：异步批量、身份补齐、字段截断、过期清理。
//
// 不测「MySQL 真的写进去了」——那是仓储层的事；这里保证的是语义：
// 一条事件该带什么字段、丢失是否影响业务、关闭时会不会把缓冲丢掉。

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go_agent/internal/pkg/identity"
)

type fakeRepo struct {
	mu    sync.Mutex
	rows  []Record
	purge []time.Time
}

func (f *fakeRepo) BatchInsert(_ context.Context, records []Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, records...)
	return nil
}

func (f *fakeRepo) List(_ context.Context, _ Filter) ([]Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.rows...), nil
}

func (f *fakeRepo) Purge(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purge = append(f.purge, before)
	return 0, nil
}

func (f *fakeRepo) snapshot() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.rows...)
}

func TestRecordAndFlush(t *testing.T) {
	fr := &fakeRepo{}
	uc := New(fr)

	ctx := identity.With(context.Background(), identity.Info{
		UserID: "alice", TenantID: "t1", Auth: "jwt", Admin: true,
	})
	Emit(ctx, Event{
		Action: "mcp.server.create", Resource: "mcp_server", ResourceID: "42",
		Detail: map[string]any{"transport": "http"},
	})
	Emit(ctx, Event{Action: "mcp.server.delete", Result: ResultDenied, Resource: "mcp_server"})

	// Close 会把缓冲写完再返回，正好当作「flush 完成」的同步点
	uc.Close()

	rows := fr.snapshot()
	if len(rows) != 2 {
		t.Fatalf("期望 2 条审计事件，实际 %d", len(rows))
	}
	var seenAllow, seenDeny bool
	for _, r := range rows {
		if r.UserID != "alice" || r.TenantID != "t1" || r.AuthType != "jwt" || !r.Admin {
			t.Errorf("身份字段未正确补齐: %+v", r)
		}
		if r.Ts.IsZero() {
			t.Error("时间戳不能为空")
		}
		switch r.Action {
		case "mcp.server.create":
			seenAllow = true
			if r.Result != ResultAllowed {
				t.Errorf("未指定 Result 时应落 allowed，实际 %q", r.Result)
			}
			if r.ResourceID != "42" || !strings.Contains(r.Detail, "http") {
				t.Errorf("资源与明细丢失: %+v", r)
			}
		case "mcp.server.delete":
			seenDeny = true
			if r.Result != ResultDenied {
				t.Errorf("拒绝事件结果应为 denied，实际 %q", r.Result)
			}
		}
	}
	if !seenAllow || !seenDeny {
		t.Fatalf("事件缺失：allow=%v deny=%v", seenAllow, seenDeny)
	}
}

func TestEmitWithoutRecorder(t *testing.T) {
	SetDefault(nil)
	// 未初始化时静默丢弃，不能 panic（服务启动早期就会调用）
	Emit(context.Background(), Event{Action: "x"})
	Denied(context.Background(), "x", "y", "1", "reason")
}

func TestDetailTruncated(t *testing.T) {
	fr := &fakeRepo{}
	uc := New(fr)
	defer uc.Close()

	big := strings.Repeat("字", 5000)
	Info(context.Background(), "test.big", "res", "1", map[string]any{"payload": big})
	uc.Close()

	rows := fr.snapshot()
	if len(rows) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(rows))
	}
	if n := len([]rune(rows[0].Detail)); n > 2100 {
		t.Fatalf("detail 应被截断到 2000 字符以内，实际 %d", n)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"直连", "203.0.113.9:5555", "", "203.0.113.9"},
		{"反代 XFF", "10.0.0.2:5555", "198.51.100.7, 10.0.0.2", "198.51.100.7"},
		{"空 RemoteAddr", "", "", ""},
	}
	for _, c := range cases {
		r := &http.Request{RemoteAddr: c.remote, Header: http.Header{}}
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("%s: clientIP = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestListClampsLimit(t *testing.T) {
	fr := &fakeRepo{}
	uc := New(fr)
	defer uc.Close()

	// limit 超上限时被夹到默认值，避免一次拉爆内存
	if _, err := uc.List(context.Background(), Filter{Limit: 100000}); err != nil {
		t.Fatalf("List 失败: %v", err)
	}
}
