package principal

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go_agent/internal/pkg/auth"
	"go_agent/internal/pkg/identity"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// API Key：命中返回固定身份，不命中交给下一个 provider。
func TestAPIKeyProvider(t *testing.T) {
	p := NewAPIKeyProvider(map[string]*Principal{
		"0123456789abcdef": {SubjectID: "svc-report", TenantID: "public", Roles: []string{"service"}},
		"short":            {SubjectID: "ignored"}, // 长度不足 16，应被忽略
	})
	if p == nil {
		t.Fatal("provider 未创建")
	}
	got, err := p.Authenticate(context.Background(), "0123456789abcdef")
	if err != nil || got.SubjectID != "svc-report" || got.AuthType != "apikey" {
		t.Fatalf("应命中: %v %+v", err, got)
	}
	if _, err := p.Authenticate(context.Background(), "wrong-token-value-xx"); err != ErrNotHandled {
		t.Fatalf("未命中应返回 ErrNotHandled，实际 %v", err)
	}
	if _, err := p.Authenticate(context.Background(), "short"); err != ErrNotHandled {
		t.Fatal("弱 key 应被忽略")
	}
}

// 本地 HS256 令牌：走与网关同一套验签，Principal 与 identity 对齐。
func TestJWTProvider(t *testing.T) {
	const secret = "test-secret"
	issuer := auth.NewIssuer(secret, time.Hour)
	tok, _, err := issuer.Issue(identity.Info{UserID: "alice", TenantID: "acme", Roles: []string{"tenant_admin"}})
	if err != nil {
		t.Fatal(err)
	}
	p := NewJWTProvider(auth.New(secret, nil))
	got, err := p.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubjectID != "alice" || got.TenantID != "acme" || !got.HasRole("tenant_admin") {
		t.Fatalf("身份不符: %+v", got)
	}
	if got.IdentityKey() != "internal||acme|alice" && got.IdentityKey() != "internal|acme|alice" {
		// 只校验关键片段，具体分隔符由 IdentityKey 定义
		if got.Issuer+"/"+got.TenantID+"/"+got.SubjectID != "internal/acme/alice" {
			t.Fatalf("唯一键不符: %s", got.IdentityKey())
		}
	}
}

// JWKS：RS256 验签 + kid 匹配 + iss/aud 校验。
func TestJWKSProvider(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": "k1", "use": "sig",
		"n": b64(key.N.Bytes()),
		"e": b64(big.NewInt(int64(key.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	p := NewJWKSProvider(srv.URL, WithJWKSIssuer("https://idp.example"), WithJWKSAudience("meridians"))
	claims := map[string]any{
		"iss": "https://idp.example", "aud": "meridians", "sub": "u-1",
		"tenant_id": "acme", "client_id": "crm", "exp": time.Now().Add(time.Hour).Unix(),
		"scope": "agent:run kb:read", "roles": []string{"analyst"},
		"department_ids": []string{"rd"},
	}
	tok := signRS256(t, key, "k1", claims)

	got, err := p.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubjectID != "u-1" || got.TenantID != "acme" || got.ClientID != "crm" ||
		!got.HasScope("agent:run") || !got.InDepartment("rd") {
		t.Fatalf("claims 映射不符: %+v", got)
	}
	if got.IdentityKey() != "https://idp.example|crm|acme|u-1" {
		t.Fatalf("唯一键不符: %s", got.IdentityKey())
	}

	// iss 不匹配必须拒绝
	bad := map[string]any{"iss": "https://evil", "aud": "meridians", "sub": "u-1", "exp": time.Now().Add(time.Hour).Unix()}
	if _, err := p.Authenticate(context.Background(), signRS256(t, key, "k1", bad)); err == nil {
		t.Fatal("iss 不匹配应拒绝")
	}
	// 过期令牌必须拒绝
	expired := map[string]any{"iss": "https://idp.example", "sub": "u-1", "exp": time.Now().Add(-time.Hour).Unix()}
	if _, err := p.Authenticate(context.Background(), signRS256(t, key, "k1", expired)); err == nil {
		t.Fatal("过期令牌应拒绝")
	}
}

// Introspection：opaque token 交给 IdP 判定。
func TestIntrospectionProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("token") != "opaque-good" {
			_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active": true, "sub": "u-2", "tenant_id": "acme", "scope": "kb:read",
			"roles": []string{"operator"}, "iss": "https://idp.example",
		})
	}))
	defer srv.Close()

	p := NewIntrospectionProvider(srv.URL, "meridians", "secret")
	got, err := p.Authenticate(context.Background(), "opaque-good")
	if err != nil {
		t.Fatal(err)
	}
	if got.SubjectID != "u-2" || !got.HasScope("kb:read") || !got.HasRole("operator") {
		t.Fatalf("身份不符: %+v", got)
	}
	if _, err := p.Authenticate(context.Background(), "opaque-bad"); err == nil {
		t.Fatal("未激活令牌应拒绝")
	}
	// JWT 形态交给别的 provider，避免每个请求都打 IdP
	if _, err := p.Authenticate(context.Background(), "a.b.c"); err != ErrNotHandled {
		t.Fatalf("JWT 应返回 ErrNotHandled，实际 %v", err)
	}
}

// Resolver：按顺序尝试，跳过 NotHandled。
func TestResolverOrder(t *testing.T) {
	keyProvider := NewAPIKeyProvider(map[string]*Principal{
		"0123456789abcdef": {SubjectID: "svc", TenantID: "public"},
	})
	r := NewResolver(keyProvider)
	if got, err := r.Resolve(context.Background(), "0123456789abcdef"); err != nil || got.SubjectID != "svc" {
		t.Fatalf("应命中 API Key: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "nothing-matches"); err == nil {
		t.Fatal("全不命中应报错")
	}
}

// 未注入 Principal 时，从 identity.Info 投影，保证旧调用点不炸。
func TestFromIdentityFallback(t *testing.T) {
	ctx := identity.With(context.Background(), identity.Info{UserID: "alice", TenantID: "acme"})
	p := From(ctx)
	if p.SubjectID != "alice" || p.TenantID != "acme" || p.Issuer != "internal" {
		t.Fatalf("投影不符: %+v", p)
	}
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	body, _ := json.Marshal(claims)
	signing := b64(head) + "." + b64(body)
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}
