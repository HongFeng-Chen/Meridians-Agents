// Package authn 外部身份源接入：LDAP / Active Directory。
//
// 验证流程（密码永远不落平台侧）：
//
//	连接(ldaps TLS 或 ldap+StartTLS) → 服务账号 bind → 按 user_filter 搜出用户 DN
//	→ 用「用户 DN + 用户密码」再 bind 一次（AD 侧验密）→ 重绑服务账号 → 读档案与 memberOf
//
// 与内置角色矩阵的关系：AD 不感知平台权限，这里只负责「证明你是谁、你在哪个组」，
// 组 → 平台角色的映射由配置（group_role_mappings）声明，映射结果写回 users 表。
package authn

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/conf"

	ldap "github.com/go-ldap/ldap/v3"
)

// 领域错误：登录用例据此决定提示与降级。
var (
	// ErrLdapNotConfigured 未配置 LDAP（url 为空）——调用方走本地密码路径
	ErrLdapNotConfigured = errors.New("未配置 LDAP 身份源")
	// ErrLdapAuthFailed 用户名或密码错误（含用户在 AD 中不存在；统一口径不给探测信息）
	ErrLdapAuthFailed = errors.New("AD 账号或密码错误")
)

// LDAPUser 验密成功后的用户档案（全部来自 AD 属性）。
type LDAPUser struct {
	Username    string
	DisplayName string
	Email       string
	// Groups memberOf 列表（DN 原样）
	Groups []string
}

// LDAPClient LDAP / AD 客户端。无状态，可并发复用；每次验密建一次短连接
// （域控连接本身很廉价，池化收益小、故障面大）。
type LDAPClient struct {
	cfg *conf.LDAP
	// timeout 单次操作超时（连接/bind/search 各自限时）
	timeout time.Duration
}

// NewLDAPClient 配置为空（url 空）时返回 nil——调用方按 ErrLdapNotConfigured 走本地路径。
func NewLDAPClient(c *conf.LDAP) *LDAPClient {
	if c == nil || strings.TrimSpace(c.GetUrl()) == "" {
		return nil
	}
	return &LDAPClient{cfg: c, timeout: 10 * time.Second}
}

// Enabled 是否可用
func (c *LDAPClient) Enabled() bool { return c != nil }

// Authenticate 验证用户名密码；成功返回档案，失败返回 ErrLdapAuthFailed。
func (c *LDAPClient) Authenticate(username, password string) (*LDAPUser, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, ErrLdapAuthFailed
	}

	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetTimeout(c.timeout)

	if err := c.bindService(conn); err != nil {
		return nil, err
	}

	// 1. 搜出用户条目（拿 DN 与档案属性）
	userDN, entry, err := c.searchUser(conn, username)
	if err != nil {
		return nil, err
	}

	// 2. 用用户 DN + 用户密码再 bind：密码验证发生在 AD 侧，
	//    平台只拿到「bind 成功/失败」这一个布尔，不接触明文与哈希。
	if err := conn.Bind(userDN, password); err != nil {
		return nil, ErrLdapAuthFailed
	}

	// 3. 恢复服务账号身份，读 memberOf 等需要权限的属性
	_ = c.bindService(conn)
	return c.toUser(username, entry), nil
}

// dial 建立连接：ldaps:// 走隐式 TLS，ldap:// 可按要求升级 StartTLS。
func (c *LDAPClient) dial() (*ldap.Conn, error) {
	url := strings.TrimSpace(c.cfg.GetUrl())
	tlsCfg := &tls.Config{InsecureSkipVerify: c.cfg.GetSkipTLSVerify()} //nolint:gosec // 自签域控场景由 skip_tls_verify 显式放行
	conn, err := ldap.DialURL(url, ldap.DialWithTLSConfig(tlsCfg))
	if err != nil {
		return nil, fmt.Errorf("连接 LDAP 服务器: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(url), "ldaps://") && c.cfg.GetStartTLS() {
		if err := conn.StartTLS(tlsCfg); err != nil {
			conn.Close()
			return nil, fmt.Errorf("升级 StartTLS: %w", err)
		}
	}
	return conn, nil
}

// bindService 服务账号 bind；未配置凭据时尝试匿名（部分 OpenLDAP 允许）。
func (c *LDAPClient) bindService(conn *ldap.Conn) error {
	dn := strings.TrimSpace(c.cfg.GetBindDN())
	if dn == "" {
		return conn.UnauthenticatedBind("")
	}
	return conn.Bind(dn, c.cfg.GetBindPassword())
}

// searchUser 按 user_filter 搜用户；%s 替换为登录名（缺省按 AD 习惯查 sAMAccountName）。
func (c *LDAPClient) searchUser(conn *ldap.Conn, username string) (string, *ldap.Entry, error) {
	filter := strings.TrimSpace(c.cfg.GetUserFilter())
	if filter == "" {
		filter = "(sAMAccountName=%s)"
	}
	filter = strings.ReplaceAll(filter, "%s", ldap.EscapeFilter(username))

	req := ldap.NewSearchRequest(
		strings.TrimSpace(c.cfg.GetBaseDN()),
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, int(c.timeout.Seconds()), false,
		filter,
		c.attributes(),
		nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		return "", nil, fmt.Errorf("搜索用户 %q: %w", username, err)
	}
	switch {
	case len(res.Entries) == 0:
		// 用户不存在与密码错误同口径：不给攻击者「用户名有效」的探测信号
		return "", nil, ErrLdapAuthFailed
	case len(res.Entries) > 1:
		return "", nil, fmt.Errorf("过滤器 %q 命中 %d 个用户，请收紧 user_filter", filter, len(res.Entries))
	}
	return res.Entries[0].DN, res.Entries[0], nil
}

// attributes 需要回读的属性列表（含组归属）
func (c *LDAPClient) attributes() []string {
	attrs := []string{
		firstNonEmpty(c.cfg.GetUserIDAttr(), "sAMAccountName"),
		firstNonEmpty(c.cfg.GetMemberOfAttr(), "memberOf"),
	}
	if a := c.cfg.GetDisplayNameAttr(); a != "" {
		attrs = append(attrs, a)
	}
	if a := c.cfg.GetEmailAttr(); a != "" {
		attrs = append(attrs, a)
	}
	return attrs
}

// toUser 把 AD 条目转成平台档案
func (c *LDAPClient) toUser(username string, entry *ldap.Entry) *LDAPUser {
	u := &LDAPUser{Username: username}
	if a := c.cfg.GetDisplayNameAttr(); a != "" {
		u.DisplayName = firstAttr(entry, a)
	}
	if u.DisplayName == "" {
		u.DisplayName = username
	}
	if a := c.cfg.GetEmailAttr(); a != "" {
		u.Email = firstAttr(entry, a)
	}
	idAttr := firstNonEmpty(c.cfg.GetUserIDAttr(), "sAMAccountName")
	if v := firstAttr(entry, idAttr); v != "" {
		u.Username = v
	}
	mOf := firstNonEmpty(c.cfg.GetMemberOfAttr(), "memberOf")
	u.Groups = entry.GetAttributeValues(mOf)
	return u
}

// MapRole 按配置把 AD 组映射成平台角色；自上而下取第一个命中，未命中用 defaultRole。
// 组匹配大小写不敏感且只匹配子串（DN 完整格式各域控不一致，子串更抗迁移）。
func (c *LDAPClient) MapRole(groups []string) string {
	for _, m := range c.cfg.GetGroupRoleMappings() {
		key := strings.ToLower(strings.TrimSpace(m.GetGroup()))
		if key == "" {
			continue
		}
		for _, g := range groups {
			if strings.Contains(strings.ToLower(g), key) {
				return m.GetRole()
			}
		}
	}
	if r := strings.TrimSpace(c.cfg.GetDefaultRole()); r != "" {
		return r
	}
	return "user"
}

func firstAttr(e *ldap.Entry, name string) string {
	return strings.TrimSpace(e.GetAttributeValue(name))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
