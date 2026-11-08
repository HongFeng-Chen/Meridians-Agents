package skillhub

// 包级测试装配：客户端现在带出站校验（防 SSRF），而本包测试都用 httptest
// （127.0.0.1）模拟市场服务。这里放行回环地址，拒绝行为本身由
// internal/pkg/egress 的用例覆盖。

import (
	"os"
	"testing"

	"go_agent/internal/pkg/egress"
)

func TestMain(m *testing.M) {
	prev := egress.CurrentPolicy()
	p := prev
	p.AllowPrivate = true
	egress.SetPolicy(p)
	code := m.Run()
	egress.SetPolicy(prev)
	os.Exit(code)
}
