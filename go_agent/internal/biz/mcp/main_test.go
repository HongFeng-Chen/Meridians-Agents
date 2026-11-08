package mcp

// 包级测试装配。
//
// 为什么需要：出站策略默认禁止访问回环/私网地址（防 SSRF），而本包的
// proxy / manager 测试全部依赖 httptest（127.0.0.1:随机端口）作为被调服务。
// 这里把回环放行一次，让业务链路测试保持可用；策略本身的拒绝行为由
// internal/pkg/egress 的用例覆盖（那是一个独立测试二进制，互不影响）。

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

// withEgressPolicy 在用例内临时覆盖出站策略，返回恢复函数
func withEgressPolicy(p egress.Policy) func() {
	prev := egress.CurrentPolicy()
	egress.SetPolicy(p)
	return func() { egress.SetPolicy(prev) }
}
