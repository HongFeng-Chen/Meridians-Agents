package rag

// 三层知识（system / tenant / user）的可见性与写权限。
//
// 这组用例守的是「共享」与「隔离」的边界：放宽可见性很容易，
// 收窄回去很难——一旦写错，历史数据就已经扩散出去了。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go_agent/internal/pkg/identity"
)

func scopeCtx(user, tenant string, roles ...string) context.Context {
	return identity.With(context.Background(), identity.Info{
		UserID: user, TenantID: tenant, Auth: "jwt", Roles: roles,
	})
}

// platformCtx 平台管理员（JWT 里带 admin 标记）
func platformCtx(user, tenant string) context.Context {
	return identity.With(context.Background(), identity.Info{
		UserID: user, TenantID: tenant, Auth: "jwt", Admin: true,
	})
}

func ingestAs(t *testing.T, r *Rag, ctx context.Context, docID, scope string) {
	t.Helper()
	if _, err := r.IngestDocument(ctx, IngestInput{
		DocID: docID, Source: docID + ".txt", Mode: "rag",
		Text:  strings.Repeat("知识库内容。", 20),
		Scope: scope,
	}); err != nil {
		t.Fatalf("入库 %s(%s): %v", docID, scope, err)
	}
}

func hasDoc(list []Document, docID string) bool {
	for _, d := range list {
		if d.DocID == docID {
			return true
		}
	}
	return false
}

// 默认作用域是 user：同租户的其他人看不到，更不用提别的租户
func TestDefaultScopeIsPrivate(t *testing.T) {
	r, _, repo, _, _ := newTestRag(t)
	alice := scopeCtx("alice", "t1")

	ingestAs(t, r, alice, "private-doc", "")

	if got := repo.docs["private-doc"].Scope; got != ScopeUser {
		t.Fatalf("未指定作用域应落 user，实际 %q", got)
	}
	list, err := r.ListDocs(alice, "", 20)
	if err != nil || !hasDoc(list, "private-doc") {
		t.Fatalf("本人应可见: err=%v list=%+v", err, list)
	}
	bobList, _ := r.ListDocs(scopeCtx("bob", "t1"), "", 20)
	if hasDoc(bobList, "private-doc") {
		t.Fatal("同租户他人不应看到个人私有知识")
	}
}

// 租户共享：同租户人人可见，跨租户不可见；只有租户管理员能写
func TestTenantScopeSharedInsideTenant(t *testing.T) {
	r, _, _, _, _ := newTestRag(t)
	admin := scopeCtx("alice", "t1", "tenant_admin")

	ingestAs(t, r, admin, "tenant-doc", ScopeTenant)

	if list, _ := r.ListDocs(scopeCtx("bob", "t1"), "", 20); !hasDoc(list, "tenant-doc") {
		t.Fatal("同租户成员应看到租户共享知识")
	}
	if list, _ := r.ListDocs(scopeCtx("carol", "t2"), "", 20); hasDoc(list, "tenant-doc") {
		t.Fatal("跨租户不应看到租户共享知识")
	}

	// 普通成员写租户作用域 → 拒绝
	_, err := r.IngestDocument(scopeCtx("bob", "t1"), IngestInput{
		DocID: "bob-tenant", Mode: "rag", Text: strings.Repeat("x", 30), Scope: ScopeTenant,
	})
	if !errors.Is(err, ErrScopeForbidden) {
		t.Fatalf("普通成员写 tenant 作用域应被拒绝，实际 %v", err)
	}
}

// 平台通用知识：跨租户可见；只有平台管理员能写能删
func TestSystemScopeVisibleEverywhere(t *testing.T) {
	r, _, _, _, _ := newTestRag(t)
	root := platformCtx("root", identity.DefaultTenant)

	ingestAs(t, r, root, "sys-doc", ScopeSystem)

	for _, ctx := range []context.Context{scopeCtx("alice", "t1"), scopeCtx("bob", "t2")} {
		if list, _ := r.ListDocs(ctx, "", 20); !hasDoc(list, "sys-doc") {
			t.Fatalf("用户 %s 应看到平台通用知识", identity.UserID(ctx))
		}
	}

	// 普通用户：既写不进去，也删不掉
	_, err := r.IngestDocument(scopeCtx("alice", "t1"), IngestInput{
		DocID: "fake-sys", Mode: "rag", Text: strings.Repeat("y", 30), Scope: ScopeSystem,
	})
	if !errors.Is(err, ErrScopeForbidden) {
		t.Fatalf("普通用户写 system 作用域应被拒绝，实际 %v", err)
	}
	if _, err := r.DeleteDoc(scopeCtx("alice", "t1"), "sys-doc"); !errors.Is(err, ErrScopeForbidden) {
		t.Fatalf("普通用户删 system 文档应被拒绝，实际 %v", err)
	}
	if _, err := r.DeleteDoc(root, "sys-doc"); err != nil {
		t.Fatalf("平台管理员应可删除 system 文档: %v", err)
	}
}

// 不能借「换个作用域重灌」把别人的私有文档转成共享知识
func TestIngestCannotHijackOthersDocument(t *testing.T) {
	r, _, _, _, _ := newTestRag(t)
	ingestAs(t, r, scopeCtx("alice", "t1"), "alice-doc", "")

	_, err := r.IngestDocument(scopeCtx("bob", "t1", "tenant_admin"), IngestInput{
		DocID: "alice-doc", Mode: "rag", Text: strings.Repeat("覆盖内容。", 10), Scope: ScopeTenant,
	})
	if err == nil || !strings.Contains(err.Error(), "已被其他作用域或用户占用") {
		t.Fatalf("租户管理员也不该顶掉他人私有文档，实际 %v", err)
	}
}

// 作者本人可以把个人知识升为租户共享（scope 参与更新）
func TestAuthorCanPromoteOwnDocument(t *testing.T) {
	r, _, repo, _, _ := newTestRag(t)
	admin := scopeCtx("alice", "t1", "tenant_admin")
	ingestAs(t, r, admin, "mine", "")

	ingestAs(t, r, admin, "mine", ScopeTenant)
	if got := repo.docs["mine"].Scope; got != ScopeTenant {
		t.Fatalf("作者应能把个人知识升为租户共享，实际 scope=%q", got)
	}
}

// 列表按作用域筛选；检索白名单与列表口径一致（含三层可见文档）
func TestListAndWhitelistShareVisibility(t *testing.T) {
	r, _, _, _, _ := newTestRag(t)
	root := platformCtx("root", identity.DefaultTenant)
	aliceAdmin := scopeCtx("alice", "t1", "tenant_admin")

	ingestAs(t, r, root, "sys", ScopeSystem)
	ingestAs(t, r, aliceAdmin, "shared", ScopeTenant)
	ingestAs(t, r, aliceAdmin, "mine", "")

	alice := scopeCtx("alice", "t1")
	all, _ := r.ListDocs(alice, "", 20)
	if len(all) != 3 {
		t.Fatalf("alice 应看到 3 篇（系统 + 租户 + 本人），实际 %d", len(all))
	}
	if only, _ := r.ListDocs(alice, ScopeSystem, 20); len(only) != 1 || only[0].DocID != "sys" {
		t.Fatalf("按 system 过滤应只剩 1 篇，实际 %+v", only)
	}

	filter, empty, err := r.DocFilterForUser(alice, "alice")
	if err != nil || empty {
		t.Fatalf("白名单不应为空: err=%v empty=%v", err, empty)
	}
	for _, id := range []string{"sys", "shared", "mine"} {
		if !strings.Contains(filter, `"`+id+`"`) {
			t.Fatalf("白名单缺少 %s: %s", id, filter)
		}
	}

	// 另一个租户：只见系统知识
	filterB, _, _ := r.DocFilterForUser(scopeCtx("bob", "t2"), "bob")
	if strings.Contains(filterB, `"shared"`) || strings.Contains(filterB, `"mine"`) {
		t.Fatalf("跨租户白名单不应含 t1 的文档: %s", filterB)
	}
	if !strings.Contains(filterB, `"sys"`) {
		t.Fatalf("跨租户白名单应含系统知识: %s", filterB)
	}
}

// 未知作用域直接报错，不静默降级
func TestNormalizeScopeRejectsUnknown(t *testing.T) {
	if _, err := NormalizeScope("global"); err == nil {
		t.Fatal("未知作用域应报错")
	}
	if got, err := NormalizeScope("  TENANT "); err != nil || got != ScopeTenant {
		t.Fatalf("应大小写/空白不敏感地识别 tenant，实际 %q %v", got, err)
	}
}
