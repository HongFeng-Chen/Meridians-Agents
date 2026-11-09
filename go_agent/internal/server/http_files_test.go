package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go_agent/internal/conf"
	"go_agent/internal/pkg/mediaauth"
)

// filesEnv 准备一个临时附件目录 + 返回认证开启的 handler
func filesEnv(t *testing.T, authOn bool) (*http.ServeMux, func()) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "202611"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "202611", "a.png"), []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mediaRootDir
	mediaRootDir = dir

	sec := &conf.Security{Auth: &conf.Auth{
		Enabled:      authOn,
		Header:       "Authorization",
		JwtSecret:    "test-secret",
		StaticTokens: map[string]string{"tok-admin": "u1:t1:admin"},
	}}
	g := newGate(sec)
	configureMediaAuth(sec)

	mux := http.NewServeMux()
	mux.Handle("/api/files/", filesHandler(g))
	return mux, func() {
		mediaRootDir = old
		mediaauth.Configure("")
	}
}

func TestFilesHandlerAllowsWhenAuthOff(t *testing.T) {
	mux, cleanup := filesEnv(t, false)
	defer cleanup()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/files/202611/a.png", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("认证关闭时应直接放行，实际 %d", rec.Code)
	}
	if rec.Body.String() != "PNG" {
		t.Fatalf("内容不对：%q", rec.Body.String())
	}
}

func TestFilesHandlerRequiresSignatureOrToken(t *testing.T) {
	mux, cleanup := filesEnv(t, true)
	defer cleanup()

	// 裸地址：未认证
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/files/202611/a.png", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("认证开启时裸地址应 401，实际 %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("401 不应被缓存，实际 %q", rec.Header().Get("Cache-Control"))
	}

	// 伪造签名
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/files/202611/a.png?sig=deadbeef", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("伪造签名应 401，实际 %d", rec.Code)
	}

	// 服务端签过的地址（读消息时补上的形态）
	signed := mediaauth.Sign("/api/files/202611/a.png")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, signed, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("带有效签名应放行，实际 %d", rec.Code)
	}

	// 换路径：把有效地址里的文件名改掉，签名必须失效（拿自己的地址读别人的文件）
	swapped := strings.Replace(signed, "a.png", "b.png", 1)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, swapped, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("签名只对原路径有效，实际 %d", rec.Code)
	}

	// 有效令牌（请求头）
	req := httptest.NewRequest(http.MethodGet, "/api/files/202611/a.png", nil)
	req.Header.Set("Authorization", "Bearer tok-admin")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带有效令牌应放行，实际 %d", rec.Code)
	}

	// 有效令牌（?token=，<img src> 只能用这种）
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/files/202611/a.png?token=tok-admin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("?token= 也应放行，实际 %d", rec.Code)
	}

	// 无效令牌
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/files/202611/a.png?token=bad", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无效令牌应 401，实际 %d", rec.Code)
	}
}
