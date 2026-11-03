package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go_agent/internal/pkg/identity"
)

func sign(t *testing.T, secret string, claims map[string]any, alg string) string {
	t.Helper()
	head, _ := json.Marshal(map[string]any{"alg": alg, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(head)
	p := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(h + "." + p))
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifyHS256(t *testing.T) {
	v := New("secret", nil)
	tok := sign(t, "secret", map[string]any{
		"sub":       "u-1",
		"tenant_id": "acme",
		"exp":       time.Now().Add(time.Hour).Unix(),
	}, "HS256")
	info, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if info.UserID != "u-1" || info.TenantID != "acme" || info.Auth != "jwt" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestVerifyRejects(t *testing.T) {
	v := New("secret", nil)
	cases := map[string]string{
		"错误密钥":    sign(t, "other", map[string]any{"sub": "u", "exp": time.Now().Add(time.Hour).Unix()}, "HS256"),
		"none 算法": sign(t, "secret", map[string]any{"sub": "u"}, "none"),
		"缺少 sub":  sign(t, "secret", map[string]any{"exp": time.Now().Add(time.Hour).Unix()}, "HS256"),
		"格式错误":    "abc",
		"空":       "",
	}
	for name, tok := range cases {
		if _, err := v.Verify(tok); err == nil {
			t.Fatalf("%s: 期望失败却通过", name)
		}
	}
}

func TestVerifyExpired(t *testing.T) {
	v := New("secret", nil)
	tok := sign(t, "secret", map[string]any{"sub": "u", "exp": time.Now().Add(-2 * time.Hour).Unix()}, "HS256")
	if _, err := v.Verify(tok); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("期望 ErrExpiredToken，得到 %v", err)
	}
}

func TestStaticToken(t *testing.T) {
	v := New("", map[string]identity.Info{"tok-abc": {UserID: "svc", TenantID: "ops"}})
	info, err := v.Verify("tok-abc")
	if err != nil {
		t.Fatalf("static: %v", err)
	}
	if info.UserID != "svc" || info.Auth != "static" {
		t.Fatalf("unexpected: %+v", info)
	}
	// 未配置密钥时，JWT 必须被拒（不能降级为只解 payload）
	bad := sign(t, "", map[string]any{"sub": "u"}, "HS256")
	if _, err := v.Verify(bad); err == nil {
		t.Fatal("空密钥时 JWT 应被拒绝")
	}
}

func TestTokenFromHeader(t *testing.T) {
	if got := TokenFromHeader("Bearer abc"); got != "abc" {
		t.Fatalf("bearer: %q", got)
	}
	if got := TokenFromHeader(" bearer  abc "); got != "abc" {
		t.Fatalf("trim: %q", got)
	}
	if got := TokenFromHeader("abc"); got != "abc" {
		t.Fatalf("raw: %q", got)
	}
}
