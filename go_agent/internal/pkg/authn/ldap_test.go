package authn

import (
	"testing"

	"go_agent/internal/conf"
)

func mkConf(mappings [][2]string, def string) *conf.LDAP {
	c := &conf.LDAP{Url: "ldap://ad.corp.example.com:389", DefaultRole: def}
	for _, m := range mappings {
		c.GroupRoleMappings = append(c.GroupRoleMappings, &conf.GroupRole{Group: m[0], Role: m[1]})
	}
	return c
}

func mustClient(t *testing.T, c *conf.LDAP) *LDAPClient {
	t.Helper()
	cl := NewLDAPClient(c)
	if cl == nil {
		t.Fatal("配置了 url 应返回客户端")
	}
	return cl
}

// 组映射自上而下取第一个命中；DN 是子串匹配且大小写不敏感
// （AD 的 DN 格式各域控不一致，完整匹配太脆）。
func TestMapRole(t *testing.T) {
	cl := mustClient(t, mkConf([][2]string{
		{"meridians-platformadmins", "platform_admin"},
		{"Meridians-Developers", "developer"},
	}, "user"))

	cases := []struct {
		groups []string
		want   string
	}{
		{[]string{"CN=Meridians-PlatformAdmins,OU=Groups,DC=corp"}, "platform_admin"},
		{[]string{"CN=Others", "CN=Meridians-Developers,OU=Groups"}, "developer"},
		{[]string{"CN=Unrelated"}, "user"},                          // 未命中走 default
		{nil, "user"},                                               // 没有组
		{[]string{"CN=meridians-platformadmins"}, "platform_admin"}, // 大小写不敏感
	}
	for _, c := range cases {
		if got := cl.MapRole(c.groups); got != c.want {
			t.Errorf("groups=%v got %q want %q", c.groups, got, c.want)
		}
	}
}

func TestMapRoleNoConfig(t *testing.T) {
	cl := mustClient(t, mkConf(nil, ""))
	if got := cl.MapRole(nil); got != "user" {
		t.Fatalf("无映射应兜底 user，得到 %q", got)
	}
}

// 未配置 url 时返回 nil，调用方走本地密码路径
func TestNewLDAPClientNil(t *testing.T) {
	if NewLDAPClient(nil) != nil || NewLDAPClient(&conf.LDAP{}) != nil {
		t.Fatal("未配置 url 应返回 nil")
	}
}

// 无服务端时 Authenticate 快速失败且报 ErrLdapAuthFailed / 连接错误，
// 不会 panic、不会挂死（超时 10s 由 SetTimeout 控制）。
func TestAuthenticateUnreachable(t *testing.T) {
	cl := mustClient(t, &conf.LDAP{Url: "ldap://127.0.0.1:1"})
	if _, err := cl.Authenticate("alice", "pw"); err == nil {
		t.Fatal("不可达的 LDAP 必须报错")
	}
}
