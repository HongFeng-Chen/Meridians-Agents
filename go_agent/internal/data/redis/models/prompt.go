package models

// PromptVersionPrefix 提示词「当前生效版本」键前缀。
//
// 与 rag 的缓存前缀分开，便于单独排查/清理：RAG 缓存失效（文档增删）时
// 不应把提示词缓存一起冲掉。
const PromptVersionPrefix = "prompt:ver:"

// PromptVersionKey 某个模板的生效版本键
func PromptVersionKey(promptKey string) string { return PromptVersionPrefix + promptKey }
