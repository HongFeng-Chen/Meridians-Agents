// Package space 知识空间（V2 第五章）：文件夹式的组织与授权单位。
//
// 关键区分（V2 文档 5.3）：
//
//	Space          负责「组织 + 授权」——谁能看见、谁能在里面建东西
//	KnowledgeBase  负责「知识数据」——文档、分块、向量
//
// 一个 Space 可以包含多个知识库；一个知识库默认归属一个 Space，
// 通过显式共享给其他 Space / Principal，而不是复制整套知识库。
//
// 空间树用「邻接表 + 物化路径」双写：
//   - parent_space_id 是权威关系（移动、改名只看它）；
//   - path / depth 是查询加速（子树前缀查询、深度限制），移动时整棵子树一起改。
//
// 继承规则（本节是判权核心，改动前先读）：
//
//	inherit    继承父空间可继承的授权（默认）
//	restricted 只认显式授权，不继承父空间
//	isolated   完全独立（个人空间默认）
//
// 特别注意：父空间成员身份 ≠ 子空间资源权限。研发部成员能看研发部共享知识，
// 但不能自动看研发部成员的个人空间——个人空间是 isolated。
package space

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go_agent/internal/pkg/authz"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/principal"
)

// 空间类型
const (
	TypePlatform   = "platform"
	TypeTenant     = "tenant"
	TypeDepartment = "department"
	TypePersonal   = "personal"
	TypeCustom     = "custom"
)

// 继承模式
const (
	InheritInherit    = "inherit"
	InheritRestricted = "restricted"
	InheritIsolated   = "isolated"
)

// 空间状态
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

// 保留空间 ID（迁移与兼容用，创建时不允许覆盖）
const (
	PlatformSpaceID  = "platform"
	tenantPrefix     = "tenant:"
	departmentPrefix = "dept:"
	personalPrefix   = "personal:"
	// MaxDepth 最大层级：无限层级会让「移动子树」和权限继承都变得不可控
	MaxDepth = 8
)

// 领域错误
var (
	ErrNotFound  = errors.New("空间不存在")
	ErrForbidden = errors.New("无权操作该空间")
	ErrInvalid   = errors.New("空间参数不合法")
	ErrConflict  = errors.New("空间已存在")
	ErrDepth     = errors.New("空间层级超过上限")
	ErrCycle     = errors.New("不能把空间移动到自己的子树下")
)

var (
	validTypes     = map[string]bool{TypePlatform: true, TypeTenant: true, TypeDepartment: true, TypePersonal: true, TypeCustom: true}
	validInherit   = map[string]bool{InheritInherit: true, InheritRestricted: true, InheritIsolated: true}
	spaceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:_\-]{1,63}$`)
)

// Space 知识空间节点。
type Space struct {
	SpaceID       string `json:"space_id"`
	ParentSpaceID string `json:"parent_space_id"`
	TenantID      string `json:"tenant_id"`
	OwnerID       string `json:"owner_id"`

	Name        string `json:"name"`
	Description string `json:"description"`
	SpaceType   string `json:"space_type"`

	InheritanceMode string `json:"inheritance_mode"`
	Path            string `json:"path"`
	Depth           int    `json:"depth"`

	Status string `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Share 显式共享：把空间按动作授给 user / department / tenant / role / service。
type Share struct {
	ID            uint64    `json:"id"`
	SpaceID       string    `json:"space_id"`
	PrincipalType string    `json:"principal_type"`
	PrincipalID   string    `json:"principal_id"`
	Actions       []string  `json:"actions"`
	CreatedAt     time.Time `json:"created_at"`
}

// 共享主体类型（与 resource 包保持一致）
const (
	PrincipalUser       = "user"
	PrincipalDepartment = "department"
	PrincipalTenant     = "tenant"
	PrincipalRole       = "role"
	PrincipalService    = "service"
)

var validPrincipalTypes = map[string]bool{
	PrincipalUser: true, PrincipalDepartment: true, PrincipalTenant: true,
	PrincipalRole: true, PrincipalService: true,
}

// Repo 空间仓储。
type Repo interface {
	Create(ctx context.Context, s *Space) error
	Get(ctx context.Context, spaceID string) (*Space, error)
	Update(ctx context.Context, s *Space) error
	Delete(ctx context.Context, spaceID string) error

	// ListByTenant 列出某租户全部空间（含 platform 空间由调用方另行合并）
	ListByTenant(ctx context.Context, tenantID string) ([]Space, error)
	// ListChildren 直接子节点
	ListChildren(ctx context.Context, parentSpaceID string) ([]Space, error)
	// ListDescendants 按 path 前缀取子树（不含自身）
	ListDescendants(ctx context.Context, spaceID string) ([]Space, error)
	// UpdatePathPrefix 移动子树：oldPrefix → newPrefix，depth 同步平移
	UpdatePathPrefix(ctx context.Context, oldPrefix, newPrefix string, delta int) error

	AddShare(ctx context.Context, sh *Share) error
	DeleteShare(ctx context.Context, id uint64) error
	GetShare(ctx context.Context, id uint64) (*Share, error)
	ListShares(ctx context.Context, spaceID string) ([]Share, error)
	// ListSharesForTenant 取本租户内与这些主体相关的共享（含 tenant / role）
	ListSharesForTenant(ctx context.Context, tenantID string, refs []PrincipalRef) ([]Share, error)
}

// PrincipalRef 共享查询的匹配条件
type PrincipalRef struct {
	Type string
	ID   string
}

// UseCase 空间管理。
type UseCase struct {
	repo Repo
	now  func() time.Time
}

// NewUseCase 构建用例。
func NewUseCase(repo Repo) *UseCase {
	return &UseCase{repo: repo, now: time.Now}
}

// CreateInput 创建空间入参
type CreateInput struct {
	SpaceID         string `json:"space_id"`
	ParentSpaceID   string `json:"parent_space_id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	SpaceType       string `json:"space_type"`
	InheritanceMode string `json:"inheritance_mode"`
	// TenantID / OwnerID 留空时由调用方身份推导（平台空间除外）。
	// 只允许平台管理员显式指定，普通调用方填了也会被身份覆盖。
	TenantID string `json:"tenant_id"`
	OwnerID  string `json:"owner_id"`
}

// Create 创建空间。
func (u *UseCase) Create(ctx context.Context, p *principal.Principal, in CreateInput) (*Space, error) {
	if p == nil {
		return nil, ErrForbidden
	}
	in.SpaceType = strings.ToLower(strings.TrimSpace(in.SpaceType))
	if !validTypes[in.SpaceType] {
		return nil, fmt.Errorf("%w: 未知空间类型 %q", ErrInvalid, in.SpaceType)
	}
	in.InheritanceMode = strings.ToLower(strings.TrimSpace(in.InheritanceMode))
	if in.InheritanceMode == "" {
		in.InheritanceMode = defaultInherit(in.SpaceType)
	}
	if !validInherit[in.InheritanceMode] {
		return nil, fmt.Errorf("%w: 未知继承模式 %q", ErrInvalid, in.InheritanceMode)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: 空间名称不能为空", ErrInvalid)
	}
	if err := u.authorizeCreate(ctx, p, in.SpaceType); err != nil {
		return nil, err
	}

	tenantID := strings.TrimSpace(in.TenantID)
	if in.SpaceType == TypePlatform {
		tenantID = "" // 平台空间不归属任何租户
	} else if tenantID == "" {
		tenantID = p.TenantID
	}
	if in.SpaceType != TypePlatform && tenantID != p.TenantID && !p.Admin {
		return nil, fmt.Errorf("%w: 不能在其他租户创建空间", ErrForbidden)
	}

	parentID := strings.TrimSpace(in.ParentSpaceID)
	depth := 0
	path := ""
	if parentID != "" {
		parent, err := u.repo.Get(ctx, parentID)
		if err != nil {
			return nil, err
		}
		if parent.SpaceType == TypePlatform && in.SpaceType != TypePlatform && in.SpaceType != TypeCustom {
			return nil, fmt.Errorf("%w: 平台空间下只能建自定义子空间", ErrForbidden)
		}
		depth = parent.Depth + 1
		path = joinPath(parent.Path, parent.SpaceID)
	}
	if depth >= MaxDepth {
		return nil, fmt.Errorf("%w: 最大 %d 层", ErrDepth, MaxDepth)
	}

	spaceID := strings.TrimSpace(in.SpaceID)
	if spaceID == "" {
		spaceID = defaultSpaceID(in.SpaceType, tenantID, in.OwnerID, p.SubjectID)
	}
	spaceID = strings.ToLower(spaceID)
	if !spaceIDPattern.MatchString(spaceID) {
		return nil, fmt.Errorf("%w: space_id 只能是小写字母数字与 :_-，长度 2~64", ErrInvalid)
	}
	if spaceID == PlatformSpaceID && in.SpaceType != TypePlatform {
		return nil, fmt.Errorf("%w: %q 是平台保留空间", ErrConflict, PlatformSpaceID)
	}

	owner := firstNonEmpty(in.OwnerID, p.SubjectID)
	if in.SpaceType == TypePersonal && owner != p.SubjectID && !p.Admin {
		return nil, fmt.Errorf("%w: 只能创建自己的个人空间", ErrForbidden)
	}
	if in.SpaceType == TypePlatform {
		owner = ""
	}

	now := u.now()
	s := &Space{
		SpaceID:         spaceID,
		ParentSpaceID:   parentID,
		TenantID:        tenantID,
		OwnerID:         owner,
		Name:            strings.TrimSpace(in.Name),
		Description:     strings.TrimSpace(in.Description),
		SpaceType:       in.SpaceType,
		InheritanceMode: in.InheritanceMode,
		Path:            path,
		Depth:           depth,
		Status:          StatusActive,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := u.repo.Create(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Get 读取空间（需要能看到它）。
func (u *UseCase) Get(ctx context.Context, p *principal.Principal, spaceID string) (*Space, error) {
	s, err := u.repo.Get(ctx, strings.TrimSpace(spaceID))
	if err != nil {
		return nil, err
	}
	ok, err := u.CanAccess(ctx, p, s.SpaceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 与不存在同口径，避免枚举空间 ID
		return nil, ErrNotFound
	}
	return s, nil
}

// List 列出当前身份可见的空间（含平台空间）。
func (u *UseCase) List(ctx context.Context, p *principal.Principal) ([]Space, error) {
	if p == nil {
		return nil, ErrForbidden
	}
	allowed, err := u.AllowedSpaceIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		set[id] = true
	}
	rows, err := u.repo.ListByTenant(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]Space, 0, len(rows))
	for _, s := range rows {
		if set[s.SpaceID] {
			out = append(out, s)
		}
	}
	// 平台空间（tenant_id 为空）单独合并
	if platforms, err := u.repo.ListByTenant(ctx, ""); err == nil {
		for _, s := range platforms {
			if set[s.SpaceID] {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// Children 直接子节点（需要能看到父空间）。
func (u *UseCase) Children(ctx context.Context, p *principal.Principal, parentID string) ([]Space, error) {
	ok, err := u.CanAccess(ctx, p, parentID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	rows, err := u.repo.ListChildren(ctx, parentID)
	if err != nil {
		return nil, err
	}
	out := make([]Space, 0, len(rows))
	for _, s := range rows {
		if allowed, _ := u.CanAccess(ctx, p, s.SpaceID); allowed {
			out = append(out, s)
		}
	}
	return out, nil
}

// UpdateInput 更新空间入参（不含 tenant/owner，换归属请新建）
type UpdateInput struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	InheritanceMode string `json:"inheritance_mode"`
	// ParentSpaceID 非空表示移动（nil 表示不移动；根空间不能移动）
	ParentSpaceID *string `json:"parent_space_id"`
}

// Update 更新空间元数据 / 移动子树。
func (u *UseCase) Update(ctx context.Context, p *principal.Principal, spaceID string, in UpdateInput) (*Space, error) {
	s, err := u.repo.Get(ctx, strings.TrimSpace(spaceID))
	if err != nil {
		return nil, err
	}
	if err := u.authorizeManage(ctx, p, s); err != nil {
		return nil, err
	}
	if in.Name != "" {
		s.Name = strings.TrimSpace(in.Name)
	}
	if in.Description != "" {
		s.Description = strings.TrimSpace(in.Description)
	}
	if in.InheritanceMode != "" {
		mode := strings.ToLower(strings.TrimSpace(in.InheritanceMode))
		if !validInherit[mode] {
			return nil, fmt.Errorf("%w: 未知继承模式 %q", ErrInvalid, mode)
		}
		s.InheritanceMode = mode
	}
	if in.ParentSpaceID != nil {
		if err := u.move(ctx, s, strings.TrimSpace(*in.ParentSpaceID)); err != nil {
			return nil, err
		}
	}
	s.UpdatedAt = u.now()
	if err := u.repo.Update(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// move 移动子树：校验环、深度，并整棵子树改 path。
func (u *UseCase) move(ctx context.Context, s *Space, newParentID string) error {
	if newParentID == s.SpaceID {
		return ErrCycle
	}
	oldPath, oldDepth := s.Path, s.Depth
	if newParentID == "" {
		s.ParentSpaceID, s.Path, s.Depth = "", "", 0
	} else {
		parent, err := u.repo.Get(ctx, newParentID)
		if err != nil {
			return err
		}
		// 目标在自身子树内 → 成环
		selfFull := joinPath(s.Path, s.SpaceID)
		parentFull := joinPath(parent.Path, parent.SpaceID)
		if parent.SpaceID == s.SpaceID || parentFull == selfFull || strings.HasPrefix(parentFull, selfFull+"/") {
			return ErrCycle
		}
		if parent.TenantID != s.TenantID && s.SpaceType != TypePlatform {
			return fmt.Errorf("%w: 不能跨租户移动空间", ErrForbidden)
		}
		if parent.Depth+1 >= MaxDepth {
			return fmt.Errorf("%w: 最大 %d 层", ErrDepth, MaxDepth)
		}
		s.ParentSpaceID = parent.SpaceID
		s.Path = parentFull
		s.Depth = parent.Depth + 1
	}
	delta := s.Depth - oldDepth
	oldPrefix := joinPath(oldPath, s.SpaceID)
	newPrefix := joinPath(s.Path, s.SpaceID)
	if oldPrefix != newPrefix {
		if err := u.repo.UpdatePathPrefix(ctx, oldPrefix, newPrefix, delta); err != nil {
			return err
		}
	}
	return nil
}

// Delete 删除空间：有子空间时拒绝，避免把子树变成孤儿。
func (u *UseCase) Delete(ctx context.Context, p *principal.Principal, spaceID string) error {
	s, err := u.repo.Get(ctx, strings.TrimSpace(spaceID))
	if err != nil {
		return err
	}
	if s.SpaceType == TypePlatform && s.SpaceID == PlatformSpaceID {
		return fmt.Errorf("%w: 平台根空间不可删除", ErrForbidden)
	}
	if err := u.authorizeManage(ctx, p, s); err != nil {
		return err
	}
	children, err := u.repo.ListChildren(ctx, s.SpaceID)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return fmt.Errorf("%w: 请先删除或移动子空间（剩 %d 个）", ErrConflict, len(children))
	}
	return u.repo.Delete(ctx, s.SpaceID)
}

// Grant 显式共享空间。
func (u *UseCase) Grant(ctx context.Context, p *principal.Principal, spaceID, principalType, principalID string, actions []string) (*Share, error) {
	s, err := u.repo.Get(ctx, strings.TrimSpace(spaceID))
	if err != nil {
		return nil, err
	}
	if err := u.authorizeManage(ctx, p, s); err != nil {
		return nil, err
	}
	pt := strings.ToLower(strings.TrimSpace(principalType))
	if !validPrincipalTypes[pt] {
		return nil, fmt.Errorf("%w: 未知主体类型 %q", ErrInvalid, pt)
	}
	pid := strings.TrimSpace(principalID)
	if pid == "" {
		return nil, fmt.Errorf("%w: 主体 ID 不能为空", ErrInvalid)
	}
	// 跨租户共享：只允许平台空间显式授给其他租户，且必须平台管理员操作
	if pt == PrincipalTenant && pid != s.TenantID && !p.Admin {
		return nil, fmt.Errorf("%w: 不能跨租户共享", ErrForbidden)
	}
	acts := normalizeActions(actions)
	if len(acts) == 0 {
		return nil, fmt.Errorf("%w: 至少指定一个动作", ErrInvalid)
	}
	sh := &Share{
		SpaceID:       s.SpaceID,
		PrincipalType: pt,
		PrincipalID:   pid,
		Actions:       acts,
		CreatedAt:     u.now(),
	}
	if err := u.repo.AddShare(ctx, sh); err != nil {
		return nil, err
	}
	return sh, nil
}

// Revoke 撤销共享。
func (u *UseCase) Revoke(ctx context.Context, p *principal.Principal, shareID uint64) error {
	// 仓储按 id 删除，权限校验依赖所属空间：先取共享再判空间管理权
	sh, err := u.repo.GetShare(ctx, shareID)
	if err != nil {
		return err
	}
	sp, err := u.repo.Get(ctx, sh.SpaceID)
	if err != nil {
		return err
	}
	if err := u.authorizeManage(ctx, p, sp); err != nil {
		return err
	}
	return u.repo.DeleteShare(ctx, shareID)
}

// Shares 列出空间的显式共享。
func (u *UseCase) Shares(ctx context.Context, p *principal.Principal, spaceID string) ([]Share, error) {
	s, err := u.repo.Get(ctx, strings.TrimSpace(spaceID))
	if err != nil {
		return nil, err
	}
	if err := u.authorizeManage(ctx, p, s); err != nil {
		return nil, err
	}
	return u.repo.ListShares(ctx, s.SpaceID)
}

// CanAccess 当前身份能否访问某个空间（读语义）。
func (u *UseCase) CanAccess(ctx context.Context, p *principal.Principal, spaceID string) (bool, error) {
	ids, err := u.AllowedSpaceIDs(ctx, p)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == spaceID {
			return true, nil
		}
	}
	return false, nil
}

// AllowedSpaceIDs 计算当前身份可见的空间集合。
//
// 组成（并集，去重）：
//
//  1. 平台公共空间（platform 及 inherit 子空间）——所有租户可见
//  2. 本租户根空间 + 其下 inherit 链上的子空间
//  3. 本人所属部门的部门空间 + inherit 子空间
//  4. 本人的个人空间
//  5. 显式共享给本人 / 部门 / 租户 / 角色 / 服务的空间 + inherit 子空间
//
// 注意 restricted / isolated 的子空间不会被父空间的授权带进来。
func (u *UseCase) AllowedSpaceIDs(ctx context.Context, p *principal.Principal) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	all, err := u.repo.ListByTenant(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	if platforms, err := u.repo.ListByTenant(ctx, ""); err == nil {
		all = append(all, platforms...)
	}
	byID := make(map[string]Space, len(all))
	children := make(map[string][]Space, len(all))
	for _, s := range all {
		byID[s.SpaceID] = s
		if s.ParentSpaceID != "" {
			children[s.ParentSpaceID] = append(children[s.ParentSpaceID], s)
		}
	}

	seed := map[string]bool{}
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := byID[id]; ok {
			seed[id] = true
		}
	}

	// 个人空间是「隐式单位」：isolated、随用随建，不要求 knowledge_spaces 里
	// 真有一行（EnsureDefaults 只是迁移期的落库动作）。这里必须无条件把本人
	// 个人空间算进可见集合——否则「入库先写了 space_id、个人空间行还没建」时，
	// 该文档会从检索白名单里凭空消失（实时库双索引冒烟实测踩过）。
	// 它不出现在 byID 里也不会被误展开：walk 只沿 children 走，没有子节点。
	if p != nil && p.SubjectID != "" {
		seed[PersonalSpaceID(p.TenantID, p.SubjectID)] = true
	}

	for _, s := range all {
		switch s.SpaceType {
		case TypePlatform:
			// 平台空间对所有租户可见（平台公共知识）
			add(s.SpaceID)
		case TypeTenant:
			if s.TenantID == p.TenantID {
				add(s.SpaceID)
			}
		case TypeDepartment:
			if s.TenantID == p.TenantID && (s.OwnerID == "" || p.InDepartment(s.OwnerID) || p.Admin) {
				add(s.SpaceID)
			}
		case TypePersonal:
			if s.TenantID == p.TenantID && s.OwnerID == p.SubjectID {
				add(s.SpaceID)
			}
		}
		// 归属人始终能看到自己创建的空间
		if s.OwnerID != "" && s.OwnerID == p.SubjectID {
			add(s.SpaceID)
		}
	}

	refs := []PrincipalRef{
		{Type: PrincipalUser, ID: p.SubjectID},
		{Type: PrincipalTenant, ID: p.TenantID},
	}
	for _, d := range p.DepartmentIDs {
		refs = append(refs, PrincipalRef{Type: PrincipalDepartment, ID: d})
	}
	for _, r := range p.Roles {
		refs = append(refs, PrincipalRef{Type: PrincipalRole, ID: r})
	}
	if shares, err := u.repo.ListSharesForTenant(ctx, p.TenantID, refs); err == nil {
		for _, sh := range shares {
			add(sh.SpaceID)
		}
	}

	// 向下展开 inherit 链：restricted / isolated 不再向下传递
	out := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if out[id] {
			return
		}
		out[id] = true
		for _, child := range children[id] {
			if child.InheritanceMode == InheritInherit {
				walk(child.SpaceID)
			}
		}
	}
	for id := range seed {
		walk(id)
	}
	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	return ids, nil
}

// EnsureDefaults 幂等创建平台 / 租户 / 个人三级默认空间。
//
// 迁移期用：老的 system / tenant / user 三层知识需要落进对应空间，
// 否则升级后检索白名单算不出来。已存在时直接返回现有空间。
func (u *UseCase) EnsureDefaults(ctx context.Context, p *principal.Principal) (platform, tenant, personal *Space, err error) {
	platform, err = u.ensureOne(ctx, PlatformSpaceID, TypePlatform, "", "", "平台公共知识", InheritInherit)
	if err != nil {
		return
	}
	if p == nil {
		return platform, nil, nil, nil
	}
	tenant, err = u.ensureOne(ctx, TenantSpaceID(p.TenantID), TypeTenant, p.TenantID, "", "租户公共知识", InheritRestricted)
	if err != nil {
		return platform, nil, nil, err
	}
	personal, err = u.ensureOne(ctx, PersonalSpaceID(p.TenantID, p.SubjectID), TypePersonal,
		p.TenantID, p.SubjectID, "个人知识", InheritIsolated)
	return platform, tenant, personal, err
}

func (u *UseCase) ensureOne(ctx context.Context, id, typ, tenantID, ownerID, name, inherit string) (*Space, error) {
	if existing, err := u.repo.Get(ctx, id); err == nil {
		return existing, nil
	}
	now := u.now()
	s := &Space{
		SpaceID: id, TenantID: tenantID, OwnerID: ownerID, Name: name,
		SpaceType: typ, InheritanceMode: inherit, Depth: 0,
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if typ != TypePlatform {
		s.Path = ""
	}
	if err := u.repo.Create(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// TenantSpaceID 租户根空间 ID
func TenantSpaceID(tenantID string) string {
	return tenantPrefix + strings.TrimSpace(tenantID)
}

// PersonalSpaceID 个人空间 ID（租户内唯一：不同租户的同名用户不共享空间）
func PersonalSpaceID(tenantID, subjectID string) string {
	return personalPrefix + strings.TrimSpace(tenantID) + ":" + strings.TrimSpace(subjectID)
}

// DepartmentSpaceID 部门空间 ID
func DepartmentSpaceID(tenantID, departmentID string) string {
	return departmentPrefix + strings.TrimSpace(tenantID) + ":" + strings.TrimSpace(departmentID)
}

// ScopeToSpaceID 把 V1 三层作用域映射到默认空间（迁移兼容）。
func ScopeToSpaceID(scope, tenantID, userID string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "system":
		return PlatformSpaceID
	case "tenant":
		return TenantSpaceID(tenantID)
	default:
		return PersonalSpaceID(tenantID, userID)
	}
}

func (u *UseCase) authorizeCreate(ctx context.Context, p *principal.Principal, typ string) error {
	switch typ {
	case TypePlatform:
		if !authz.Allowed(ctx, authz.PermPlatformConfig) && !p.Admin {
			return fmt.Errorf("%w: 创建平台空间需要平台管理员", ErrForbidden)
		}
	case TypeTenant, TypeDepartment, TypeCustom:
		if !authz.Allowed(ctx, authz.PermTenantManage) && !p.Admin {
			return fmt.Errorf("%w: 创建租户/部门空间需要租户管理员", ErrForbidden)
		}
	case TypePersonal:
		// 个人空间只能给自己建（owner 校验在 Create 里）
	}
	return nil
}

func (u *UseCase) authorizeManage(ctx context.Context, p *principal.Principal, s *Space) error {
	if p == nil || s == nil {
		return ErrForbidden
	}
	if p.Admin || (s.OwnerID != "" && s.OwnerID == p.SubjectID) {
		return nil
	}
	switch s.SpaceType {
	case TypePlatform:
		if authz.Allowed(ctx, authz.PermPlatformConfig) {
			return nil
		}
	case TypeTenant, TypeDepartment, TypeCustom:
		if s.TenantID == p.TenantID && authz.Allowed(ctx, authz.PermTenantManage) {
			return nil
		}
	}
	// 显式 shared 且带 manage 动作
	refs := []PrincipalRef{{Type: PrincipalUser, ID: p.SubjectID}, {Type: PrincipalTenant, ID: p.TenantID}}
	for _, d := range p.DepartmentIDs {
		refs = append(refs, PrincipalRef{Type: PrincipalDepartment, ID: d})
	}
	shares, err := u.repo.ListSharesForTenant(ctx, p.TenantID, refs)
	if err == nil {
		for _, sh := range shares {
			if sh.SpaceID == s.SpaceID && hasAction(sh.Actions, "manage_permission") {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: 需要空间管理权限", ErrForbidden)
}

func defaultInherit(typ string) string {
	switch typ {
	case TypePlatform:
		return InheritInherit
	case TypePersonal:
		return InheritIsolated
	case TypeTenant:
		return InheritRestricted
	default:
		return InheritInherit
	}
}

func defaultSpaceID(typ, tenantID, ownerID, subjectID string) string {
	switch typ {
	case TypePlatform:
		return PlatformSpaceID
	case TypeTenant:
		return TenantSpaceID(tenantID)
	case TypePersonal:
		return PersonalSpaceID(tenantID, firstNonEmpty(ownerID, subjectID))
	case TypeDepartment:
		return DepartmentSpaceID(tenantID, firstNonEmpty(ownerID, subjectID))
	default:
		return strings.ToLower(strings.ReplaceAll(firstNonEmpty(tenantID, "space")+"-"+subjectID, " ", "-"))
	}
}

func normalizeActions(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

func hasAction(actions []string, want string) bool {
	for _, a := range actions {
		if strings.EqualFold(strings.TrimSpace(a), want) {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// joinPath 拼接物化路径，避免根节点（path 为空）拼出以 "/" 开头的路径。
// 路径里的每一段都是 space_id，不含空串，因此不需要处理重复分隔符。
func joinPath(parentPath, parentID string) string {
	parentPath = strings.Trim(parentPath, "/")
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return parentPath
	}
	if parentPath == "" {
		return parentID
	}
	return parentPath + "/" + parentID
}

// DevPrincipal 开发态身份（无认证时用于确保默认空间），仅测试与迁移脚本使用。
func DevPrincipal(ctx context.Context) *principal.Principal {
	return principal.FromIdentity(identity.From(ctx))
}
