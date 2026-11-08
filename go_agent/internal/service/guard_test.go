package service

import (
	"context"
	"testing"

	"go_agent/internal/pkg/identity"

	kerrors "github.com/go-kratos/kratos/v3/errors"
)

// TestRequireAdmin 管理面闸门：只有显式管理员或「开发态 + 回环」才放行。
func TestRequireAdmin(t *testing.T) {
	cases := []struct {
		name string
		info identity.Info
		ok   bool
	}{
		{"JWT 管理员", identity.Info{UserID: "ops", Auth: "jwt", Admin: true}, true},
		{"静态令牌管理员", identity.Info{UserID: "ops", Auth: "static", Admin: true}, true},
		{"开发态本机", identity.Info{UserID: "dev", Auth: "dev", Local: true}, true},
		{"开发态远程匿名", identity.Info{UserID: "dev", Auth: "dev"}, false},
		{"已登录普通用户", identity.Info{UserID: "u1", Auth: "jwt"}, false},
		{"无身份", identity.Info{}, false},
	}
	for _, c := range cases {
		err := requireAdmin(identity.With(context.Background(), c.info), "mcp.server.create")
		if c.ok && err != nil {
			t.Errorf("%s: 期望放行, got %v", c.name, err)
		}
		if !c.ok {
			if err == nil {
				t.Errorf("%s: 期望 403, got nil", c.name)
				continue
			}
			if code := kerrors.FromError(err).Code; code != 403 {
				t.Errorf("%s: 期望 403, got %d", c.name, code)
			}
		}
	}
}
