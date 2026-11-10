package rag

// 知识作用域：一条文档「谁能看见、谁能改」。
//
// 三层知识与数据归属的关系：
//
//	system  平台通用知识：所有租户、所有用户可见；只有平台管理员（platform:config）能写
//	tenant  租户通用知识：同租户内所有用户可见；租户管理员（tenant:manage）能写
//	user    个人私有知识（默认）：只有本人可见；本人能写
//
// 为什么作用域放在 MySQL 的文档元数据上，而不是写进 Milvus 分块：
// 检索隔离一直是「先由 MySQL 算出可见 doc_id 白名单，再拼 doc_id in [...] 过滤」，
// 白名单本来就覆盖了 (tenant, user) 两个维度。作用域只是把这条规则从「按人」推广成
// 「按层」，不需要改向量库 schema——Milvus 的标量字段不支持在线加列，改 schema 意味着
// 重建集合、重灌全部向量，代价与收益不成比例。图谱同理：节点靠 chunk_refs 里的 doc_id
// 归属，可见文档集合一变，图谱召回范围自动跟着变。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go_agent/internal/pkg/authz"
	"go_agent/internal/pkg/identity"
)

// 作用域取值（写进 rag_documents.scope）
const (
	ScopeSystem = "system"
	ScopeTenant = "tenant"
	ScopeUser   = "user"
)

// ErrScopeForbidden 无权在目标作用域写入（或无权改动该文档）
var ErrScopeForbidden = errors.New("无权操作该知识作用域")

// ErrInvalidScope 作用域取值非法（参数错误，不是权限问题）
var ErrInvalidScope = errors.New("知识作用域不合法")

// NormalizeScope 归一化作用域：空值落到 user（个人私有，最保守的默认），
// 其余取值必须显式匹配。陌生取值直接报错而不是静默降级——
// 「写错了但看起来成功了」会让本该私有的内容以更宽的可见性落库。
func NormalizeScope(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", ScopeUser:
		return ScopeUser, nil
	case ScopeTenant:
		return ScopeTenant, nil
	case ScopeSystem:
		return ScopeSystem, nil
	default:
		return "", fmt.Errorf("%w: %q（可选 system / tenant / user）", ErrInvalidScope, s)
	}
}

// CanWriteScope 当前身份能否把知识写进该作用域。
//
// 只校验「作用域等级」，不校验具体文档归属；改已有文档时还要看 CanWriteDoc。
func CanWriteScope(ctx context.Context, scope string) bool {
	switch scope {
	case ScopeSystem:
		// platform:config 只给平台管理员（开发态回环另有 identity.AdminAllowed 兜底）
		return authz.Allowed(ctx, authz.PermPlatformConfig)
	case ScopeTenant:
		return authz.Allowed(ctx, authz.PermTenantManage)
	default:
		return true
	}
}

// CanWriteDoc 当前身份能否覆盖 / 删除一篇已存在的文档。
//
// 规则按文档自身的作用域判定，而不是按操作者的意图：
// 平台管理员可以改系统知识与任意租户的共享知识，但不能因此把私有文档「收编」成
// 自己的——user 作用域始终要求本人操作。
func CanWriteDoc(ctx context.Context, doc *Document) bool {
	if doc == nil {
		return true
	}
	me, tenant := identity.UserID(ctx), identity.TenantID(ctx)
	switch doc.Scope {
	case ScopeSystem:
		return authz.Allowed(ctx, authz.PermPlatformConfig)
	case ScopeTenant:
		if !sameTenant(doc.TenantID, tenant) {
			return false
		}
		// 创建者本人也能维护自己贡献的租户知识
		return doc.UserID == me || authz.Allowed(ctx, authz.PermTenantManage)
	default:
		if !sameTenant(doc.TenantID, tenant) {
			return false
		}
		// 历史数据（user_id 为空）保持可见可改，避免升级即失联
		return doc.UserID == "" || doc.UserID == me
	}
}

// scopeHint 错误信息里给「需要什么权限」的一句话说明
func scopeHint(scope string) string {
	switch scope {
	case ScopeSystem:
		return "平台管理员（platform:config）"
	case ScopeTenant:
		return "租户管理员（tenant:manage）"
	default:
		return "本人"
	}
}

// scopeName 作用域的中文名（错误信息与前端展示共用同一套措辞）
func scopeName(scope string) string {
	switch scope {
	case ScopeSystem:
		return "平台通用"
	case ScopeTenant:
		return "租户共享"
	default:
		return "个人私有"
	}
}

// Visible 文档对 (tenantID, userID) 是否可见——仓储 SQL 与内存实现共用这一条语义。
//
// 三个条件彼此独立，任一成立即可见：
//
//	scope=system                                  跨租户的公共知识
//	scope=tenant 且 tenant_id 相同                 租户内共享
//	scope=user   且 tenant_id 与 user_id 都相同     个人私有
func Visible(doc *Document, tenantID, userID string) bool {
	switch doc.Scope {
	case ScopeSystem:
		return true
	case ScopeTenant:
		return sameTenant(doc.TenantID, tenantID)
	default:
		if !sameTenant(doc.TenantID, tenantID) {
			return false
		}
		// userID 为空表示「不限用户」（管理视角），此时只按租户收敛
		return userID == "" || doc.UserID == userID
	}
}
