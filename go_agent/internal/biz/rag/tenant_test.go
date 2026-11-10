package rag

// P1-01（文档侧）：租户隔离。
// 验收标准是「两个不同 Tenant 即使使用相同 UserID，也不能互相读取数据」，
// 所以这里刻意用同一个 userID、只换 tenant，验证隔离确实来自租户而不是用户。

import (
	"context"
	"strings"
	"testing"

	"go_agent/internal/pkg/identity"
)

func tenantCtx(userID, tenantID string) context.Context {
	return identity.With(context.Background(), identity.Info{UserID: userID, TenantID: tenantID})
}

// 同 userID 不同租户：文档列表与检索白名单互不可见
func TestDocumentsIsolatedByTenant(t *testing.T) {
	r, _, _, _, _ := newTestRag(t)
	ctxA := tenantCtx("alice", "t1")
	ctxB := tenantCtx("alice", "t2")

	if _, err := r.Ingest(ctxA, "doc-a", "a.txt", "rag", strings.Repeat("租户 A 的文档内容。", 20)); err != nil {
		t.Fatalf("租户 A 入库: %v", err)
	}

	docsA, err := r.ListDocs(ctxA, "", 20)
	if err != nil {
		t.Fatalf("ListDocs A: %v", err)
	}
	if len(docsA) != 1 {
		t.Fatalf("租户 A 应看到自己的 1 篇文档，实际 %d", len(docsA))
	}
	if docsA[0].TenantID != "t1" {
		t.Fatalf("文档归属租户应为 t1，实际 %q", docsA[0].TenantID)
	}

	docsB, err := r.ListDocs(ctxB, "", 20)
	if err != nil {
		t.Fatalf("ListDocs B: %v", err)
	}
	if len(docsB) != 0 {
		t.Fatalf("同 userID 的另一租户不应看到文档，实际 %d 篇", len(docsB))
	}

	// 检索白名单同口径：B 拿不到任何 doc_id，检索直接返回空，不会跑到向量层
	filter, empty, err := r.DocFilterForUser(ctxB, "alice")
	if err != nil {
		t.Fatalf("DocFilterForUser B: %v", err)
	}
	if !empty || filter != "" {
		t.Fatalf("B 的白名单应为空，实际 empty=%v filter=%q", empty, filter)
	}
	hits, err := r.RetrieveForUser(ctxB, "租户", "alice")
	if err != nil {
		t.Fatalf("RetrieveForUser B: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("B 不应召回到任何分块，实际 %d 条", len(hits))
	}
}

// 跨租户同名 doc_id：不能覆盖对方文档（doc_id 全局唯一，先到先得）
func TestIngestRejectsDocIDOwnedByAnotherTenant(t *testing.T) {
	r, _, _, _, _ := newTestRag(t)
	ctxA := tenantCtx("alice", "t1")
	ctxB := tenantCtx("bob", "t2")

	if _, err := r.Ingest(ctxA, "same-doc", "a.txt", "rag", strings.Repeat("A 的内容。", 20)); err != nil {
		t.Fatalf("租户 A 入库: %v", err)
	}
	_, err := r.Ingest(ctxB, "same-doc", "a.txt", "rag", strings.Repeat("B 的内容。", 20))
	if err == nil {
		t.Fatal("跨租户同名 doc_id 应被拒绝覆盖")
	}
	if !strings.Contains(err.Error(), "已被其他作用域或用户占用") {
		t.Fatalf("错误信息应说明占用，实际 %v", err)
	}
}

// 历史数据（tenant 为空）仍归默认租户，不会被升级后判成「不属于任何人」
func TestLegacyDocumentVisibleToDefaultTenant(t *testing.T) {
	r, _, repo, _, _ := newTestRag(t)
	// 模拟加列前的老行：只有 user_id，没有 tenant_id
	if err := repo.Upsert(context.Background(), &Document{
		DocID: "legacy", UserID: "alice", Source: "old.txt", Mode: "rag", Status: "ready",
	}); err != nil {
		t.Fatalf("写入历史文档: %v", err)
	}
	ctx := tenantCtx("alice", "public")
	docs, err := r.ListDocs(ctx, "", 20)
	if err != nil {
		t.Fatalf("ListDocs: %v", err)
	}
	found := false
	for _, d := range docs {
		if d.DocID == "legacy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("默认租户应能看到历史文档，实际 %+v", docs)
	}
}
