package models

import "strings"

// ShortSessionPrefix 短期会话记忆键前缀：每个会话一个 List
const ShortSessionPrefix = "mem:short:"

// ShortSessionKey 会话键（无作用域）。
//
// 保留它是为了兼容已有数据与测试；业务读写一律走 ScopedSessionKey——
// 只用 session_id 做键，等于「猜到会话 ID 就能读到别人的对话」。
func ShortSessionKey(sessionID string) string { return ShortSessionPrefix + sessionID }

// ScopedSessionKey 带归属的会话键：mem:short:{tenant}:{user}:{session}
//
// 归属直接编码进键名，是这一层唯一可靠的隔离手段：伪造 session_id 只会落到
// 自己的命名空间（读到空列表），不需要额外查「会话归属表」，也不存在
// 「先查后判」的时间窗。tenant / user 为空时回落到占位符，
// 避免出现 mem:short:::s-1 这类字段错位、不同用户撞到同一个键。
func ScopedSessionKey(tenantID, userID, sessionID string) string {
	var b strings.Builder
	b.Grow(len(ShortSessionPrefix) + len(tenantID) + len(userID) + len(sessionID) + 2)
	b.WriteString(ShortSessionPrefix)
	b.WriteString(scopePart(tenantID))
	b.WriteByte(':')
	b.WriteString(scopePart(userID))
	b.WriteByte(':')
	b.WriteString(sessionID)
	return b.String()
}

func scopePart(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return "_"
}

// ShortSessionScanPattern 枚举全部会话时的 SCAN 模式
func ShortSessionScanPattern() string { return scanPattern(ShortSessionPrefix) }

// MaxSessionMessages 单会话最多保留的消息数（超出裁剪最旧的）。
//
// 保留策略属于数据形态的一部分：它决定了 List 的最大长度，
// 放在这里可以让「写多长」和「键怎么定」在同一处看到。
const MaxSessionMessages = 200
