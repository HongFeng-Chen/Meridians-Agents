package service

// 系统管理面（多租户 + 成员名册）。
//
// 实现 api/admin/v1/admin.proto。规则全部在 internal/biz/org：
//   - 租户 key 不可改、public 不可删、有成员的租户不可删；
//   - 成员不能停用/删除自己，租户至少保留一名启用的 tenant_admin；
//   - 非平台管理员只能看/改自己租户（作用域在 biz 里强制，不依赖前端传参）。
//
// 权限：列表类走 tenant:manage（租户管理员可管本租户成员），租户的增删改走
// platform:config（隔离域本身是平台级配置）。

import (
	"context"
	"strconv"
	"time"

	v1 "go_agent/api/admin/v1"
	"go_agent/internal/biz/org"
	"go_agent/internal/pkg/authz"
)

// AdminService 系统管理面。
type AdminService struct {
	v1.UnimplementedAdminServiceServer

	uc *org.UseCase
}

var (
	_ v1.AdminServiceHTTPServer = (*AdminService)(nil)
	_ v1.AdminServiceServer     = (*AdminService)(nil)
)

// NewAdminService 创建服务
func NewAdminService(uc *org.UseCase) *AdminService {
	return &AdminService{uc: uc}
}

// ListTenants 租户列表。
func (s *AdminService) ListTenants(ctx context.Context, req *v1.ListTenantsRequest) (*v1.ListTenantsReply, error) {
	if err := requirePerm(ctx, authz.PermTenantManage, "admin.tenant.list", "tenant", ""); err != nil {
		return nil, err
	}
	list, err := s.uc.ListTenants(ctx, org.TenantFilter{Q: req.GetQ(), Status: req.GetStatus()})
	if err != nil {
		return nil, mapOrgErr(err)
	}
	out := &v1.ListTenantsReply{Items: make([]*v1.Tenant, 0, len(list)), Total: int32(len(list))}
	for i := range list {
		out.Items = append(out.Items, toTenant(&list[i]))
	}
	return out, nil
}

// CreateTenant 新建租户（平台级）。
func (s *AdminService) CreateTenant(ctx context.Context, req *v1.CreateTenantRequest) (*v1.TenantReply, error) {
	if err := requireAdminFor(ctx, "admin.tenant.create", "tenant", req.GetKey()); err != nil {
		return nil, err
	}
	t, err := s.uc.CreateTenant(ctx, org.TenantInput{
		Key:    req.GetKey(),
		Name:   req.GetName(),
		Remark: req.GetRemark(),
	})
	if err != nil {
		return nil, mapOrgErr(err)
	}
	return &v1.TenantReply{Tenant: toTenant(t)}, nil
}

// UpdateTenant 修改租户名称 / 状态 / 备注。
func (s *AdminService) UpdateTenant(ctx context.Context, req *v1.UpdateTenantRequest) (*v1.TenantReply, error) {
	if err := requireAdminFor(ctx, "admin.tenant.update", "tenant", req.GetKey()); err != nil {
		return nil, err
	}
	t, err := s.uc.UpdateTenant(ctx, req.GetKey(), org.TenantPatch{
		Name:   req.Name,
		Status: req.Status,
		Remark: req.Remark,
	})
	if err != nil {
		return nil, mapOrgErr(err)
	}
	return &v1.TenantReply{Tenant: toTenant(t)}, nil
}

// DeleteTenant 删除租户。
func (s *AdminService) DeleteTenant(ctx context.Context, req *v1.DeleteTenantRequest) (*v1.DeleteReply, error) {
	if err := requireAdminFor(ctx, "admin.tenant.delete", "tenant", req.GetKey()); err != nil {
		return nil, err
	}
	if err := s.uc.DeleteTenant(ctx, req.GetKey()); err != nil {
		return nil, mapOrgErr(err)
	}
	return &v1.DeleteReply{Id: req.GetKey(), Deleted: true}, nil
}

// ListUsers 成员列表。
func (s *AdminService) ListUsers(ctx context.Context, req *v1.ListUsersRequest) (*v1.ListUsersReply, error) {
	if err := requirePerm(ctx, authz.PermTenantManage, "admin.user.list", "user", ""); err != nil {
		return nil, err
	}
	list, total, err := s.uc.ListUsers(ctx, org.UserFilter{
		TenantID: req.GetTenantId(),
		Q:        req.GetQ(),
		Role:     req.GetRole(),
		Status:   req.GetStatus(),
	})
	if err != nil {
		return nil, mapOrgErr(err)
	}
	out := &v1.ListUsersReply{Items: make([]*v1.User, 0, len(list)), Total: int32(total)}
	for i := range list {
		out.Items = append(out.Items, toUser(&list[i]))
	}
	return out, nil
}

// CreateUser 登记成员。
func (s *AdminService) CreateUser(ctx context.Context, req *v1.CreateUserRequest) (*v1.UserReply, error) {
	if err := requirePerm(ctx, authz.PermTenantManage, "admin.user.create", "user", req.GetUsername()); err != nil {
		return nil, err
	}
	u, err := s.uc.CreateUser(ctx, org.UserInput{
		TenantID:    req.GetTenantId(),
		Username:    req.GetUsername(),
		DisplayName: req.GetDisplayName(),
		Email:       req.GetEmail(),
		Role:        req.GetRole(),
	})
	if err != nil {
		return nil, mapOrgErr(err)
	}
	return &v1.UserReply{User: toUser(u)}, nil
}

// UpdateUser 修改成员资料 / 角色 / 状态。
func (s *AdminService) UpdateUser(ctx context.Context, req *v1.UpdateUserRequest) (*v1.UserReply, error) {
	id := req.GetId()
	if err := requirePerm(ctx, authz.PermTenantManage, "admin.user.update", "user", strconv.FormatInt(id, 10)); err != nil {
		return nil, err
	}
	u, err := s.uc.UpdateUser(ctx, id, org.UserPatch{
		DisplayName: req.DisplayName,
		Email:       req.Email,
		Role:        req.Role,
		Status:      req.Status,
	})
	if err != nil {
		return nil, mapOrgErr(err)
	}
	return &v1.UserReply{User: toUser(u)}, nil
}

// DeleteUser 移除成员。
func (s *AdminService) DeleteUser(ctx context.Context, req *v1.DeleteUserRequest) (*v1.DeleteReply, error) {
	id := req.GetId()
	if err := requirePerm(ctx, authz.PermTenantManage, "admin.user.delete", "user", strconv.FormatInt(id, 10)); err != nil {
		return nil, err
	}
	if err := s.uc.DeleteUser(ctx, id); err != nil {
		return nil, mapOrgErr(err)
	}
	return &v1.DeleteReply{Id: strconv.FormatInt(id, 10), Deleted: true}, nil
}

func toTenant(t *org.Tenant) *v1.Tenant {
	if t == nil {
		return nil
	}
	return &v1.Tenant{
		Key:       t.Key,
		Name:      t.Name,
		Status:    t.Status,
		Remark:    t.Remark,
		UserCount: int32(t.UserCount),
		TaskCount: t.TaskCount,
		Builtin:   t.Builtin,
		CreatedAt: formatTime(t.CreatedAt),
		UpdatedAt: formatTime(t.UpdatedAt),
	}
}

func toUser(u *org.User) *v1.User {
	if u == nil {
		return nil
	}
	return &v1.User{
		Id:          u.ID,
		TenantId:    u.TenantID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Role:        u.Role,
		Status:      u.Status,
		Source:      u.Source,
		CreatedAt:   formatTime(u.CreatedAt),
		UpdatedAt:   formatTime(u.UpdatedAt),
	}
}

// formatTime 统一按 RFC3339 输出；零值返回空串（前端显示「—」而不是 0001-01-01）
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
