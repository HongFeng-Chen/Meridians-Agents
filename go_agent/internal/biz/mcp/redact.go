package mcp

// 凭证脱敏（P1-03）：MCP 服务配置里的 Headers / Env / 代理配置可能带
// Authorization、Cookie、长期 token、数据库密码。读接口原样返回这些字段，
// 等于把凭证发给任何能打开服务详情的人。
//
// 通用逻辑在 internal/pkg/redact（AI 平台网关头也用它，避免两套口径）；
// 这里只保留 MCP 语义的包装：读接口打码、写接口把掩码还原成旧值。

import (
	"strings"

	"go_agent/internal/pkg/redact"
)

// MaskValue 掩码字面量。写回时该值表示「保持原值」。
const MaskValue = redact.Mask

// IsSecretKey 键名是否承载凭证
func IsSecretKey(k string) bool { return redact.IsSecretKey(k) }

// MaskHeaders 只保留键名，值替换为掩码
func MaskHeaders(in map[string]string) map[string]string { return redact.MaskMap(in) }

// MergeMaskedHeaders 把「掩码值」还原成旧值；空输入表示不改
func MergeMaskedHeaders(in, old map[string]string) map[string]string {
	return redact.MergeMasked(in, old)
}

// MaskProxyConfig 代理配置按 JSON 结构打码（凭证字段保留键、值打码）
func MaskProxyConfig(raw string) string { return redact.MaskJSON(raw) }

// MergeMaskedProxyConfig 配置里的掩码值还原为旧值；空输入表示不改
func MergeMaskedProxyConfig(in, old string) string { return redact.MergeMaskedJSON(in, old) }

// ServerHasCredentials 服务是否配置了凭证（读接口只给这个布尔，不回显值）
func ServerHasCredentials(s *Server) bool {
	if s == nil {
		return false
	}
	if redact.HasSecretValue(s.Headers) || redact.HasSecretValue(s.Env) {
		return true
	}
	if strings.TrimSpace(s.ProxyConfig) != "" {
		return redact.JSONHasSecretValue(s.ProxyConfig)
	}
	return false
}
