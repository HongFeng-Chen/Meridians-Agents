package prompt

import "regexp"

// placeholderRe 匹配 {{name}}，允许花括号内空格；变量名限定 ASCII 标识符，
// 避免中文/标点误匹配把正文当成变量。
var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_.\-]*)\s*\}\}`)

// keyRe 模板 key 形态：小写点分层（rag.qa.system），也允许业务自定义命名
var keyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-]{0,127}$`)

// ValidKey 校验模板 key 合法性
func ValidKey(key string) bool { return keyRe.MatchString(key) }

// Placeholders 提取模板中的变量名：去重，保持首次出现顺序。
func Placeholders(tpl string) []string {
	matches := placeholderRe.FindAllStringSubmatch(tpl, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := m[1]
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// Render 用 vars 替换 {{name}}，返回渲染结果与缺失变量名。
//
// 缺失的变量原样保留占位符——预览接口据此高亮，正式渲染由调用方决定是否报错。
// 变量值为空字符串视为「已提供」，这是合法用法（例如空上下文）。
func Render(tpl string, vars map[string]string) (string, []string) {
	var missing []string
	seen := make(map[string]struct{})

	out := placeholderRe.ReplaceAllStringFunc(tpl, func(raw string) string {
		name := placeholderRe.FindStringSubmatch(raw)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			missing = append(missing, name)
		}
		return raw
	})
	return out, missing
}
