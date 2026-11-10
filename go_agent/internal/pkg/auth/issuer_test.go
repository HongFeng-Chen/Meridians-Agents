package auth

import (
	"errors"
	"testing"
	"time"

	"go_agent/internal/pkg/identity"
)

func TestHashAndCheckPassword(t *testing.T) {
	h, err := HashPassword("s3cret-pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) == 0 || h == "s3cret-pw" {
		t.Fatalf("哈希异常: %q", h)
	}
	if err := CheckPassword(h, "s3cret-pw"); err != nil {
		t.Fatalf("正确密码应通过: %v", err)
	}
	if err := CheckPassword(h, "wrong"); err == nil {
		t.Fatal("错误密码不应通过")
	}
}

func TestHashPasswordRejectsWeak(t *testing.T) {
	for _, pw := range []string{"", "abc12", "  abc12 "} {
		if _, err := HashPassword(pw); err == nil {
			t.Fatalf("弱密码 %q 应被拒绝", pw)
		}
	}
}

func TestCheckPasswordEmptyHash(t *testing.T) {
	if err := CheckPassword("", "whatever"); !errors.Is(err, ErrNoLocalPassword) {
		t.Fatalf("空哈希应返回 ErrNoLocalPassword，得到 %v", err)
	}
}

// 签发 → 验签必须走同一个闭环：Verifier 就是网关的那一套，登录态不被特殊对待。
func TestIssuerRoundtrip(t *testing.T) {
	iss := NewIssuer("test-secret", 1*time.Hour)
	if iss == nil {
		t.Fatal("配置了密钥应返回 issuer")
	}
	token, exp, err := iss.Issue(identity.Info{
		UserID: "alice", TenantID: "acme", Auth: "jwt",
		Roles: []string{"tenant_admin"}, Admin: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Before(time.Now()) {
		t.Fatal("过期时间应在未来")
	}

	v := New("test-secret", nil)
	info, err := v.Verify(token)
	if err != nil {
		t.Fatalf("网关应能验签登录令牌: %v", err)
	}
	if info.UserID != "alice" || info.TenantID != "acme" || info.Auth != "jwt" {
		t.Fatalf("身份不符: %+v", info)
	}
	// tenant_admin 不触发仅管理员闸门；换 platform_admin 应触发
	if info.Admin {
		t.Fatal("tenant_admin 不应被当成平台管理员")
	}
}

func TestIssuerPlatformAdmin(t *testing.T) {
	iss := NewIssuer("test-secret", 0)
	token, _, err := iss.Issue(identity.Info{UserID: "root", TenantID: "public", Roles: []string{"platform_admin"}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := New("test-secret", nil).Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Admin {
		t.Fatal("platform_admin 应被网关识别为管理员（兼容旧闸门）")
	}
}

func TestIssuerRejectsNoSecret(t *testing.T) {
	if iss := NewIssuer("", 0); iss != nil {
		t.Fatal("空密钥应返回 nil")
	}
	iss := NewIssuer("k", 0)
	iss.Secret = nil
	if _, _, err := iss.Issue(identity.Info{UserID: "x"}); !errors.Is(err, ErrIssuerNotConfigured) {
		t.Fatalf("应返回 ErrIssuerNotConfigured: %v", err)
	}
}

// 篡改 payload 后网关必须拒绝（防止改 role claim 提权）。
func TestTamperedTokenRejected(t *testing.T) {
	iss := NewIssuer("test-secret", 0)
	token, _, err := iss.Issue(identity.Info{UserID: "alice", TenantID: "public", Roles: []string{"user"}})
	if err != nil {
		t.Fatal(err)
	}
	// 把签名段换掉
	forged := token[:len(token)-4] + "AAAA"
	if _, err := New("test-secret", nil).Verify(forged); err == nil {
		t.Fatal("篡改后的令牌必须被拒绝")
	}
}
