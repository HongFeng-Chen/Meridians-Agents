package models

// AnswerPrefix RAG 问答缓存键前缀。
//
// 键本身由 biz 侧生成（问题 + 过滤条件做 sha256 后取前 12 字节），
// 因为「什么算同一个问题」是领域规则；data 层只按前缀做批量失效。
// 两侧前缀必须一致：键的真实构造见 biz/rag.AnswerCacheKey。
const AnswerPrefix = "rag:ans:"

// AnswerScanPattern 清理全部问答缓存时的 SCAN 模式
func AnswerScanPattern() string { return scanPattern(AnswerPrefix) }

// IngestLockPrefix 文档入库锁前缀（SET NX EX 实现单文档并发导入互斥）。
//
// 锁键同样由 biz 侧拼接（见 biz/rag.IngestLockKey）；这里只登记前缀，
// 便于排查「某个文档的导入锁还在不在」。
const IngestLockPrefix = "rag:lock:ingest:"
