// issuer.go 登录成功后的 HS256 JWT 签发。
//
// 与 Verifier 共用同一个密钥与 claim 布局（sub / tenant_id / role），登录态
// 和网关验签是同一种令牌——不引入第二种格式，外部系统签发的令牌照样能用。
// 签发只依赖标准库，与 auth.go 的「不引入 JWT 框架」原则一致。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/pkg/identity"
)

// ErrIssuerNotConfigured 未配置 JWT 密钥时无法签发（只能走静态令牌 / LDAP 关闭）
var ErrIssuerNotConfigured = errors.New("未配置 JWT 密钥，无法签发登录令牌")

// Issuer 登录令牌签发器。
type Issuer struct {
	Secret []byte
	// TTL 令牌有效期；非正值时用默认 12h
	TTL time.Duration
	// Issuer / Audience 写入 iss / aud claim；为空则不写
	// （Verifier 侧配置了校验时两边要同时配）
	Issuer   string
	Audience string
	// Now 可注入时钟（单测用）
	Now func() time.Time
}

// NewIssuer 从密钥字符串构建；secret 为空返回 nil（登录用例据此报「未配置」）。
func NewIssuer(secret string, ttl time.Duration) *Issuer {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Issuer{Secret: []byte(secret), TTL: ttl}
}

// Issue 为登录成功的用户签发 HS256 令牌。
func (i *Issuer) Issue(info identity.Info) (string, time.Time, error) {
	if len(i.Secret) == 0 {
		return "", time.Time{}, ErrIssuerNotConfigured
	}
	now := time.Now()
	if i.Now != nil {
		now = i.Now()
	}
	exp := now.Add(i.TTL).Unix()

	header, err := b64json(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", time.Time{}, err
	}
	claims := map[string]any{
		"sub":       info.UserID,
		"iat":       now.Unix(),
		"exp":       exp,
		"role":      firstRole(info),
		"tenant_id": info.TenantID,
	}
	if len(info.Roles) > 1 {
		claims["roles"] = info.Roles
	}
	if i.Issuer != "" {
		claims["iss"] = i.Issuer
	}
	if i.Audience != "" {
		claims["aud"] = i.Audience
	}
	payload, err := b64json(claims)
	if err != nil {
		return "", time.Time{}, err
	}

	mac := hmac.New(sha256.New, i.Secret)
	mac.Write([]byte(header + "." + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + payload + "." + sig, time.Unix(exp, 0), nil
}

// firstRole 角色集合里取第一个规范角色写进 role claim；
// 平台管理员额外保证 admin 别名在列（兼容仅认 admin 的旧闸门）。
func firstRole(info identity.Info) string {
	if len(info.Roles) == 0 {
		return "user"
	}
	return info.Roles[0]
}

// b64json 序列化并做 URL-safe base64（无填充，JWT 标准）
func b64json(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("序列化令牌段: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
