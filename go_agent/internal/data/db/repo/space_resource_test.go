package repo

import (
	"context"
	"testing"
	"time"

	"go_agent/internal/biz/rag"
	"go_agent/internal/biz/resource"
	"go_agent/internal/biz/space"
	"go_agent/internal/data/db/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// newTestDB 用内存 SQLite 验证仓储 SQL（空间树、资源授权、文档空间过滤）。
// 不复用 db.NewData：那会连真实 MySQL，单测必须离线可跑。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(
		&models.KnowledgeSpacePO{}, &models.KnowledgeSpaceSharePO{},
		&models.ResourceMetadataPO{}, &models.ResourcePermissionPO{}, &models.ResourceVersionPO{},
		&models.DocumentPO{},
	); err != nil {
		t.Fatal(err)
	}
	return gdb
}

func TestSpaceRepoTree(t *testing.T) {
	gdb := newTestDB(t)
	r := &SpaceRepo{db: gdb}
	ctx := context.Background()
	now := time.Now()

	root := &space.Space{SpaceID: "tenant:acme", TenantID: "acme", Name: "acme", SpaceType: space.TypeTenant, InheritanceMode: space.InheritRestricted, Status: space.StatusActive, CreatedAt: now, UpdatedAt: now}
	if err := r.Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	child := &space.Space{SpaceID: "dept:acme:rd", ParentSpaceID: root.SpaceID, TenantID: "acme", Name: "研发", SpaceType: space.TypeDepartment, InheritanceMode: space.InheritInherit, Path: root.SpaceID, Depth: 1, Status: space.StatusActive, CreatedAt: now, UpdatedAt: now}
	if err := r.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	grand := &space.Space{SpaceID: "team:acme:rd:core", ParentSpaceID: child.SpaceID, TenantID: "acme", Name: "核心", SpaceType: space.TypeCustom, InheritanceMode: space.InheritInherit, Path: root.SpaceID + "/" + child.SpaceID, Depth: 2, Status: space.StatusActive, CreatedAt: now, UpdatedAt: now}
	if err := r.Create(ctx, grand); err != nil {
		t.Fatal(err)
	}

	desc, err := r.ListDescendants(ctx, root.SpaceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc) != 2 {
		t.Fatalf("应有两个后代，实际 %d", len(desc))
	}

	// 移动子树：把 dept 挂到自定义空间下会成环（业务层拦），这里验证路径前缀更新
	if err := r.UpdatePathPrefix(ctx, "tenant:acme", "tenant:acme", 0); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(ctx, grand.SpaceID)
	if got.Depth != 2 || got.Path != "tenant:acme/dept:acme:rd" {
		t.Fatalf("路径/深度不符: %+v", got)
	}

	// 重复创建 → 领域冲突
	if err := r.Create(ctx, root); err == nil {
		t.Fatal("重复 space_id 应报冲突")
	}
}

func TestResourceRepoGrantsAndVersions(t *testing.T) {
	gdb := newTestDB(t)
	r := &ResourceRepo{db: gdb}
	ctx := context.Background()
	now := time.Now()

	res := &resource.Resource{
		ResourceID: "agent:test", ResourceType: resource.TypeAgent, TenantID: "acme",
		SpaceID: "tenant:acme", OwnerID: "alice", Name: "测试", Visibility: resource.VisibilityPrivate,
		Version: 1, Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := r.Create(ctx, res); err != nil {
		t.Fatal(err)
	}
	if err := r.AddVersion(ctx, &resource.Version{ResourceID: res.ResourceID, Version: 1, Snapshot: "{}", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// 同版本再写 → 冲突（历史不可覆盖）
	if err := r.AddVersion(ctx, &resource.Version{ResourceID: res.ResourceID, Version: 1, Snapshot: "{}", CreatedAt: now}); err == nil {
		t.Fatal("重复版本应报冲突")
	}

	grant := &resource.Grant{ResourceID: res.ResourceID, PrincipalType: resource.PrincipalUser, PrincipalID: "bob", Action: resource.ActionRead, Effect: resource.EffectAllow, CreatedAt: now}
	if err := r.AddGrant(ctx, grant); err != nil {
		t.Fatal(err)
	}
	// 幂等：再授权同动作改为 deny
	if err := r.AddGrant(ctx, &resource.Grant{ResourceID: res.ResourceID, PrincipalType: resource.PrincipalUser, PrincipalID: "bob", Action: resource.ActionRead, Effect: resource.EffectDeny, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	granted, err := r.Grants(ctx, res.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 || granted[0].Effect != resource.EffectDeny {
		t.Fatalf("授权应被更新为 deny: %+v", granted)
	}
	if err := r.DeleteGrant(ctx, res.ResourceID, resource.PrincipalUser, "bob", resource.ActionRead); err != nil {
		t.Fatal(err)
	}
	granted, _ = r.Grants(ctx, res.ResourceID)
	if len(granted) != 0 {
		t.Fatal("撤销后不应有授权")
	}
}

func TestDocumentRepoSpaceFilter(t *testing.T) {
	gdb := newTestDB(t)
	r := &DocumentRepo{db: gdb}
	ctx := context.Background()
	now := time.Now()

	docs := []models.DocumentPO{
		{DocID: "d-platform", TenantID: "acme", UserID: "alice", Scope: rag.ScopeSystem, SpaceID: "platform", Status: "ready", CreatedAt: now, UpdatedAt: now},
		{DocID: "d-tenant", TenantID: "acme", UserID: "alice", Scope: rag.ScopeTenant, SpaceID: "tenant:acme", Status: "ready", CreatedAt: now, UpdatedAt: now},
		{DocID: "d-alice", TenantID: "acme", UserID: "alice", Scope: rag.ScopeUser, SpaceID: "personal:acme:alice", Status: "ready", CreatedAt: now, UpdatedAt: now},
		{DocID: "d-legacy", TenantID: "acme", UserID: "bob", Scope: rag.ScopeUser, SpaceID: "", Status: "ready", CreatedAt: now, UpdatedAt: now},
		{DocID: "d-other", TenantID: "other", UserID: "eve", Scope: rag.ScopeUser, SpaceID: "personal:other:eve", Status: "ready", CreatedAt: now, UpdatedAt: now},
	}
	if err := gdb.Create(&docs).Error; err != nil {
		t.Fatal(err)
	}

	// alice 的空间：平台 + 租户 + 个人
	ids, err := r.ListDocIDsBySpaces(ctx, "acme", "alice", []string{"platform", "tenant:acme", "personal:acme:alice"})
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for _, want := range []string{"d-platform", "d-tenant", "d-alice"} {
		if !set[want] {
			t.Fatalf("应包含 %s: %v", want, ids)
		}
	}
	if set["d-legacy"] || set["d-other"] {
		t.Fatalf("不应包含他人/未迁移文档: %v", ids)
	}

	// 空空间集合 → 退回三层作用域（alice 能看到平台 + 租户 + 自己）
	legacy, err := r.ListDocIDsBySpaces(ctx, "acme", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 3 {
		t.Fatalf("回退作用域应有 3 篇（平台/租户/个人），实际 %v", legacy)
	}
}
