// Package redact 配置凭证的读接口脱敏与写回还原。
//
// 背景：MCP 服务配置的 Headers/Env、AI 平台的网关 Headers、代理配置里的
// Authorization/token/password，都属于「能直接用」的凭证。这些字段一旦随读接口
// 原样返回，任何能看详情页的人都能拿到（前端、浏览器缓存、日志、截图、工单附件
// 都会复制一份）。
//
// 约定：
//   - 读：值一律替换成 Mask（键名保留，前端才知道配了哪些）；
//   - 写：收到 Mask 表示「这次不改」，用旧值回填——表单原样提交不会抹掉凭证；
//   - 空输入表示「不改」，避免只改标题的请求把凭证清空。
//
// 这只解决「不再复制扩散」；静态加密存储（KMS / Envelope Encryption）是存储层的事。
package redact

import (
	"encoding/json"
	"strings"
)

// Mask 掩码字面量。写回该值表示「保持原值」。
const Mask = "******"

// secretKeys 明确是凭证的键（小写比较）
var secretKeys = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"api-key":             true,
	"apikey":              true,
	"api_key":             true,
	"token":               true,
	"access_token":        true,
	"refresh_token":       true,
	"id_token":            true,
	"password":            true,
	"passwd":              true,
	"pwd":                 true,
	"secret":              true,
	"client_secret":       true,
	"private_key":         true,
	"x-auth-token":        true,
	"auth":                true,
}

// secretFragments 键名里含有这些片段即视为凭证（X-Company-Signature 之类）
var secretFragments = []string{
	"token", "secret", "password", "passwd", "apikey", "api_key",
	"credential", "authorization", "signature",
}

// IsSecretKey 键名是否承载凭证
func IsSecretKey(k string) bool {
	n := strings.ToLower(strings.TrimSpace(k))
	if n == "" {
		return false
	}
	if secretKeys[n] {
		return true
	}
	for _, f := range secretFragments {
		if strings.Contains(n, f) {
			return true
		}
	}
	return false
}

// MaskMap 只保留键名，值替换为掩码。
//
// 非凭证键也一并打码：header 的语义无法从名字可靠判断（X-Company-Id 背后
// 可能是签名串），漏判的代价远大于「多看一次自己的配置」。
func MaskMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k := range in {
		out[k] = Mask
	}
	return out
}

// MergeMasked 把「掩码值」还原成旧值；空输入表示不改，直接返回旧值。
func MergeMasked(in, old map[string]string) map[string]string {
	if len(in) == 0 {
		return old
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v == Mask {
			if ov, ok := old[k]; ok {
				out[k] = ov
				continue
			}
			continue // 旧配置里没有这个键：掩码等于没填
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

// HasSecretValue 配置里是否有非空的凭证键
func HasSecretValue(in map[string]string) bool {
	for k, v := range in {
		if IsSecretKey(k) && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// MaskJSON 按 JSON 结构递归替换凭证字段的字符串值，结构（含数组）保持不变。
// 解析失败返回空串：宁可让用户重填，也不把可能带密钥的原文吐出去。
func MaskJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return ""
	}
	b, err := json.Marshal(maskJSON(v))
	if err != nil {
		return ""
	}
	return string(b)
}

// MergeMaskedJSON 配置里的掩码值还原为旧值；空输入表示不改。
func MergeMaskedJSON(in, old string) string {
	in = strings.TrimSpace(in)
	if in == "" {
		return old
	}
	var v any
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		return in // 交给上层校验报错
	}
	var ov any
	if strings.TrimSpace(old) != "" {
		_ = json.Unmarshal([]byte(old), &ov)
	}
	b, err := json.Marshal(mergeMaskedJSON(v, ov))
	if err != nil {
		return in
	}
	return string(b)
}

// JSONHasSecretValue 结构化配置里是否有非空凭证字段
func JSONHasSecretValue(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return false
	}
	return walkCredential(v)
}

func maskJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if s, ok := val.(string); ok && IsSecretKey(k) && s != "" {
				out[k] = Mask
				continue
			}
			out[k] = maskJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = maskJSON(t[i])
		}
		return out
	default:
		return v
	}
}

func mergeMaskedJSON(v, old any) any {
	switch t := v.(type) {
	case map[string]any:
		om, _ := old.(map[string]any)
		out := make(map[string]any, len(t))
		for k, val := range t {
			if s, ok := val.(string); ok && s == Mask {
				if ov, ok := om[k].(string); ok {
					out[k] = ov
				}
				continue // 旧配置没有这条：掩码当空处理
			}
			var ov any
			if om != nil {
				ov = om[k]
			}
			out[k] = mergeMaskedJSON(val, ov)
		}
		return out
	case []any:
		oa, _ := old.([]any)
		out := make([]any, len(t))
		for i := range t {
			var ov any
			if i < len(oa) {
				ov = oa[i]
			}
			out[i] = mergeMaskedJSON(t[i], ov)
		}
		return out
	default:
		return v
	}
}

func walkCredential(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok && IsSecretKey(k) && strings.TrimSpace(s) != "" {
				return true
			}
			if walkCredential(val) {
				return true
			}
		}
	case []any:
		for i := range t {
			if walkCredential(t[i]) {
				return true
			}
		}
	}
	return false
}
