package service

import (
	"context"
	"testing"

	kerrors "github.com/go-kratos/kratos/v3/errors"

	"go_agent/internal/pkg/identity"
)

// ctxWith 构造指定身份上下文；auth 传 jwt/static/dev 之一
func ctxWith(user, auth string) context.Context {
	return identity.With(context.Background(), identity.Info{UserID: user, TenantID: "t1", Auth: auth})
}

// TestOwnsRecord 归属判断：空归属放行（历史数据），同用户放行，跨用户拒绝
func TestOwnsRecord(t *testing.T) {
	alice := ctxWith("alice", "jwt")
	if !ownsRecord(alice, "") {
		t.Fatalf("空归属应放行（历史数据）")
	}
	if !ownsRecord(alice, "alice") {
		t.Fatalf("同用户应放行")
	}
	if ownsRecord(alice, "bob") {
		t.Fatalf("跨用户必须拒绝")
	}
	// 未注入身份（内部链路）→ default 作用域
	if ownsRecord(context.Background(), "alice") {
		t.Fatalf("无身份上下文不应匹配真实用户数据")
	}
}

// TestEnsureOwnedForbidden 跨用户返回 403 语义的错误
func TestEnsureOwnedForbidden(t *testing.T) {
	err := ensureOwned(ctxWith("alice", "jwt"), "bob", "会话")
	if err == nil {
		t.Fatalf("跨用户应返回错误")
	}
	if code := kerrors.FromError(err).Code; code != 403 {
		t.Fatalf("错误码应为 403，实际 %d", code)
	}
	if err := ensureOwned(ctxWith("alice", "jwt"), "alice", "会话"); err != nil {
		t.Fatalf("同用户不应报错: %v", err)
	}
}

// TestScopedUserID 归属写入键：认证态强制令牌身份，开发态允许请求体指定
func TestScopedUserID(t *testing.T) {
	if got := scopedUserID(ctxWith("alice", "jwt"), "bob"); got != "alice" {
		t.Fatalf("认证态应忽略请求体 user_id，实际 %q", got)
	}
	if got := scopedUserID(ctxWith("alice", "static"), "bob"); got != "alice" {
		t.Fatalf("静态令牌态应忽略请求体 user_id，实际 %q", got)
	}
	if got := scopedUserID(ctxWith("default", "dev"), "bob"); got != "bob" {
		t.Fatalf("开发态应允许请求体指定用户，实际 %q", got)
	}
	if got := scopedUserID(ctxWith("default", "dev"), ""); got != "default" {
		t.Fatalf("开发态未指定时回落 default，实际 %q", got)
	}
}
