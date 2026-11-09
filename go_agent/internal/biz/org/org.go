package org

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"go_agent/internal/pkg/authz"
	"go_agent/internal/pkg/identity"
)

// tenantKeyPattern 租户键：小写字母开头，允许数字 / - / _，2-32 位。
// 不许大写：同一个租户在 JWT 里可能被写成 ACME，入库前统一小写，避免出现两个「同名」租户。
var tenantKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

// usernamePattern 用户名（JWT sub）：大小写敏感，允许 . _ -，2-64 位。
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,63}$`)

// UseCase 系统管理用例：租户与成员名册。
type UseCase struct {
	repo Repo
}

// New 构造用例
func New(repo Repo) *UseCase { return &UseCase{repo: repo} }

// —— 租户 ——

// ListTenants 租户列表（含成员数与任务数）。
//
// 非平台管理员只能看到自己所属租户：租户列表本身是「有哪些隔离域」的信息，
// 泄露它等于泄露平台上有哪些客户。
func (u *UseCase) ListTenants(ctx context.Context, f TenantFilter) ([]Tenant, error) {
	if !isPlatformAdmin(ctx) {
		f.Key = callerTenant(ctx)
	}
	list, err := u.repo.ListTenants(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return list, nil
	}
	keys := make([]string, 0, len(list))
	for i := range list {
		keys = append(keys, list[i].Key)
	}
	stats, err := u.repo.Stats(ctx, keys)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if s, ok := stats[list[i].Key]; ok {
			list[i].UserCount = s.UserCount
			list[i].TaskCount = s.TaskCount
		}
		list[i].Builtin = list[i].Key == DefaultTenantKey
	}
	return list, nil
}

// CreateTenant 新建租户（仅平台管理员）。
func (u *UseCase) CreateTenant(ctx context.Context, in TenantInput) (*Tenant, error) {
	if !isPlatformAdmin(ctx) {
		return nil, ErrForbidden
	}
	key, err := normalizeTenantKey(in.Key)
	if err != nil {
		return nil, err
	}
	if key == DefaultTenantKey {
		return nil, fmt.Errorf("%w：%s 是内置租户", ErrTenantExists, DefaultTenantKey)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = key
	}
	if len([]rune(name)) > 64 {
		return nil, fmt.Errorf("%w：名称最长 64 字符", ErrInvalidTenant)
	}
	remark := strings.TrimSpace(in.Remark)
	if len([]rune(remark)) > 255 {
		return nil, fmt.Errorf("%w：备注最长 255 字符", ErrInvalidTenant)
	}
	t := &Tenant{Key: key, Name: name, Status: StatusActive, Remark: remark}
	if err := u.repo.CreateTenant(ctx, t); err != nil {
		return nil, err
	}
	created, err := u.repo.GetTenant(ctx, key)
	if err != nil {
		return nil, err
	}
	created.Builtin = created.Key == DefaultTenantKey
	return created, nil
}

// UpdateTenant 修改名称 / 状态 / 备注（仅平台管理员）。
//
// key 不可改：它已经是 agent_tasks.tenant_id 等列的取值，改名等于把历史数据变成孤儿。
func (u *UseCase) UpdateTenant(ctx context.Context, key string, p TenantPatch) (*Tenant, error) {
	if !isPlatformAdmin(ctx) {
		return nil, ErrForbidden
	}
	key = strings.ToLower(strings.TrimSpace(key))
	cur, err := u.repo.GetTenant(ctx, key)
	if err != nil {
		return nil, err
	}
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return nil, fmt.Errorf("%w：名称不能为空", ErrInvalidTenant)
		}
		if len([]rune(name)) > 64 {
			return nil, fmt.Errorf("%w：名称最长 64 字符", ErrInvalidTenant)
		}
		cur.Name = name
	}
	if p.Status != nil {
		st, err := normalizeStatus(*p.Status)
		if err != nil {
			return nil, err
		}
		cur.Status = st
	}
	if p.Remark != nil {
		remark := strings.TrimSpace(*p.Remark)
		if len([]rune(remark)) > 255 {
			return nil, fmt.Errorf("%w：备注最长 255 字符", ErrInvalidTenant)
		}
		cur.Remark = remark
	}
	if err := u.repo.UpdateTenant(ctx, cur); err != nil {
		return nil, err
	}
	cur.Builtin = cur.Key == DefaultTenantKey
	return cur, nil
}

// DeleteTenant 删除租户（仅平台管理员）。
//
// 只删租户行，不动历史任务：任务属于执行证据，删租户不该把证据一起抹掉。
// 因此这里只拦「还有成员」的情况——成员是可以迁走的，任务不是。
func (u *UseCase) DeleteTenant(ctx context.Context, key string) error {
	if !isPlatformAdmin(ctx) {
		return ErrForbidden
	}
	key = strings.ToLower(strings.TrimSpace(key))
	if key == DefaultTenantKey {
		return ErrBuiltinTenant
	}
	if _, err := u.repo.GetTenant(ctx, key); err != nil {
		return err
	}
	n, err := u.repo.CountUsers(ctx, key)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w（当前 %d 人）", ErrTenantHasUsers, n)
	}
	return u.repo.DeleteTenant(ctx, key)
}

// —— 成员 ——

// ListUsers 成员列表。非平台管理员强制限定在自己租户内。
func (u *UseCase) ListUsers(ctx context.Context, f UserFilter) ([]User, int, error) {
	if !isPlatformAdmin(ctx) {
		f.TenantID = callerTenant(ctx)
	}
	return u.repo.ListUsers(ctx, f)
}

// CreateUser 登记成员。
func (u *UseCase) CreateUser(ctx context.Context, in UserInput) (*User, error) {
	tenant := strings.ToLower(strings.TrimSpace(in.TenantID))
	if tenant == "" {
		tenant = callerTenant(ctx)
	}
	if !isPlatformAdmin(ctx) && tenant != callerTenant(ctx) {
		return nil, fmt.Errorf("%w：只能在所属租户 %s 内添加成员", ErrForbidden, callerTenant(ctx))
	}
	if _, err := u.repo.GetTenant(ctx, tenant); err != nil {
		return nil, err
	}
	username := strings.TrimSpace(in.Username)
	if !usernamePattern.MatchString(username) {
		return nil, fmt.Errorf("%w：用户名 2-64 位，只能用字母、数字、. _ -", ErrInvalidUser)
	}
	role, err := normalizeRole(in.Role)
	if err != nil {
		return nil, err
	}
	if role == string(authz.RolePlatformAdmin) && !isPlatformAdmin(ctx) {
		return nil, fmt.Errorf("%w：platform_admin 只能由平台管理员授予", ErrForbidden)
	}
	email := strings.TrimSpace(in.Email)
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	display := strings.TrimSpace(in.DisplayName)
	if len([]rune(display)) > 64 {
		return nil, fmt.Errorf("%w：显示名最长 64 字符", ErrInvalidUser)
	}
	if display == "" {
		display = username
	}
	usr := &User{
		TenantID:    tenant,
		Username:    username,
		DisplayName: display,
		Email:       email,
		Role:        role,
		Status:      StatusActive,
		Source:      SourceManual,
	}
	if err := u.repo.CreateUser(ctx, usr); err != nil {
		return nil, err
	}
	return u.repo.GetUser(ctx, usr.ID)
}

// UpdateUser 修改成员资料 / 角色 / 状态。
//
// 两条硬保护：
//   - 不能停用自己（否则管理员点一下就失去入口，还得去令牌系统救急）；
//   - 租户必须保留至少一名启用状态的 tenant_admin（否则这个租户再没人能管成员）。
func (u *UseCase) UpdateUser(ctx context.Context, id int64, p UserPatch) (*User, error) {
	cur, err := u.scopedUser(ctx, id)
	if err != nil {
		return nil, err
	}
	self := cur.Username == callerUser(ctx)

	if p.DisplayName != nil {
		display := strings.TrimSpace(*p.DisplayName)
		if display == "" {
			return nil, fmt.Errorf("%w：显示名不能为空", ErrInvalidUser)
		}
		if len([]rune(display)) > 64 {
			return nil, fmt.Errorf("%w：显示名最长 64 字符", ErrInvalidUser)
		}
		cur.DisplayName = display
	}
	if p.Email != nil {
		email := strings.TrimSpace(*p.Email)
		if err := validateEmail(email); err != nil {
			return nil, err
		}
		cur.Email = email
	}
	demoteAdmin := false
	if p.Role != nil {
		role, err := normalizeRole(*p.Role)
		if err != nil {
			return nil, err
		}
		if role == string(authz.RolePlatformAdmin) && !isPlatformAdmin(ctx) {
			return nil, fmt.Errorf("%w：platform_admin 只能由平台管理员授予", ErrForbidden)
		}
		if cur.Role == string(authz.RoleTenantAdmin) && role != string(authz.RoleTenantAdmin) {
			demoteAdmin = true
		}
		cur.Role = role
	}
	disableAdmin := false
	if p.Status != nil {
		st, err := normalizeStatus(*p.Status)
		if err != nil {
			return nil, err
		}
		if self && st == StatusDisabled {
			return nil, ErrSelfProtect
		}
		if cur.Role == string(authz.RoleTenantAdmin) && st == StatusDisabled && cur.Status != StatusDisabled {
			disableAdmin = true
		}
		cur.Status = st
	}
	// 最后一个管理员保护只约束租户内自助管理。平台管理员是恢复通道：
	// 租户管理员离职、又没人能接手时，只有平台侧能把这个租户清理干净，
	// 否则「有成员的租户不可删」会变成永久死结。
	if (demoteAdmin || disableAdmin) && !isPlatformAdmin(ctx) {
		n, err := u.repo.CountActiveAdmins(ctx, cur.TenantID, cur.ID)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, ErrLastAdmin
		}
	}
	if err := u.repo.UpdateUser(ctx, cur); err != nil {
		return nil, err
	}
	return u.repo.GetUser(ctx, cur.ID)
}

// DeleteUser 移除成员。
//
// default 是历史数据与开发态的归属主体，删掉它会让「无主数据」变成常态；
// 自己则不该在控制台里把自己删出门。
func (u *UseCase) DeleteUser(ctx context.Context, id int64) error {
	cur, err := u.scopedUser(ctx, id)
	if err != nil {
		return err
	}
	if cur.Username == DefaultUser {
		return fmt.Errorf("%w：%s 是内置主体", ErrForbidden, DefaultUser)
	}
	if cur.Username == callerUser(ctx) {
		return ErrSelfProtect
	}
	if cur.Role == string(authz.RoleTenantAdmin) && cur.Status == StatusActive && !isPlatformAdmin(ctx) {
		n, err := u.repo.CountActiveAdmins(ctx, cur.TenantID, cur.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrLastAdmin
		}
	}
	return u.repo.DeleteUser(ctx, id)
}

// scopedUser 取成员并校验作用域：非平台管理员只能操作本租户成员。
func (u *UseCase) scopedUser(ctx context.Context, id int64) (*User, error) {
	cur, err := u.repo.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if !isPlatformAdmin(ctx) && cur.TenantID != callerTenant(ctx) {
		return nil, fmt.Errorf("%w：只能管理所属租户的成员", ErrForbidden)
	}
	return cur, nil
}

// —— 内部工具 ——

func isPlatformAdmin(ctx context.Context) bool {
	return authz.Allowed(ctx, authz.PermPlatformConfig)
}

func callerTenant(ctx context.Context) string {
	if t := identity.TenantID(ctx); t != "" {
		return strings.ToLower(t)
	}
	return DefaultTenantKey
}

func callerUser(ctx context.Context) string {
	if u := identity.UserID(ctx); u != "" {
		return u
	}
	return DefaultUser
}

func normalizeTenantKey(raw string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if !tenantKeyPattern.MatchString(key) {
		return "", fmt.Errorf("%w：租户键 2-32 位，小写字母开头，只能用字母、数字、- _", ErrInvalidTenant)
	}
	return key, nil
}

func normalizeStatus(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case StatusActive, "":
		return StatusActive, nil
	case StatusDisabled:
		return StatusDisabled, nil
	default:
		return "", fmt.Errorf("%w：状态只能是 %s / %s", ErrInvalidTenant, StatusActive, StatusDisabled)
	}
}

func normalizeRole(raw string) (string, error) {
	r := authz.Normalize(raw)
	if r == "" {
		return string(authz.RoleUser), nil
	}
	switch r {
	case authz.RolePlatformAdmin, authz.RoleTenantAdmin, authz.RoleDeveloper,
		authz.RoleOperator, authz.RoleAuditor, authz.RoleUser:
		return string(r), nil
	}
	return "", fmt.Errorf("%w：未知角色 %q", ErrInvalidUser, raw)
}

func validateEmail(email string) error {
	if email == "" {
		return nil
	}
	if len(email) > 128 || !strings.Contains(email, "@") || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") {
		return fmt.Errorf("%w：邮箱格式不正确", ErrInvalidUser)
	}
	return nil
}
