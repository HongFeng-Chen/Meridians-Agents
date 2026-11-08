package mcp

// 出站访问策略在 MCP 域的适配层。
//
// 真正的策略与传输闸门在 internal/pkg/egress（技能包下载、模型网关等其它
// 出站场景共用同一套规则）；这里只做两件事：
//   - 把 egress 的错误包装成 ErrInvalidServer，保持 MCP 管理面统一的 400 语义；
//   - 保留 MCP 侧的调用点名字（ValidateEgressURL / CheckEgressHost …），
//     让「这里为什么要校验」在业务代码里一眼可见。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go_agent/internal/pkg/egress"
)

// InternalServerName 平台自己注册的 MCP 服务名（端点固定是本机回环的 /mcp/platform）。
//
// 它是出站策略里唯一被豁免的服务：地址由进程自己按监听端口拼出来，
// 不来自任何用户输入，因此「禁止回环」这条规则对它没有安全收益，
// 反而会让内置工具源在默认配置下直接不可用。
// 声明在这里是为了让 platformmcp 引用同一个常量（避免两处字符串漂移）。
const InternalServerName = "platform"

// IsInternalServer 是否为平台内置服务
func IsInternalServer(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), InternalServerName)
}

// ValidateEgressURL 校验 endpoint / spec_url / base_url 能否作为出站目标。
func ValidateEgressURL(raw string) error {
	if err := egress.ValidateURL(raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidServer, err)
	}
	return nil
}

// CheckEgressHost 主机（域名或字面 IP）校验
func CheckEgressHost(host string) error {
	if err := egress.CheckHost(host); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidServer, err)
	}
	return nil
}

// GuardOptions 传输级校验开关（别名，调用方不必同时 import 两个包）
type GuardOptions = egress.Options

// GuardedTransport 带出站校验的 Transport
func GuardedTransport(base *http.Transport, opts GuardOptions) *http.Transport {
	return egress.Transport(base, opts)
}

// GuardedClient 带出站校验的 http.Client（含重定向校验）
func GuardedClient(timeout time.Duration, opts GuardOptions) *http.Client {
	return egress.Client(timeout, opts)
}

// GuardRedirects 给已有 Client 补重定向校验
func GuardRedirects(c *http.Client, opts GuardOptions) *http.Client {
	return egress.GuardRedirects(c, opts)
}

// IsEgressBlocked 判断错误是否来自出站策略（便于上层区分「配置非法」与「网络故障」）
func IsEgressBlocked(err error) bool { return errors.Is(err, egress.ErrBlocked) }

// ValidateServerEndpoint 服务定义维度的出站校验：内置服务豁免，其余一律过策略。
func ValidateServerEndpoint(name, endpoint string) error {
	if IsInternalServer(name) {
		return nil
	}
	return ValidateEgressURL(endpoint)
}
