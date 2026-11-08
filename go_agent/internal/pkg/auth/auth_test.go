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

// TestVerifyRoleClaim 管理员只能来自签名后的 role/roles claim：
// 登录用户默认不是管理员（P0：MCP 管理面曾只认「已登录」）。
func TestVerifyRoleClaim(t *testing.T) {
	v := New("secret", nil)
	exp := time.Now().Add(time.Hour).Unix()

	cases := []struct {
		name  string
		claim map[string]any
		want  bool
	}{
		{"字符串 admin", map[string]any{"sub": "u1", "exp": exp, "role": "admin"}, true},
		{"数组含 admin", map[string]any{"sub": "u1", "exp": exp, "roles": []any{"user", "admin"}}, true},
		{"布尔 is_admin", map[string]any{"sub": "u1", "exp": exp, "is_admin": true}, true},
		{"普通用户", map[string]any{"sub": "u1", "exp": exp, "role": "user"}, false},
		{"无角色", map[string]any{"sub": "u1", "exp": exp}, false},
	}
	for _, c := range cases {
		info, err := v.Verify(sign(t, "secret", c.claim, "HS256"))
		if err != nil {
			t.Fatalf("%s: verify: %v", c.name, err)
		}
		if info.Admin != c.want {
			t.Errorf("%s: Admin=%v, want %v", c.name, info.Admin, c.want)
		}
	}
}

// TestStaticTokenAdmin 静态令牌第三段 role=admin 才授予管理员
func TestStaticTokenAdmin(t *testing.T) {
	v := New("", map[string]identity.Info{
		"tok-admin": {UserID: "ops", TenantID: "acme", Auth: "static", Admin: true},
		"tok-user":  {UserID: "dev", TenantID: "acme", Auth: "static"},
	})
	if info, _ := v.Verify("tok-admin"); !info.Admin {
		t.Fatal("tok-admin 应为管理员")
	}
	if info, _ := v.Verify("tok-user"); info.Admin {
		t.Fatal("tok-user 不应为管理员")
	}
}

// 没有 exp 的签名令牌默认拒绝（否则一次泄露 = 永久后门）
func TestVerifyRequiresExp(t *testing.T) {
	v := New("secret", nil)
	tok := sign(t, "secret", map[string]any{"sub": "u"}, "HS256")
	if _, err := v.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("缺少 exp 应被拒绝，实际 %v", err)
	}

	// 显式放开（兼容历史签发体系）后才接受
	v2 := New("secret", nil)
	v2.RequireExp = false
	if _, err := v2.Verify(tok); err != nil {
		t.Fatalf("RequireExp=false 时应放行: %v", err)
	}
}

// iss / aud 配置后必须匹配（多系统共用密钥时的基本隔离）
func TestVerifyIssuerAudience(t *testing.T) {
	v := New("secret", nil)
	v.Issuer = "meridians"
	v.Audience = "agents-api"

	okTok := sign(t, "secret", map[string]any{
		"sub": "u", "iss": "meridians", "aud": "agents-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	}, "HS256")
	if _, err := v.Verify(okTok); err != nil {
		t.Fatalf("iss/aud 匹配应通过: %v", err)
	}

	// aud 用数组形式也应识别
	arrTok := sign(t, "secret", map[string]any{
		"sub": "u", "iss": "meridians", "aud": []string{"other", "agents-api"},
		"exp": time.Now().Add(time.Hour).Unix(),
	}, "HS256")
	if _, err := v.Verify(arrTok); err != nil {
		t.Fatalf("aud 数组应通过: %v", err)
	}

	badIss := sign(t, "secret", map[string]any{
		"sub": "u", "iss": "someone-else", "aud": "agents-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	}, "HS256")
	if _, err := v.Verify(badIss); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("iss 不匹配应拒绝，实际 %v", err)
	}

	badAud := sign(t, "secret", map[string]any{
		"sub": "u", "iss": "meridians", "aud": "other-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	}, "HS256")
	if _, err := v.Verify(badAud); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("aud 不匹配应拒绝，实际 %v", err)
	}
}

// iat 在未来（超出时钟偏移）视为非法签发
func TestVerifyRejectsFutureIat(t *testing.T) {
	v := New("secret", nil)
	tok := sign(t, "secret", map[string]any{
		"sub": "u",
		"iat": time.Now().Add(time.Hour).Unix(),
		"exp": time.Now().Add(2 * time.Hour).Unix(),
	}, "HS256")
	if _, err := v.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("未来 iat 应拒绝，实际 %v", err)
	}
}
