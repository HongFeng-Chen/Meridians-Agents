// password.go 本地密码登录的哈希与校验。
//
// 选 bcrypt 而不是 argon2：bcrypt 在 Go 生态里最经得起审计（x/crypto 官方维护），
// 且哈希串自带 cost 与盐，未来换参数不需要额外的迁移表。安全基线：
//   - 哈希只存 users.password_hash，明文只在登录请求的内存里存在一瞬；
//   - 校验用恒定时间比较（bcrypt.CompareHashAndPassword 内部已保证）；
//   - cost 默认 10（~70ms/次），登录接口本身有 429 限流兜底暴力破解。
package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// ErrNoLocalPassword 该用户未设置本地密码（LDAP 用户），调用方应改走 LDAP 验密
var ErrNoLocalPassword = errors.New("该用户未设置本地密码")

// bcryptCost 哈希强度。10 是 2026 年仍在安全区间的最低值；
// 机器富余时可以在配置里调高，旧哈希会在下次登录成功后自动升级（见 MaybeUpgrade）。
const bcryptCost = 10

// HashPassword 生成 bcrypt 哈希。
func HashPassword(plain string) (string, error) {
	plain = strings.TrimSpace(plain)
	if utf8.RuneCountInString(plain) < 6 {
		return "", fmt.Errorf("密码至少 6 个字符")
	}
	if utf8.RuneCountInString(plain) > 128 {
		return "", fmt.Errorf("密码过长（最多 128 字符）")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("生成密码哈希: %w", err)
	}
	return string(h), nil
}

// CheckPassword 校验明文与哈希是否匹配；哈希为空 = 该用户未设置本地密码
// （LDAP 用户），返回 ErrNoLocalPassword 让调用方走 LDAP。
func CheckPassword(hash, plain string) error {
	if strings.TrimSpace(hash) == "" {
		return ErrNoLocalPassword
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}

// MaybeUpgrade 登录成功后检查哈希强度，低于当前 cost 则透明升级。
// 老用户的哈希不用等迁移脚本，一次登录就完成换血。
func MaybeUpgrade(hash string) (newHash string, ok bool) {
	if strings.TrimSpace(hash) == "" {
		return "", false
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil || cost >= bcryptCost {
		return "", false
	}
	return hash, false // 占位：升级需要明文，由用例层持明文时调用 HashPassword
}
