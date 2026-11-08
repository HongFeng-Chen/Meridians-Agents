package mcp

// 凭证脱敏（P1-03）：读接口不回显、写回掩码不丢值。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsSecretKey(t *testing.T) {
	secret := []string{"Authorization", "X-API-Key", "Cookie", "X-Company-Signature", "client_secret", "DB_PASSWORD", "access_token"}
	for _, k := range secret {
		if !IsSecretKey(k) {
			t.Fatalf("%q 应判定为凭证键", k)
		}
	}
	plain := []string{"Content-Type", "X-Tenant", "Accept", "sort_key", ""}
	for _, k := range plain {
		if IsSecretKey(k) {
			t.Fatalf("%q 不应判定为凭证键", k)
		}
	}
}

func TestMaskAndMergeHeaders(t *testing.T) {
	real := map[string]string{"Authorization": "Bearer sk-live-123", "X-Tenant": "acme"}
	masked := MaskHeaders(real)
	if masked["Authorization"] != MaskValue || masked["X-Tenant"] != MaskValue {
		t.Fatalf("值应全部打码，实际 %v", masked)
	}
	if _, ok := masked["Authorization"]; !ok {
		t.Fatal("键名应保留（前端要能显示配置了哪些头）")
	}

	// 表单原样提交（带掩码）→ 还原真实值
	back := MergeMaskedHeaders(masked, real)
	if back["Authorization"] != "Bearer sk-live-123" {
		t.Fatalf("掩码应还原为旧值，实际 %q", back["Authorization"])
	}
	// 用户改了值 → 用新值
	edited := MergeMaskedHeaders(map[string]string{"Authorization": "Bearer sk-new"}, real)
	if edited["Authorization"] != "Bearer sk-new" {
		t.Fatalf("新值应生效，实际 %q", edited["Authorization"])
	}
	// 空 map = 不改
	if got := MergeMaskedHeaders(nil, real); len(got) != 2 {
		t.Fatalf("空输入应保留原配置，实际 %v", got)
	}
	// 旧配置里没有的键 + 掩码 = 没填，忽略
	if got := MergeMaskedHeaders(map[string]string{"X-New": MaskValue}, real); len(got) != 0 {
		t.Fatalf("无旧值的掩码应被忽略，实际 %v", got)
	}
}

func TestMaskProxyConfig(t *testing.T) {
	raw := `{
		"base_url":"https://api.example.com",
		"auth":{"type":"static","token":"tk-live","header":"Authorization"},
		"request":{"headers":{"X-Tenant":"acme"},"query":{"appKey":"k1"}},
		"ops":[{"name":"a","secret":"s1"}]
	}`
	masked := MaskProxyConfig(raw)
	if masked == "" {
		t.Fatal("应返回打码后的配置")
	}
	if strings.Contains(masked, "tk-live") || strings.Contains(masked, `"s1"`) {
		t.Fatalf("凭证值不应出现在读接口输出里: %s", masked)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(masked), &m); err != nil {
		t.Fatalf("打码后仍应是合法 JSON: %v", err)
	}
	if m["base_url"] != "https://api.example.com" {
		t.Fatalf("非敏感字段应保留，实际 %v", m["base_url"])
	}

	// 原样写回 → 真实值回来
	merged := MergeMaskedProxyConfig(masked, raw)
	var back map[string]any
	if err := json.Unmarshal([]byte(merged), &back); err != nil {
		t.Fatalf("还原后应合法: %v", err)
	}
	auth := back["auth"].(map[string]any)
	if auth["token"] != "tk-live" {
		t.Fatalf("token 应还原，实际 %v", auth["token"])
	}
	ops := back["ops"].([]any)
	if ops[0].(map[string]any)["secret"] != "s1" {
		t.Fatalf("数组内凭证应还原，实际 %v", ops[0])
	}

	// 不可解析的配置整体不回显（宁可让用户重填，也不吐可能含密钥的原文）
	if got := MaskProxyConfig(`{"auth": {`); got != "" {
		t.Fatalf("非法 JSON 应返回空，实际 %q", got)
	}
}

func TestServerHasCredentials(t *testing.T) {
	if ServerHasCredentials(&Server{Headers: map[string]string{"Content-Type": "application/json"}}) {
		t.Fatal("普通头不算凭证")
	}
	if !ServerHasCredentials(&Server{Headers: map[string]string{"Authorization": "Bearer x"}}) {
		t.Fatal("Authorization 应算凭证")
	}
	if !ServerHasCredentials(&Server{Env: map[string]string{"DB_PASSWORD": "p"}}) {
		t.Fatal("env 里的密码应算凭证")
	}
	if !ServerHasCredentials(&Server{ProxyConfig: `{"auth":{"type":"static","token":"t"}}`}) {
		t.Fatal("代理配置里的 token 应算凭证")
	}
	if ServerHasCredentials(&Server{ProxyConfig: `{"auth":{"type":"none"}}`, Headers: map[string]string{}}) {
		t.Fatal("没有凭证时不应误报")
	}
}
