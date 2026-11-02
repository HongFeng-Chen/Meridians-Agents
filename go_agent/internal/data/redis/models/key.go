package models

// scanPattern 把键前缀转成 SCAN 匹配模式。
//
// 所有批量清理/枚举都必须走 SCAN + MATCH（而不是 KEYS），
// 统一从这里生成模式串，避免有人手写 "prefix*" 漏掉星号。
func scanPattern(prefix string) string { return prefix + "*" }
