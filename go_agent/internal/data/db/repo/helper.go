// Package repo MySQL 仓储实现层：依赖 db（连接句柄）与 models（PO 定义），
// 实现 biz 层声明的 DocumentRepo / PromptRepo / LongTermRepo 接口。
package repo

import (
	"encoding/json"
	"strings"
)

// encodeTags 用 JSON 存字符串数组，避免逗号/引号等分隔符歧义
func encodeTags(list []string) string {
	if len(list) == 0 {
		return ""
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalTags(s string) []string {
	var tags []string
	_ = json.Unmarshal([]byte(s), &tags)
	return tags
}

// isDuplicate 判断是否唯一索引冲突（MySQL 1062 / SQLite UNIQUE，后者供单测用）
func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "1062") ||
		strings.Contains(strings.ToLower(msg), "unique constraint")
}

// escapeLike 转义 LIKE 通配符，避免用户输入的 % 全表扫描
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// clip 截断超长字符串，避免 MySQL 严格模式写入失败
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
