// Package mediadb image_generations / video_generations 两个仓储共用的
// gorm 查询与编码辅助。只放无状态小函数，避免两个包互相依赖。
package mediadb

import (
	"encoding/json"
	"errors"

	"go_agent/internal/biz/llm"

	"gorm.io/gorm"
)

// ErrNotFound gorm 查无记录（供调用方映射成领域错误）
var ErrNotFound = gorm.ErrRecordNotFound

// ApplyFilter 应用租户 / 用户 / 数据源 / 模型 / 结果过滤（Model 层已指定表）
func ApplyFilter(q *gorm.DB, f llm.MediaFilter) *gorm.DB {
	if f.TenantID != "" {
		q = q.Where("tenant_id = ?", f.TenantID)
	}
	if f.UserID != "" {
		q = q.Where("user_id = ?", f.UserID)
	}
	if f.Provider != "" {
		q = q.Where("provider = ?", f.Provider)
	}
	if f.Model != "" {
		q = q.Where("model = ?", f.Model)
	}
	if f.OK != nil {
		if *f.OK {
			q = q.Where("ok = ? OR status = ?", true, llm.MediaSucceeded)
		} else {
			q = q.Where("ok = ? OR status = ?", false, llm.MediaFailed)
		}
	}
	return q
}

// ClampLimit 分页上限（单页最多 100 条）
func ClampLimit(n int) int {
	if n < 1 || n > 100 {
		return 50
	}
	return n
}

// EncodeList 字符串数组 → JSON 文本（nil 存空串，减少无意义 "[]"）
func EncodeList(list []string) string {
	if len(list) == 0 {
		return ""
	}
	b, err := json.Marshal(list)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeList JSON 文本 → 字符串数组（空串 / 非法 JSON 返回 nil）
func DecodeList(s string) []string {
	s = trimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

// IsNotFound 判断 gorm 错误是否查无记录
func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
