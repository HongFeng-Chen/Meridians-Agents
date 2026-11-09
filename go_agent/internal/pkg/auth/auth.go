// Package auth 凭证校验：HS256 JWT + 静态 Token（内网脚本 / CI 用）。
//
// 只依赖标准库，刻意不引入 JWT 框架——这里只需要「验签 + 取 claim + 校验有效期」
// 三件事，少一个依赖就少一处供应链风险。
//
// 支持的凭证：
//  1. JWT（HS256）：header.payload.signature，payload 里取 sub / tenant_id；
//  2. 静态 Token：配置里 map[token]{user,tenant}，适合内网调用与健康检查脚本。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go_agent/internal/pkg/identity"
)

// 领域错误：网关层据此返回 401（不区分细节，避免给攻击者提供探测信息）。
var (
	ErrMissingToken = errors.New("缺少访问令牌")
	ErrInvalidToken = errors.New("访问令牌无效")
	ErrExpiredToken = errors.New("访问令牌已过期")
)

// maxClockSkew 允许的时钟偏移（多节点部署时机器时间不会完全一致）。
const maxClockSkew = 60 * time.Second

// Verifier 凭证校验器。Secret 为空表示未配置 JWT（只接受静态 Token）。
type Verifier struct {
	Secret      []byte
	UserClaim   string
	TenantClaim string
	// RoleClaim 角色 claim 名（默认 role；同时兼容 roles 数组与 is_admin 布尔）
	RoleClaim string
	// RequireExp 强制要求 exp（默认 true）。
	// 没有 exp 的签名令牌永不过期——一旦泄露就是永久后门，而「忘记加 exp」
	// 在自建签发的场景里非常常见，所以默认拒绝而不是默认放行。
	RequireExp bool
	// Issuer / Audience 非空时校验 iss / aud（多系统共用密钥时的基本隔离）
	Issuer   string
	Audience string
	// Static 静态 Token → 身份（可选）
	Static map[string]identity.Info
	// Now 可注入时钟（单测用），为零值时用 time.Now
	Now func() time.Time
}

// New 构建校验器；claim 名为空时用默认值 sub / tenant_id / role。
func New(secret string, static map[string]identity.Info) *Verifier {
	return &Verifier{
		Secret:      []byte(strings.TrimSpace(secret)),
		UserClaim:   "sub",
		TenantClaim: "tenant_id",
		RoleClaim:   "role",
		RequireExp:  !envBool("AUTH_JWT_ALLOW_NO_EXP"),
		Issuer:      strings.TrimSpace(os.Getenv("AUTH_JWT_ISSUER")),
		Audience:    strings.TrimSpace(os.Getenv("AUTH_JWT_AUDIENCE")),
		Static:      static,
	}
}

// envBool 读取布尔环境变量（1/true/yes/on）
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Verify 校验令牌并返回身份。
func (v *Verifier) Verify(token string) (identity.Info, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return identity.Info{}, ErrMissingToken
	}
	if info, ok := v.Static[token]; ok {
		if info.Auth == "" {
			info.Auth = "static"
		}
		return info, nil
	}
	if len(v.Secret) == 0 {
		// 未配置密钥：JWT 一律拒绝（绝不降级为「解出 payload 就放行」）
		return identity.Info{}, ErrInvalidToken
	}
	return v.verifyHS256(token)
}

// verifyHS256 校验 JWT 签名与有效期。
func (v *Verifier) verifyHS256(token string) (identity.Info, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return identity.Info{}, ErrInvalidToken
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return identity.Info{}, ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return identity.Info{}, ErrInvalidToken
	}
	// 只接受 HS256：显式拒绝 alg=none 与 RS256（防算法混淆攻击）
	if !strings.EqualFold(header.Alg, "HS256") {
		return identity.Info{}, fmt.Errorf("%w: 不支持的签名算法 %q", ErrInvalidToken, header.Alg)
	}

	mac := hmac.New(sha256.New, v.Secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return identity.Info{}, ErrInvalidToken
	}
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return identity.Info{}, ErrInvalidToken
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity.Info{}, ErrInvalidToken
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return identity.Info{}, ErrInvalidToken
	}

	now := v.now()
	exp, hasExp := numberClaim(claims, "exp")
	if !hasExp {
		if v.RequireExp {
			return identity.Info{}, fmt.Errorf("%w: 令牌缺少 exp（有效期），拒绝接受长期有效令牌", ErrInvalidToken)
		}
	} else if now.After(time.Unix(exp, 0).Add(maxClockSkew)) {
		return identity.Info{}, ErrExpiredToken
	}
	if nbf, ok := numberClaim(claims, "nbf"); ok {
		if now.Add(maxClockSkew).Before(time.Unix(nbf, 0)) {
			return identity.Info{}, ErrInvalidToken
		}
	}
	// iat 在未来（超出时钟偏移）：签发时间不合理，按无效处理
	if iat, ok := numberClaim(claims, "iat"); ok {
		if now.Add(maxClockSkew).Before(time.Unix(iat, 0)) {
			return identity.Info{}, fmt.Errorf("%w: iat 晚于当前时间", ErrInvalidToken)
		}
	}
	if v.Issuer != "" {
		if got := stringClaim(claims, "iss"); got != v.Issuer {
			return identity.Info{}, fmt.Errorf("%w: iss 不匹配", ErrInvalidToken)
		}
	}
	if v.Audience != "" {
		if !audienceMatch(claims["aud"], v.Audience) {
			return identity.Info{}, fmt.Errorf("%w: aud 不匹配", ErrInvalidToken)
		}
	}

	user := stringClaim(claims, v.UserClaim)
	if user == "" {
		user = stringClaim(claims, "user_id")
	}
	if user == "" {
		return identity.Info{}, fmt.Errorf("%w: 缺少用户标识 claim %q", ErrInvalidToken, v.UserClaim)
	}
	tenant := stringClaim(claims, v.TenantClaim)
	if tenant == "" {
		tenant = identity.DefaultTenant
	}
	// 管理员判定只看签名后的 claim：登录 ≠ 管理员，管理面必须显式授权。
	admin := false
	roleClaim := firstNonEmpty(v.RoleClaim, "role")
	roles := append(stringListClaim(claims, roleClaim), stringListClaim(claims, "roles")...)
	for _, r := range roles {
		if isAdminRole(r) {
			admin = true
			break
		}
	}
	if b, ok := claims["is_admin"].(bool); ok && b {
		admin = true
	}
	return identity.Info{UserID: user, TenantID: tenant, Auth: "jwt", Admin: admin, Roles: roles}, nil
}

// isAdminRole 该角色是否等价于平台管理员。
// 除历史写法 admin/superadmin 外，RBAC 里的 platform_admin 也在此列，
// 保证「只发 platform_admin 角色」的令牌仍能通过旧的仅管理员闸门。
func isAdminRole(r string) bool {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "admin", "superadmin", "platform_admin", "platform-admin", "platformadmin":
		return true
	}
	return false
}

// stringClaim 读取字符串 claim（数字也转成字符串）。
func stringClaim(claims map[string]any, key string) string {
	if key == "" {
		return ""
	}
	switch v := claims[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

// numberClaim 读取数字 claim（Unix 秒）。
func numberClaim(claims map[string]any, key string) (int64, bool) {
	switch v := claims[key].(type) {
	case float64:
		return int64(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// audienceMatch aud 可能是字符串或字符串数组，任一命中即通过
func audienceMatch(claim any, want string) bool {
	switch v := claim.(type) {
	case string:
		return strings.TrimSpace(v) == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) == want {
				return true
			}
		}
	}
	return false
}

// TokenFromHeader 从 Authorization 头取令牌，兼容 "Bearer xxx" 与裸 token。
func TokenFromHeader(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

// stringListClaim 读取「字符串或字符串数组」形式的 claim（role / roles 两种写法都认）。
func stringListClaim(claims map[string]any, key string) []string {
	if key == "" {
		return nil
	}
	switch v := claims[key].(type) {
	case string:
		if s := strings.TrimSpace(v); s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
