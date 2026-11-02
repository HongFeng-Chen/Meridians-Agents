package models

// ShortSessionPrefix 短期会话记忆键前缀：每个会话一个 List
const ShortSessionPrefix = "mem:short:"

// ShortSessionKey 会话键
func ShortSessionKey(sessionID string) string { return ShortSessionPrefix + sessionID }

// ShortSessionScanPattern 枚举全部会话时的 SCAN 模式
func ShortSessionScanPattern() string { return scanPattern(ShortSessionPrefix) }

// MaxSessionMessages 单会话最多保留的消息数（超出裁剪最旧的）。
//
// 保留策略属于数据形态的一部分：它决定了 List 的最大长度，
// 放在这里可以让「写多长」和「键怎么定」在同一处看到。
const MaxSessionMessages = 200
