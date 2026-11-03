package rag

import (
	"context"
	"strings"
	"testing"

	"go_agent/internal/pkg/identity"
)

// ctxAs 构造指定用户身份的上下文（模拟网关注入）
func ctxAs(user string) context.Context {
	return identity.With(context.Background(), identity.Info{UserID: user, TenantID: "t1", Auth: "jwt"})
}

// TestUserDocFilterWhitelist 白名单表达式只包含该用户的文档；无文档时返回 empty
func TestUserDocFilterWhitelist(t *testing.T) {
	r, _, repo, _, _ := newTestRag(t)
	repo.docs["a-doc"] = &Document{DocID: "a-doc", UserID: "alice"}
	repo.docs["b-doc"] = &Document{DocID: "b-doc", UserID: "bob"}

	expr, empty, err := r.userDocFilter(context.Background(), "alice")
	if err != nil || empty {
		t.Fatalf("alice 应有文档: err=%v empty=%v", err, empty)
	}
	if !strings.Contains(expr, `"a-doc"`) || strings.Contains(expr, `"b-doc"`) {
		t.Fatalf("白名单应只含本人文档，实际: %s", expr)
	}
	if !strings.HasPrefix(expr, chunkDocIDField+" in [") {
		t.Fatalf("表达式字段/语法不符: %s", expr)
	}

	_, empty, err = r.userDocFilter(context.Background(), "carol")
	if err != nil {
		t.Fatalf("空用户查询不应报错: %v", err)
	}
	if !empty {
		t.Fatalf("carol 没有文档，应返回 empty=true")
	}
}

// TestRetrieveForUserPassesWhitelist 检索把用户白名单作为 Milvus filter 下推
func TestRetrieveForUserPassesWhitelist(t *testing.T) {
	r, store, repo, _, _ := newTestRag(t)
	repo.docs["b-doc"] = &Document{DocID: "b-doc", UserID: "bob"}

	if _, err := r.RetrieveForUser(context.Background(), "harness 是什么", "bob"); err != nil {
		t.Fatalf("RetrieveForUser: %v", err)
	}
	if !strings.Contains(store.lastFilter, `"b-doc"`) {
		t.Fatalf("过滤表达式未下推，实际: %q", store.lastFilter)
	}

	// 没有文档的用户：直接返回空，且不应发起向量检索（filter 保持为空）
	store.lastFilter = ""
	hits, err := r.RetrieveForUser(context.Background(), "harness 是什么", "nobody")
	if err != nil || len(hits) != 0 {
		t.Fatalf("无文档用户应返回空结果: hits=%v err=%v", hits, err)
	}
	if store.lastFilter != "" {
		t.Fatalf("无文档用户不应发起检索，实际 filter=%q", store.lastFilter)
	}
}

// TestIngestRecordsOwnerAndRejectsForeignDocID 入库固化归属；同名文档已属他人时拒绝覆盖
func TestIngestRecordsOwnerAndRejectsForeignDocID(t *testing.T) {
	r, _, repo, _, _ := newTestRag(t)
	text := strings.Repeat("Harness 是测试与评测执行框架。", 6)

	if _, err := r.Ingest(ctxAs("alice"), "harness", "a.txt", "rag", text); err != nil {
		t.Fatalf("alice 入库失败: %v", err)
	}
	if got := repo.docs["harness"]; got == nil || got.UserID != "alice" {
		t.Fatalf("文档归属未写入 alice: %+v", got)
	}

	if _, err := r.Ingest(ctxAs("bob"), "harness", "a.txt", "rag", text); err == nil {
		t.Fatalf("bob 复用 alice 的 doc_id 应被拒绝")
	}
}
