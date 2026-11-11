// Package resource 统一资源模型与资源级授权（V2 第四章）。
//
// 目标：Agent / Prompt / Skill / MCP Server / MCP Tool / Knowledge Base /
// Graph Space / Evaluation Dataset 共用一套「归属 + 可见性 + 显式授权」模型，
// 而不是每个模块各自实现一套 if tenant == ... 的判断。
//
// 判定顺序（顺序本身就是安全策略，改动前先读这里）：
//
//  1. 资源不存在            → 拒绝
//  2. 跨租户                 → 拒绝（平台公共资源只读例外）
//  3. 显式 Deny             → 拒绝（Deny 永远优先于 Allow）
//  4. 显式 Allow            → 允许
//  5. 归属人 / 空间继承      → 允许（空间由 SpaceAuthorizer 计算，遵循继承模式）
//  6. 平台公共资源 + read    → 允许
//  7. 其余                   → 拒绝（默认拒绝）
//
// 注意：第三方令牌里的 roles 不参与这里的判定。它们只说明「在对方系统里是谁」，
// 不代表「在本平台能操作哪些 AI 资源」——这是 V2 文档 3.3 明确要求的分离。
package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go_agent/internal/pkg/principal"
)

// 资源类型（与数据库 resource_metadata.resource_type 取值一致）
const (
	TypeAgent             = "agent"
	TypePrompt            = "prompt"
	TypeSkill             = "skill"
	TypeMCPServer         = "mcp_server"
	TypeMCPTool           = "mcp_tool"
	TypeKnowledgeBase     = "knowledge_base"
	TypeGraphSpace        = "graph_space"
	TypeEvaluationDataset = "evaluation_dataset"
)

// 资源操作（统一动词，所有类型共用）
const (
	ActionCreate           = "create"
	ActionRead             = "read"
	ActionUpdate           = "update"
	ActionDelete           = "delete"
	ActionShare            = "share"
	ActionBind             = "bind"
	ActionExecute          = "execute"
	ActionPublish          = "publish"
	ActionManagePermission = "manage_permission"
)

// 可见性（与空间类型是两个概念，见 V2 文档 4.3）
const (
	VisibilityPrivate = "private" // 仅归属人
	VisibilityTenant  = "tenant"  // 同租户
	VisibilityPublic  = "platform"
)

// 授权主体类型
const (
	PrincipalUser       = "user"
	PrincipalDepartment = "department"
	PrincipalTenant     = "tenant"
	PrincipalRole       = "role"
	PrincipalService    = "service"
)

// 授权效果
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"
)

// 领域错误
var (
	ErrNotFound  = errors.New("资源不存在")
	ErrForbidden = errors.New("无权操作该资源")
	ErrInvalid   = errors.New("资源参数不合法")
	ErrConflict  = errors.New("资源已存在")
)

var validTypes = map[string]bool{
	TypeAgent: true, TypePrompt: true, TypeSkill: true,
	TypeMCPServer: true, TypeMCPTool: true, TypeKnowledgeBase: true,
	TypeGraphSpace: true, TypeEvaluationDataset: true,
}

var validActions = map[string]bool{
	ActionCreate: true, ActionRead: true, ActionUpdate: true, ActionDelete: true,
	ActionShare: true, ActionBind: true, ActionExecute: true, ActionPublish: true,
	ActionManagePermission: true,
}

// Resource 统一资源元数据。
//
// 具体业务表（agent_definitions / prompt_templates / …）仍保存自己的配置，
// 这里只保存「归属、可见性、版本」这些跨类型一致的字段，避免把业务字段
// 塞进一张大宽表。resource_id 由业务表的自然键提供（如 agent_key）。
type Resource struct {
	ResourceID string `json:"resource_id"`
	TenantID   string `json:"tenant_id"`
	SpaceID    string `json:"space_id"`
	OwnerID    string `json:"owner_id"`

	ResourceType string `json:"resource_type"`

	Name        string `json:"name"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`

	Version int64  `json:"version"`
	Status  string `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Grant 一条显式授权（一个主体 + 一个动作 + allow/deny）。
//
// 拆成「每个动作一行」而不是 actions JSON 数组：判权时能直接命中索引，
// 撤销单个动作也不需要读改写整行。
type Grant struct {
	ID            uint64    `json:"id"`
	ResourceID    string    `json:"resource_id"`
	PrincipalType string    `json:"principal_type"`
	PrincipalID   string    `json:"principal_id"`
	Action        string    `json:"action"`
	Effect        string    `json:"effect"`
	CreatedAt     time.Time `json:"created_at"`
}

// Version 不可变版本快照（发布/更新时追加，不覆盖历史）。
type Version struct {
	ResourceID string    `json:"resource_id"`
	Version    int64     `json:"version"`
	Snapshot   string    `json:"snapshot"`
	Checksum   string    `json:"checksum"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// Filter 列表过滤条件
type Filter struct {
	TenantID     string
	SpaceID      string
	ResourceType string
	OwnerID      string
	Status       string
	Limit        int
}

// Repo 资源元数据仓储（MySQL 实现位于 data/db/repo）。
type Repo interface {
	Create(ctx context.Context, r *Resource) error
	Update(ctx context.Context, r *Resource) error
	Get(ctx context.Context, resourceID string) (*Resource, error)
	List(ctx context.Context, f Filter) ([]Resource, error)
	Delete(ctx context.Context, resourceID string) error

	AddVersion(ctx context.Context, v *Version) error
	ListVersions(ctx context.Context, resourceID string) ([]Version, error)

	Grants(ctx context.Context, resourceID string) ([]Grant, error)
	AddGrant(ctx context.Context, g *Grant) error
	DeleteGrant(ctx context.Context, resourceID, principalType, principalID, action string) error
}

// SpaceAuthorizer 知识空间解析（由 biz/space 实现；nil 表示未启用空间体系）。
//
// 之所以用窄接口而不是直接依赖 space 包：资源层只关心「这个人能看见哪些空间」，
// 不需要知道空间树怎么建、共享怎么授。
type SpaceAuthorizer interface {
	AllowedSpaceIDs(ctx context.Context, p *principal.Principal) ([]string, error)
}

// UseCase 资源注册与授权。
type UseCase struct {
	repo   Repo
	spaces SpaceAuthorizer
}

// NewUseCase 构建用例（spaces 可为 nil，此时只按租户/归属/显式授权判定）。
func NewUseCase(repo Repo, spaces SpaceAuthorizer) *UseCase {
	return &UseCase{repo: repo, spaces: spaces}
}

// Register 登记资源。已存在同 ID 资源时返回 ErrConflict——
// 更新必须走 Update，否则版本号会静默丢失。
func (u *UseCase) Register(ctx context.Context, p *principal.Principal, r *Resource) (*Resource, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: 缺少调用方身份", ErrForbidden)
	}
	if err := normalizeResource(r); err != nil {
		return nil, err
	}
	if r.TenantID == "" {
		r.TenantID = p.TenantID
	}
	if r.TenantID != p.TenantID {
		return nil, fmt.Errorf("%w: 不能在其他租户登记资源", ErrForbidden)
	}
	if r.OwnerID == "" {
		r.OwnerID = p.SubjectID
	}
	if r.OwnerID != p.SubjectID && !p.Admin {
		return nil, fmt.Errorf("%w: 只能以本人名义登记资源", ErrForbidden)
	}
	now := time.Now()
	r.Version = 1
	r.Status = firstNonEmpty(r.Status, "active")
	r.CreatedAt, r.UpdatedAt = now, now
	if err := u.repo.Create(ctx, r); err != nil {
		return nil, err
	}
	snap, _ := json.Marshal(r)
	if err := u.repo.AddVersion(ctx, &Version{
		ResourceID: r.ResourceID,
		Version:    r.Version,
		Snapshot:   string(snap),
		Checksum:   checksum(snap),
		CreatedBy:  p.SubjectID,
		CreatedAt:  now,
	}); err != nil {
		return nil, err
	}
	return r, nil
}

// Update 更新元数据并追加版本快照（版本号 +1，历史版本不可变）。
func (u *UseCase) Update(ctx context.Context, p *principal.Principal, r *Resource) (*Resource, error) {
	if p == nil {
		return nil, ErrForbidden
	}
	if err := normalizeResource(r); err != nil {
		return nil, err
	}
	cur, err := u.repo.Get(ctx, r.ResourceID)
	if err != nil {
		return nil, err
	}
	ok, err := u.Check(ctx, p, r.ResourceID, ActionUpdate)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: 需要 update 权限", ErrForbidden)
	}
	// 归属/租户不允许通过 Update 改动：换租户等于换一份资源，应新建
	r.TenantID = cur.TenantID
	r.OwnerID = cur.OwnerID
	r.Version = cur.Version + 1
	r.CreatedAt = cur.CreatedAt
	r.UpdatedAt = time.Now()
	if r.Status == "" {
		r.Status = cur.Status
	}
	if err := u.repo.Update(ctx, r); err != nil {
		return nil, err
	}
	snap, _ := json.Marshal(r)
	if err := u.repo.AddVersion(ctx, &Version{
		ResourceID: r.ResourceID,
		Version:    r.Version,
		Snapshot:   string(snap),
		Checksum:   checksum(snap),
		CreatedBy:  p.SubjectID,
		CreatedAt:  r.UpdatedAt,
	}); err != nil {
		return nil, err
	}
	return r, nil
}

// Get 读取资源元数据（带 read 判权）。
func (u *UseCase) Get(ctx context.Context, p *principal.Principal, resourceID string) (*Resource, error) {
	r, err := u.repo.Get(ctx, strings.TrimSpace(resourceID))
	if err != nil {
		return nil, err
	}
	ok, err := u.Check(ctx, p, r.ResourceID, ActionRead)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 无权读与不存在同口径，避免枚举资源 ID
		return nil, ErrNotFound
	}
	return r, nil
}

// List 列出当前身份可见的资源。
//
// 先按租户收敛（数据库条件），再逐条判权——资源量在租户维度是有界的，
// 这样写比把权限条件拼进 SQL 更不容易出错；规模化后再加权限索引。
func (u *UseCase) List(ctx context.Context, p *principal.Principal, f Filter) ([]Resource, error) {
	if p == nil {
		return nil, ErrForbidden
	}
	if f.TenantID == "" {
		f.TenantID = p.TenantID
	}
	if f.TenantID != p.TenantID && !p.Admin {
		return nil, fmt.Errorf("%w: 不能跨租户列举资源", ErrForbidden)
	}
	rows, err := u.repo.List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for i := range rows {
		ok, err := u.Check(ctx, p, rows[i].ResourceID, ActionRead)
		if err != nil || !ok {
			continue
		}
		out = append(out, rows[i])
	}
	return out, nil
}

// Delete 删除资源（仅归属人或具备 manage_permission 的主体）。
func (u *UseCase) Delete(ctx context.Context, p *principal.Principal, resourceID string) error {
	ok, err := u.Check(ctx, p, resourceID, ActionDelete)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 需要 delete 权限", ErrForbidden)
	}
	return u.repo.Delete(ctx, resourceID)
}

// Grant 显式授权。只有归属人 / manage_permission 能授，且不能授出比自己更大的权限
// （简化实现：授权人必须对该资源具备 manage_permission 或为归属人）。
func (u *UseCase) Grant(ctx context.Context, p *principal.Principal, resourceID, principalType, principalID string, actions []string, effect string) error {
	if p == nil {
		return ErrForbidden
	}
	resourceID = strings.TrimSpace(resourceID)
	principalType = strings.ToLower(strings.TrimSpace(principalType))
	principalID = strings.TrimSpace(principalID)
	if resourceID == "" || principalID == "" {
		return fmt.Errorf("%w: 资源与主体不能为空", ErrInvalid)
	}
	if !validPrincipalTypes[principalType] {
		return fmt.Errorf("%w: 未知主体类型 %q", ErrInvalid, principalType)
	}
	effect = strings.ToLower(strings.TrimSpace(effect))
	if effect == "" {
		effect = EffectAllow
	}
	if effect != EffectAllow && effect != EffectDeny {
		return fmt.Errorf("%w: 未知授权效果 %q", ErrInvalid, effect)
	}
	if len(actions) == 0 {
		return fmt.Errorf("%w: 至少指定一个动作", ErrInvalid)
	}
	r, err := u.repo.Get(ctx, resourceID)
	if err != nil {
		return err
	}
	// 跨租户授受一律拒绝：显式授权不能突破硬性租户隔离（V2 文档 5.6 第 3 条）
	ok, err := u.canManage(ctx, p, r)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 需要 manage_permission 权限", ErrForbidden)
	}
	if principalType == PrincipalTenant && principalID != r.TenantID {
		return fmt.Errorf("%w: 不能把租户资源授权给其他租户", ErrForbidden)
	}
	now := time.Now()
	for _, a := range actions {
		a = strings.ToLower(strings.TrimSpace(a))
		if !validActions[a] {
			return fmt.Errorf("%w: 未知动作 %q", ErrInvalid, a)
		}
		if err := u.repo.AddGrant(ctx, &Grant{
			ResourceID:    resourceID,
			PrincipalType: principalType,
			PrincipalID:   principalID,
			Action:        a,
			Effect:        effect,
			CreatedAt:     now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Revoke 撤销显式授权。
func (u *UseCase) Revoke(ctx context.Context, p *principal.Principal, resourceID, principalType, principalID, action string) error {
	r, err := u.repo.Get(ctx, strings.TrimSpace(resourceID))
	if err != nil {
		return err
	}
	ok, err := u.canManage(ctx, p, r)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 需要 manage_permission 权限", ErrForbidden)
	}
	return u.repo.DeleteGrant(ctx, r.ResourceID,
		strings.ToLower(strings.TrimSpace(principalType)),
		strings.TrimSpace(principalID),
		strings.ToLower(strings.TrimSpace(action)))
}

// Grants 列出资源上的显式授权（需要 manage_permission）。
func (u *UseCase) Grants(ctx context.Context, p *principal.Principal, resourceID string) ([]Grant, error) {
	r, err := u.repo.Get(ctx, strings.TrimSpace(resourceID))
	if err != nil {
		return nil, err
	}
	ok, err := u.canManage(ctx, p, r)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: 需要 manage_permission 权限", ErrForbidden)
	}
	return u.repo.Grants(ctx, r.ResourceID)
}

// Versions 版本历史（read 即可看，但只返回当前租户资源）。
func (u *UseCase) Versions(ctx context.Context, p *principal.Principal, resourceID string) ([]Version, error) {
	if _, err := u.Get(ctx, p, resourceID); err != nil {
		return nil, err
	}
	return u.repo.ListVersions(ctx, strings.TrimSpace(resourceID))
}

// Check 资源授权判定（默认拒绝）。实现 V2 文档 4.4 的 ResourceAuthorization。
// Registered 资源是否已登记。
//
// 运行时二次授权必须区分「未登记」与「明确拒绝」：Check 对未登记资源
// 同样返回 false（默认拒绝），如果直接用 Check 的结论，所有还没接入
// resource.Register 的旧资源都会在运行时被一刀切拒绝。
func (u *UseCase) Registered(ctx context.Context, resourceID string) (bool, error) {
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return false, nil
	}
	_, err := u.repo.Get(ctx, resourceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (u *UseCase) Check(ctx context.Context, p *principal.Principal, resourceID, action string) (bool, error) {
	if p == nil {
		return false, nil
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if !validActions[action] {
		return false, fmt.Errorf("%w: 未知动作 %q", ErrInvalid, action)
	}
	r, err := u.repo.Get(ctx, strings.TrimSpace(resourceID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}

	// 2. 租户硬隔离：平台公共资源允许跨租户只读
	publicRead := r.Visibility == VisibilityPublic && action == ActionRead
	if r.TenantID != "" && r.TenantID != p.TenantID {
		if !publicRead && !p.Admin {
			return false, nil
		}
	}

	grants, err := u.repo.Grants(ctx, r.ResourceID)
	if err != nil {
		return false, err
	}
	// 3/4. 显式 Deny 优先，其次显式 Allow
	allowedByGrant, denied := matchGrants(grants, p, action)
	if denied {
		return false, nil
	}
	if allowedByGrant {
		return true, nil
	}

	// 5. 归属人：对自己登记的资源拥有全部动作
	if r.OwnerID != "" && r.OwnerID == p.SubjectID {
		return true, nil
	}

	// 5'. 空间继承：资源挂在某个空间下，且该空间对调用方可见
	if r.SpaceID != "" && u.spaces != nil {
		ids, err := u.spaces.AllowedSpaceIDs(ctx, p)
		if err != nil {
			return false, err
		}
		if contains(ids, r.SpaceID) {
			return true, nil
		}
	}

	// 6. 平台公共资源只读
	if publicRead {
		return true, nil
	}
	// 租户共享资源：同租户可读（写操作仍要求显式授权或归属）
	if r.Visibility == VisibilityTenant && r.TenantID == p.TenantID && action == ActionRead {
		return true, nil
	}
	return false, nil
}

func (u *UseCase) canManage(ctx context.Context, p *principal.Principal, r *Resource) (bool, error) {
	if p == nil || r == nil {
		return false, nil
	}
	if p.Admin {
		return true, nil
	}
	if r.OwnerID != "" && r.OwnerID == p.SubjectID {
		return true, nil
	}
	grants, err := u.repo.Grants(ctx, r.ResourceID)
	if err != nil {
		return false, err
	}
	allowed, denied := matchGrants(grants, p, ActionManagePermission)
	return allowed && !denied, nil
}

// matchGrants 判断显式授权是否命中当前主体。
//
// 命中规则：
//   - user / service：principal_id == subjectID
//   - department：principal_id ∈ 调用方部门
//   - tenant：principal_id == 租户
//   - role：principal_id 出现在令牌 roles 里（只作参考，仍受租户隔离约束）
func matchGrants(grants []Grant, p *principal.Principal, action string) (allowed, denied bool) {
	for _, g := range grants {
		if !strings.EqualFold(g.Action, action) {
			continue
		}
		if !grantMatches(g, p) {
			continue
		}
		if g.Effect == EffectDeny {
			denied = true
			continue
		}
		allowed = true
	}
	return allowed, denied
}

func grantMatches(g Grant, p *principal.Principal) bool {
	if p == nil {
		return false
	}
	switch strings.ToLower(g.PrincipalType) {
	case PrincipalUser, PrincipalService:
		return equalFold(g.PrincipalID, p.SubjectID)
	case PrincipalDepartment:
		return p.InDepartment(g.PrincipalID)
	case PrincipalTenant:
		return equalFold(g.PrincipalID, p.TenantID)
	case PrincipalRole:
		return p.HasRole(g.PrincipalID)
	}
	return false
}

var validPrincipalTypes = map[string]bool{
	PrincipalUser: true, PrincipalDepartment: true, PrincipalTenant: true,
	PrincipalRole: true, PrincipalService: true,
}

func normalizeResource(r *Resource) error {
	if r == nil {
		return fmt.Errorf("%w: 资源为空", ErrInvalid)
	}
	r.ResourceID = strings.TrimSpace(r.ResourceID)
	if r.ResourceID == "" {
		return fmt.Errorf("%w: resource_id 不能为空", ErrInvalid)
	}
	r.ResourceType = strings.ToLower(strings.TrimSpace(r.ResourceType))
	if !validTypes[r.ResourceType] {
		return fmt.Errorf("%w: 未知资源类型 %q", ErrInvalid, r.ResourceType)
	}
	switch r.Visibility {
	case "":
		r.Visibility = VisibilityPrivate
	case VisibilityPrivate, VisibilityTenant, VisibilityPublic:
	default:
		return fmt.Errorf("%w: 未知可见性 %q", ErrInvalid, r.Visibility)
	}
	return nil
}

func checksum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// SortActions 去重排序（测试与展示用）
func SortActions(in []string) []string {
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
	sort.Strings(out)
	return out
}
