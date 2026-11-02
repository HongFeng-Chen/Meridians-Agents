// Package models 定义 Redis 的键空间（key schema）。
//
// Redis 没有表结构，这里的 model 就是「键长什么样」：键前缀、键构造、以及
// 失效时 SCAN 用的匹配模式。把它集中在这里的原因与 MySQL PO、Milvus Schema 一致——
// 数据形态只在一处定义，缓存实现（redis 包里的 cache_*.go）只负责读写，
// 运维排查时也能一眼看清全部键。
//
// 一个业务域一个文件：
//
//	key.go    通用：前缀 → SCAN 匹配模式的转换
//	answer.go RAG 问答缓存与入库锁（rag:ans:）
//	memory.go 短期会话记忆（mem:short:）
//	prompt.go 提示词生效版本（prompt:ver:）
//
// 新增缓存时先在这里定键，再写缓存实现，避免前缀散落在各处的字符串拼接里。
package models
