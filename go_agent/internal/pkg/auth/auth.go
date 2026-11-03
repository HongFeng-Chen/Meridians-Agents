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
	// Static 静态 Token → 身份（可选）
	Static map[string]identity.Info
	// Now 可注入时钟（单测用），为零值时用 time.Now
	Now func() time.Time
}

// New 构建校验器；claim 名为空时用默认值 sub / tenant_id。
func New(secret string, static map[string]identity.Info) *Verifier {
	return &Verifier{
		Secret:      []byte(strings.TrimSpace(secret)),
		UserClaim:   "sub",
		TenantClaim: "tenant_id",
		Static:      static,
	}
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
	if exp, ok := numberClaim(claims, "exp"); ok {
		if now.After(time.Unix(exp, 0).Add(maxClockSkew)) {
			return identity.Info{}, ErrExpiredToken
		}
	}
	if nbf, ok := numberClaim(claims, "nbf"); ok {
		if now.Add(maxClockSkew).Before(time.Unix(nbf, 0)) {
			return identity.Info{}, ErrInvalidToken
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
	return identity.Info{UserID: user, TenantID: tenant, Auth: "jwt"}, nil
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

// TokenFromHeader 从 Authorization 头取令牌，兼容 "Bearer xxx" 与裸 token。
func TokenFromHeader(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}
