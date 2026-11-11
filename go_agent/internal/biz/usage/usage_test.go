package usage

import (
	"context"
	"testing"

	"go_agent/internal/pkg/principal"
)

type fakeRepo struct{ records []Record }

func (f *fakeRepo) Add(_ context.Context, r *Record) error {
	f.records = append(f.records, *r)
	return nil
}

func (f *fakeRepo) Summary(_ context.Context, _ Filter) ([]Bucket, error) {
	return []Bucket{{Key: "total", TotalTokens: 100, CostMicros: 50, Calls: int64(len(f.records))}}, nil
}

func ctxWith(p *principal.Principal) context.Context {
	return principal.With(context.Background(), p)
}

// 租户与主体由 Principal 决定，不能由请求体改写。
func TestRecordTenantFromPrincipal(t *testing.T) {
	repo := &fakeRepo{}
	uc := New(repo)
	p := &principal.Principal{Issuer: "t", SubjectID: "alice", TenantID: "acme"}

	rec, err := uc.Record(ctxWith(p), p, &Record{Kind: KindLLM, InputTokens: 10, OutputTokens: 5, CostMicros: 3, TenantID: "other"})
	if err == nil {
		t.Fatal("伪造其他租户应拒绝")
	}
	_ = rec

	rec, err = uc.Record(ctxWith(p), p, &Record{Kind: KindLLM, InputTokens: 10, OutputTokens: 5, CostMicros: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rec.TenantID != "acme" || rec.SubjectID != "alice" || rec.TotalTokens != 15 || rec.Currency != "CNY" {
		t.Fatalf("记录归一化不符: %+v", rec)
	}
}

// 未知成本来源直接拒绝，避免脏数据进入账单。
func TestRecordRejectsUnknownKind(t *testing.T) {
	uc := New(&fakeRepo{})
	p := &principal.Principal{Issuer: "t", SubjectID: "alice", TenantID: "acme"}
	if _, err := uc.Record(ctxWith(p), p, &Record{Kind: "magic"}); err == nil {
		t.Fatal("未知 kind 应拒绝")
	}
}

// 成本聚合：普通用户不能跨租户看。
func TestSummaryTenantScope(t *testing.T) {
	uc := New(&fakeRepo{})
	p := &principal.Principal{Issuer: "t", SubjectID: "alice", TenantID: "acme"}
	if _, err := uc.Summary(ctxWith(p), p, Filter{TenantID: "other"}); err == nil {
		t.Fatal("跨租户查看成本应拒绝")
	}
	out, err := uc.Summary(ctxWith(p), p, Filter{GroupBy: "agent"})
	if err != nil || len(out) != 1 {
		t.Fatalf("聚合失败: %v %+v", err, out)
	}
}
