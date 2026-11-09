// Package mediaauth 聊天附件的访问签名。
//
// 附件（图片 / 视频）走 /api/files/ 静态前缀，浏览器用 <img src> / <video src>
// 直接取，带不了 Authorization 头。以前这条路由完全不受鉴权管辖：
// 拿到地址的人（日志、转发链接、Referer）都能读别人的附件。
//
// 现在的做法是把「地址本身」变成凭据：
//
//	sig = hex(HMAC-SHA256(secret, path))
//
// 服务端在读消息时给附件地址补上 ?sig=，静态路由校验签名。
// 签名只覆盖路径，不覆盖用户，所以它是「能力型 URL」——
// 但这已经足够挡住「随手改路径读别人的文件」和「猜文件名批量扫」，
// 而且不需要前端配合（<img src> 仍然是个普通地址）。
//
// 需要真正按用户/会话做授权时，得给附件建索引（bucket/name → 会话/上传人），
// 那是数据模型的事，不在这一层。
package mediaauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
)

// param 签名参数名（与前端无关，前端只是原样使用后端给的地址）
const param = "sig"

var (
	mu     sync.RWMutex
	secret []byte
)

// Configure 配置签名密钥（启动时调用一次）。secret 为空表示关闭签名：
// 开发态（认证关闭）不签名也不校验，保持「打开就能看图」的体验。
func Configure(key string) {
	mu.Lock()
	defer mu.Unlock()
	key = strings.TrimSpace(key)
	if key == "" {
		secret = nil
		return
	}
	secret = []byte(key)
}

// ConfigureRandom 用进程内随机密钥启用签名。
//
// 用于「认证开启但没配 JWT 密钥」（纯静态令牌部署）：签名只在本次进程内有效，
// 重启后旧地址失效——附件地址在读取消息时现签，所以用户无感。
func ConfigureRandom() {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// 熵源不可用：退化成定长常量没有意义，直接关闭签名（行为等同未配置）
		Configure("")
		return
	}
	Configure(hex.EncodeToString(buf))
}

// Enabled 是否启用签名
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return len(secret) > 0
}

// Sign 给路径补签名参数；未启用时原样返回（幂等：已有签名会被替换）
func Sign(path string) string {
	if path == "" || !Enabled() {
		return path
	}
	base, q := splitQuery(path)
	q.Del(param)
	q.Set(param, sign(base))
	if enc := q.Encode(); enc != "" {
		return base + "?" + enc
	}
	return base
}

// Verify 校验路径上的签名是否匹配
func Verify(path, sig string) bool {
	if !Enabled() {
		return true
	}
	if sig == "" {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(sign(path)))
}

// VerifyURL 从完整 URL 里取出签名并校验（路径只取 Path 部分）
func VerifyURL(raw string) bool {
	if !Enabled() {
		return true
	}
	base, q := splitQuery(raw)
	return Verify(base, q.Get(param))
}

// PathOnly 去掉查询串，只留路径（落库 / 反向解析磁盘相对路径时用）
func PathOnly(raw string) string {
	base, _ := splitQuery(raw)
	return base
}

// splitQuery 拆出路径与查询参数；解析失败时按「整串是路径」处理
func splitQuery(raw string) (string, url.Values) {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		q, err := url.ParseQuery(raw[i+1:])
		if err != nil {
			q = url.Values{}
		}
		return raw[:i], q
	}
	return raw, url.Values{}
}

// sign 计算路径签名（固定长度 hex，便于日志与比较）
func sign(path string) string {
	mu.RLock()
	key := secret
	mu.RUnlock()
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(path))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}
