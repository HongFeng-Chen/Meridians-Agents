// org.go 系统管理域 MySQL 仓储：tenants / users 两表。
//
// 建表与播种都放在构造函数里（auto_migrate 打开时）：public 租户必须存在，
// 否则认证关闭时的开发身份、历史数据的 tenant_id='public' 在控制台里查不到归属。
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/org"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// OrgRepo 租户与成员仓库
type OrgRepo struct {
	db *gorm.DB
}

var _ org.Repo = (*OrgRepo)(nil)

// NewOrgRepo 创建仓库；auto_migrate 打开时自动建表并补齐内置租户
func NewOrgRepo(c *conf.Data, d *db.Data) (org.Repo, error) {
	r := &OrgRepo{db: d.DB()}
	if !c.GetDatabase().GetAutoMigrate() {
		return r, nil
	}
	if err := r.db.AutoMigrate(&models.TenantPO{}, &models.UserPO{}); err != nil {
		return nil, fmt.Errorf("迁移 tenants/users 表: %w", err)
	}
	// 播种 public：已存在则只补空名称，不覆盖线上改动
	seed := models.TenantPO{
		Key:    org.DefaultTenantKey,
		Name:   "默认租户",
		Status: org.StatusActive,
		Remark: "认证关闭时的开发身份与历史数据归属，不可删除",
	}
	if err := r.db.Where("tenant_key = ?", seed.Key).Attrs(seed).FirstOrCreate(&seed).Error; err != nil {
		return nil, fmt.Errorf("初始化内置租户: %w", err)
	}
	return r, nil
}

// —— 租户 ——

// ListTenants 租户列表：内置租户排最前，其余按创建时间正序。
func (r *OrgRepo) ListTenants(ctx context.Context, f org.TenantFilter) ([]org.Tenant, error) {
	q := r.db.WithContext(ctx).Model(&models.TenantPO{})
	if f.Key != "" {
		q = q.Where("tenant_key = ?", f.Key)
	}
	if kw := strings.TrimSpace(f.Q); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		q = q.Where("(tenant_key LIKE ? OR name LIKE ?)", like, like)
	}
	if st := strings.TrimSpace(f.Status); st != "" {
		q = q.Where("status = ?", st)
	}
	var rows []models.TenantPO
	if err := q.Order("tenant_key = 'public' DESC, created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]org.Tenant, 0, len(rows))
	for i := range rows {
		out = append(out, toTenant(&rows[i]))
	}
	return out, nil
}

// GetTenant 按 key 取租户
func (r *OrgRepo) GetTenant(ctx context.Context, key string) (*org.Tenant, error) {
	var row models.TenantPO
	err := r.db.WithContext(ctx).Where("tenant_key = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, org.ErrTenantNotFound
	}
	if err != nil {
		return nil, err
	}
	t := toTenant(&row)
	return &t, nil
}

// CreateTenant 新建租户
func (r *OrgRepo) CreateTenant(ctx context.Context, t *org.Tenant) error {
	row := models.TenantPO{
		Key:    t.Key,
		Name:   t.Name,
		Status: t.Status,
		Remark: t.Remark,
	}
	err := r.db.WithContext(ctx).Create(&row).Error
	if isDuplicate(err) {
		return org.ErrTenantExists
	}
	return err
}

// UpdateTenant 更新名称/状态/备注（key 是主键，不允许变更）
func (r *OrgRepo) UpdateTenant(ctx context.Context, t *org.Tenant) error {
	res := r.db.WithContext(ctx).Model(&models.TenantPO{}).
		Where("tenant_key = ?", t.Key).
		Updates(map[string]any{
			"name":       t.Name,
			"status":     t.Status,
			"remark":     t.Remark,
			"updated_at": time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return org.ErrTenantNotFound
	}
	return nil
}

// DeleteTenant 删除租户行（历史任务保留）
func (r *OrgRepo) DeleteTenant(ctx context.Context, key string) error {
	res := r.db.WithContext(ctx).Where("tenant_key = ?", key).Delete(&models.TenantPO{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return org.ErrTenantNotFound
	}
	return nil
}

// Stats 批量统计成员数与任务数
func (r *OrgRepo) Stats(ctx context.Context, keys []string) (map[string]org.Stat, error) {
	out := make(map[string]org.Stat, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	type row struct {
		TenantID string `gorm:"column:tenant_id"`
		N        int64  `gorm:"column:n"`
	}
	for i := range keys {
		out[keys[i]] = org.Stat{}
	}

	var users []row
	if err := r.db.WithContext(ctx).Model(&models.UserPO{}).
		Select("tenant_id, COUNT(*) AS n").
		Where("tenant_id IN ?", keys).
		Group("tenant_id").Scan(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		s := out[u.TenantID]
		s.UserCount = int(u.N)
		out[u.TenantID] = s
	}

	var tasks []row
	if err := r.db.WithContext(ctx).Model(&models.AgentTaskPO{}).
		Select("tenant_id, COUNT(*) AS n").
		Where("tenant_id IN ?", keys).
		Group("tenant_id").Scan(&tasks).Error; err != nil {
		return nil, err
	}
	for _, t := range tasks {
		s := out[t.TenantID]
		s.TaskCount = t.N
		out[t.TenantID] = s
	}
	return out, nil
}

// —— 成员 ——

// listUsersLimit 单次返回上限：名册是「人」的量级，超过这个数说明该分页而不是全量拉。
const listUsersLimit = 500

// ListUsers 成员列表
func (r *OrgRepo) ListUsers(ctx context.Context, f org.UserFilter) ([]org.User, int, error) {
	q := r.db.WithContext(ctx).Model(&models.UserPO{})
	if f.TenantID != "" {
		q = q.Where("tenant_id = ?", f.TenantID)
	}
	if kw := strings.TrimSpace(f.Q); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		q = q.Where("(username LIKE ? OR display_name LIKE ? OR email LIKE ?)", like, like, like)
	}
	if role := strings.TrimSpace(f.Role); role != "" {
		q = q.Where("role = ?", role)
	}
	if st := strings.TrimSpace(f.Status); st != "" {
		q = q.Where("status = ?", st)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.UserPO
	if err := q.Order("tenant_id ASC, id ASC").Limit(listUsersLimit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]org.User, 0, len(rows))
	for i := range rows {
		out = append(out, toUser(&rows[i]))
	}
	return out, int(total), nil
}

// GetUser 按 id 取成员
func (r *OrgRepo) GetUser(ctx context.Context, id int64) (*org.User, error) {
	var row models.UserPO
	err := r.db.WithContext(ctx).First(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, org.ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	u := toUser(&row)
	return &u, nil
}

// CreateUser 新建成员
func (r *OrgRepo) CreateUser(ctx context.Context, u *org.User) error {
	row := models.UserPO{
		TenantID:    u.TenantID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Role:        u.Role,
		Status:      u.Status,
		Source:      u.Source,
	}
	err := r.db.WithContext(ctx).Create(&row).Error
	if isDuplicate(err) {
		return org.ErrUserExists
	}
	if err != nil {
		return err
	}
	u.ID = int64(row.ID)
	return nil
}

// UpdateUser 更新资料/角色/状态（tenant_id 与 username 不可变）
func (r *OrgRepo) UpdateUser(ctx context.Context, u *org.User) error {
	res := r.db.WithContext(ctx).Model(&models.UserPO{}).
		Where("id = ?", u.ID).
		Updates(map[string]any{
			"display_name": u.DisplayName,
			"email":        u.Email,
			"role":         u.Role,
			"status":       u.Status,
			"updated_at":   time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return org.ErrUserNotFound
	}
	return nil
}

// DeleteUser 删除成员
func (r *OrgRepo) DeleteUser(ctx context.Context, id int64) error {
	res := r.db.WithContext(ctx).Where("id = ?", id).Delete(&models.UserPO{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return org.ErrUserNotFound
	}
	return nil
}

// CountActiveAdmins 租户内启用状态的租户管理员数量
func (r *OrgRepo) CountActiveAdmins(ctx context.Context, tenantID string, excludeID int64) (int, error) {
	q := r.db.WithContext(ctx).Model(&models.UserPO{}).
		Where("tenant_id = ? AND role = ? AND status = ?", tenantID, "tenant_admin", org.StatusActive)
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}

// CountUsers 租户内成员数
func (r *OrgRepo) CountUsers(ctx context.Context, tenantID string) (int, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&models.UserPO{}).Where("tenant_id = ?", tenantID).Count(&n).Error
	return int(n), err
}

func toTenant(p *models.TenantPO) org.Tenant {
	return org.Tenant{
		Key:       p.Key,
		Name:      p.Name,
		Status:    p.Status,
		Remark:    p.Remark,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

func toUser(p *models.UserPO) org.User {
	return org.User{
		ID:          int64(p.ID),
		TenantID:    p.TenantID,
		Username:    p.Username,
		DisplayName: p.DisplayName,
		Email:       p.Email,
		Role:        p.Role,
		Status:      p.Status,
		Source:      p.Source,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
}

// —— 登录链路 ——

// FindUserByUsername 按（租户, 用户名）取成员。租户隔离沿用唯一索引
// uk_users_tenant_username：同租户内唯一，跨租户允许同名（那是不同的人）。
func (r *OrgRepo) FindUserByUsername(ctx context.Context, tenantID, username string) (*org.User, error) {
	var row models.UserPO
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND username = ?", ownerOr(tenantID), username).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, org.ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	u := toUser(&row)
	return &u, nil
}

// GetUserCredentials 登录验密专用：只取密码哈希与状态，不构造完整档案。
func (r *OrgRepo) GetUserCredentials(ctx context.Context, tenantID, username string) (string, string, error) {
	var row models.UserPO
	err := r.db.WithContext(ctx).
		Select("password_hash", "status").
		Where("tenant_id = ? AND username = ?", ownerOr(tenantID), username).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", "", org.ErrUserNotFound
	}
	if err != nil {
		return "", "", err
	}
	return row.PasswordHash, row.Status, nil
}

// SetPasswordHash 写入/重置密码哈希
func (r *OrgRepo) SetPasswordHash(ctx context.Context, id int64, passwordHash string) error {
	res := r.db.WithContext(ctx).Model(&models.UserPO{}).
		Where("id = ?", id).
		Updates(map[string]any{"password_hash": passwordHash, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return org.ErrUserNotFound
	}
	return nil
}

// SyncLDAPUser LDAP 登录后的档案同步。
//
// 角色以「组映射结果」为准（每次登录都会同步）：AD 里的组成员关系变了，
// 平台角色跟着变，不需要管理员来平台手工改——这是 LDAP 接入的核心收益。
// 停用状态不受影响（disabled 用户在登录用例里就被拒了，走不到这里）。
func (r *OrgRepo) SyncLDAPUser(ctx context.Context, tenantID, username, displayName, email, role string, provision bool) (*org.User, error) {
	u, err := r.FindUserByUsername(ctx, tenantID, username)
	if err == nil {
		u.DisplayName = displayName
		u.Email = email
		u.Role = role
		if uerr := r.UpdateUser(ctx, u); uerr != nil {
			return nil, uerr
		}
		return u, nil
	}
	if !errors.Is(err, org.ErrUserNotFound) {
		return nil, err
	}
	if !provision {
		return nil, org.ErrUserNotFound
	}
	nu := &org.User{
		TenantID:    ownerOr(tenantID),
		Username:    username,
		DisplayName: displayName,
		Email:       email,
		Role:        role,
		Status:      org.StatusActive,
		Source:      org.SourceLDAP,
	}
	if err := r.CreateUser(ctx, nu); err != nil {
		return nil, err
	}
	return nu, nil
}
