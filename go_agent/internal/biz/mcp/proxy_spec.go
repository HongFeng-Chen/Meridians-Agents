package mcp

// Swagger / OpenAPI 文档解析：把 paths 下的接口转成工具定义。
//
// 支持两类文档：
//   - OpenAPI 3.x（openapi: 3.0.x / 3.1.x）：servers[].url、requestBody.content
//   - Swagger 2.0（swagger: "2.0"）：host + basePath + schemes、in: body / formData
//
// 只做本地 $ref 解析（#/components/schemas/…、#/definitions/…），不请求外部 $ref：
// 跨文件引用在代理场景里几乎都是打包好的单文件，遇到外链按 any 处理并在 warning 里提示。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// maxSpecBytes 文档大小上限（20MB：大厂中台动辄几千接口）
	maxSpecBytes = 20 << 20
	// maxSchemaDepth $ref 递归深度上限
	maxSchemaDepth = 12
	// maxToolsPerSpec 单个文档最多导入的接口数（防止把平台打满）
	maxToolsPerSpec = 800
	// maxSpecTextBytes 粘贴内容上限
	maxSpecTextBytes = 20 << 20
)

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true,
	"delete": true, "head": true, "options": true,
}

// specDoc 解析后的文档（中间态）
type specDoc struct {
	kind     string // openapi3 | swagger2
	title    string
	version  string
	baseURL  string
	raw      map[string]any
	ops      []ProxyOp
	warnings []string
}

// parseSpecText 解析文档内容（JSON 或 YAML 自适应）。
func parseSpecText(text []byte) (*specDoc, error) {
	if len(text) == 0 {
		return nil, fmt.Errorf("文档内容为空")
	}
	if len(text) > maxSpecTextBytes {
		return nil, fmt.Errorf("文档超过 %d MB 上限", maxSpecTextBytes>>20)
	}
	var raw map[string]any
	trimmed := strings.TrimSpace(string(text))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(text, &raw); err != nil {
			return nil, fmt.Errorf("YAML 解析失败: %w", err)
		}
	}
	return buildSpecDoc(normalizeMap(raw))
}

// normalizeMap YAML 解析出来可能是 map[string]interface{} / map[interface{}]interface{}，
// 统一成 map[string]any，避免后续到处做类型断言。
func normalizeMap(v any) map[string]any {
	m, ok := v.(map[string]any)
	if ok {
		return m
	}
	if mi, ok2 := v.(map[any]any); ok2 {
		out := make(map[string]any, len(mi))
		for k, val := range mi {
			out[fmt.Sprint(k)] = normalizeValue(val)
		}
		return out
	}
	return nil
}

func normalizeValue(v any) any {
	switch t := v.(type) {
	case map[any]any:
		return normalizeMap(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = normalizeValue(t[i])
		}
		return out
	default:
		return v
	}
}

// buildSpecDoc 识别文档类型并抽取接口。
func buildSpecDoc(raw map[string]any) (*specDoc, error) {
	if raw == nil {
		return nil, fmt.Errorf("不是合法的对象型文档")
	}
	d := &specDoc{raw: raw}
	if v, ok := raw["openapi"].(string); ok && strings.HasPrefix(v, "3") {
		d.kind = "openapi3"
	} else if v, ok := raw["swagger"].(string); ok && strings.HasPrefix(v, "2") {
		d.kind = "swagger2"
	} else {
		return nil, fmt.Errorf("无法识别文档版本：缺少 openapi: 3.x 或 swagger: \"2.0\"")
	}
	if info := asMap(raw["info"]); info != nil {
		d.title = str(info["title"])
		d.version = str(info["version"])
	}
	d.baseURL = specBaseURL(d.kind, raw)
	paths := asMap(raw["paths"])
	if paths == nil {
		return nil, fmt.Errorf("文档里没有 paths，无法生成任何工具")
	}

	used := map[string]int{}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names) // 稳定顺序：同一个文档多次解析结果一致
	for _, path := range names {
		item := asMap(paths[path])
		if item == nil {
			continue
		}
		commonParams := d.params(item["parameters"])
		methods := make([]string, 0, len(item))
		for m := range item {
			if httpMethods[strings.ToLower(m)] {
				methods = append(methods, strings.ToLower(m))
			}
		}
		sort.Strings(methods)
		for _, method := range methods {
			op := asMap(item[method])
			if op == nil {
				continue
			}
			if len(d.ops) >= maxToolsPerSpec {
				d.warnings = append(d.warnings, fmt.Sprintf("接口数超过 %d，其余已跳过", maxToolsPerSpec))
				return d, nil
			}
			o := d.buildOp(path, method, op, commonParams)
			// 同名接口（operationId 重复）加序号消歧
			if n, ok := used[o.Name]; ok {
				used[o.Name] = n + 1
				o.Name = fmt.Sprintf("%s_%d", o.Name, n+1)
			} else {
				used[o.Name] = 1
			}
			d.ops = append(d.ops, o)
		}
	}
	if len(d.ops) == 0 {
		return nil, fmt.Errorf("paths 里没有可导入的接口（只支持 GET/POST/PUT/PATCH/DELETE）")
	}
	return d, nil
}

// specBaseURL 文档声明的服务地址。
func specBaseURL(kind string, raw map[string]any) string {
	if kind == "openapi3" {
		if servers, ok := raw["servers"].([]any); ok && len(servers) > 0 {
			if s := asMap(servers[0]); s != nil {
				return strings.TrimRight(str(s["url"]), "/")
			}
		}
		return ""
	}
	// swagger2：schemes[0] + host + basePath
	host := str(raw["host"])
	basePath := strings.TrimRight(str(raw["basePath"]), "/")
	if basePath == "/" {
		basePath = ""
	}
	scheme := "https"
	if ss, ok := raw["schemes"].([]any); ok && len(ss) > 0 {
		if s := str(ss[0]); s != "" {
			scheme = s
		}
	}
	if host == "" {
		return basePath
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, basePath)
}

// buildOp 单个接口 → ProxyOp。
func (d *specDoc) buildOp(path, method string, raw map[string]any, common []ProxyParam) ProxyOp {
	o := ProxyOp{
		Method:      strings.ToUpper(method),
		Path:        path,
		Summary:     firstLine(str(raw["summary"])),
		Description: str(raw["description"]),
	}
	if tags, ok := raw["tags"].([]any); ok {
		for _, t := range tags {
			if s := str(t); s != "" {
				o.Tags = append(o.Tags, s)
			}
		}
	}
	if b, ok := raw["deprecated"].(bool); ok {
		o.Deprecated = b
	}

	// 参数：pathItem 级与 operation 级合并（同名同位置以 operation 为准）
	byKey := map[string]ProxyParam{}
	order := []string{}
	for _, p := range append(append([]ProxyParam{}, common...), d.params(raw["parameters"])...) {
		k := p.In + ":" + p.Name
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = p
	}
	for _, k := range order {
		o.Params = append(o.Params, byKey[k])
	}

	// 请求体
	if d.kind == "openapi3" {
		if rb := asMap(raw["requestBody"]); rb != nil {
			if content := asMap(rb["content"]); content != nil {
				schema := pickJSONContent(content)
				if schema != nil {
					resolved := d.resolve(schema, 0)
					o.BodySchema = d.schemaJSON(resolved)
				}
			}
		}
	} else {
		// swagger2：in=body 的 schema，或 formData 拼成 object
		formProps := map[string]any{}
		var formRequired []any
		kept := o.Params[:0]
		for _, p := range o.Params {
			switch p.In {
			case "body":
				if meta, ok := d.paramRaw(raw, p.Name, "body"); ok {
					schema := asMap(meta["schema"])
					if schema != nil {
						o.BodySchema = d.schemaJSON(d.resolve(schema, 0))
					}
				}
				continue // body 参数不进工具入参（统一用 body 字段）
			case "formdata":
				prop := map[string]any{"type": defaultStr(p.Type, "string")}
				if p.Description != "" {
					prop["description"] = p.Description
				}
				formProps[p.Name] = prop
				if p.Required {
					formRequired = append(formRequired, p.Name)
				}
				continue
			}
			kept = append(kept, p)
		}
		o.Params = kept
		if len(formProps) > 0 && o.BodySchema == "" {
			schema := map[string]any{"type": "object", "properties": formProps}
			if len(formRequired) > 0 {
				schema["required"] = formRequired
			}
			b, _ := json.Marshal(schema)
			o.BodySchema = string(b)
		}
	}

	o.Name = opToolName(str(raw["operationId"]), method, path)
	return o
}

// paramRaw 从原始 parameters 里找某个 body/formData 参数定义。
func (d *specDoc) paramRaw(op map[string]any, name, in string) (map[string]any, bool) {
	list, _ := op["parameters"].([]any)
	for _, it := range list {
		m := asMap(it)
		if m == nil {
			continue
		}
		if str(m["name"]) == name && strings.EqualFold(str(m["in"]), in) {
			return m, true
		}
	}
	return nil, false
}

// params 解析 parameters 数组。
func (d *specDoc) params(v any) []ProxyParam {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]ProxyParam, 0, len(list))
	for _, it := range list {
		m := asMap(it)
		if m == nil {
			continue
		}
		p := ProxyParam{
			Name:        str(m["name"]),
			In:          strings.ToLower(str(m["in"])),
			Description: firstLine(str(m["description"])),
		}
		if p.Name == "" {
			continue
		}
		if b, ok := m["required"].(bool); ok {
			p.Required = b
		}
		// OpenAPI3 参数类型在 schema 里；Swagger2 直接平铺
		schema := asMap(m["schema"])
		src := m
		if schema != nil {
			src = schema
		}
		p.Type = typeOf(src)
		p.Format = str(src["format"])
		if def, ok := src["default"]; ok {
			p.Default = def
		}
		if enums, ok := src["enum"].([]any); ok {
			p.Enum = enums
		}
		if p.In == "path" {
			p.Required = true
		}
		out = append(out, p)
	}
	return out
}

// typeOf 推断参数类型：优先 type 字段，其次 items/properties。
func typeOf(m map[string]any) string {
	if t := str(m["type"]); t != "" {
		return t
	}
	if m["properties"] != nil {
		return "object"
	}
	if m["items"] != nil {
		return "array"
	}
	return "string"
}

// resolve 本地 $ref 解析（#/a/b/c 指针）。
func (d *specDoc) resolve(node any, depth int) any {
	if depth > maxSchemaDepth {
		return map[string]any{}
	}
	m, ok := node.(map[string]any)
	if !ok {
		return node
	}
	ref := str(m["$ref"])
	if ref == "" {
		// 递归解析 properties / items / allOf，让 schema 自包含
		if props := asMap(m["properties"]); props != nil {
			for k, v := range props {
				props[k] = d.resolve(v, depth+1)
			}
		}
		if items, ok := m["items"]; ok {
			m["items"] = d.resolve(items, depth+1)
		}
		if all, ok := m["allOf"].([]any); ok {
			for i := range all {
				all[i] = d.resolve(all[i], depth+1)
			}
		}
		return m
	}
	if !strings.HasPrefix(ref, "#/") {
		d.warnings = append(d.warnings, "存在外部 $ref（"+ref+"），已按任意类型处理")
		return map[string]any{}
	}
	cur := any(d.raw)
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		cm := asMap(cur)
		if cm == nil {
			return map[string]any{}
		}
		cur = cm[seg]
	}
	if cur == nil {
		return map[string]any{}
	}
	return d.resolve(cur, depth+1)
}

// schemaJSON 把 schema 收敛成稳定的 JSON Schema 子集（去掉文档里的冗余字段）。
func (d *specDoc) schemaJSON(node any) string {
	s := d.trimSchema(node, 0)
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	if len(b) > 32<<10 { // 单个 schema 上限 32KB
		return `{"type":"object","description":"（schema 过大已省略）"}`
	}
	return string(b)
}

func (d *specDoc) trimSchema(node any, depth int) any {
	if depth > maxSchemaDepth {
		return map[string]any{}
	}
	m, ok := node.(map[string]any)
	if !ok {
		return node
	}
	out := map[string]any{}
	for _, k := range []string{"type", "format", "description", "title", "default", "enum", "example", "nullable", "minItems", "maxItems"} {
		if v, ok := m[k]; ok && v != nil {
			out[k] = v
		}
	}
	// allOf：合并 properties 与 required（常见的 Swagger2 继承写法）
	if all, ok := m["allOf"].([]any); ok && len(all) > 0 {
		merged := map[string]any{"type": "object"}
		props := map[string]any{}
		var required []any
		for _, it := range all {
			sub := asMap(d.trimSchema(d.resolve(it, depth+1), depth+1))
			if sub == nil {
				continue
			}
			if p := asMap(sub["properties"]); p != nil {
				for k, v := range p {
					props[k] = v
				}
			}
			if r, ok := sub["required"].([]any); ok {
				required = append(required, r...)
			}
		}
		if len(props) > 0 {
			merged["properties"] = props
		}
		if len(required) > 0 {
			merged["required"] = required
		}
		out = merged
	}
	if props := asMap(m["properties"]); props != nil {
		np := make(map[string]any, len(props))
		for k, v := range props {
			np[k] = d.trimSchema(v, depth+1)
		}
		out["properties"] = np
		out["type"] = "object"
	}
	if items, ok := m["items"]; ok {
		out["items"] = d.trimSchema(items, depth+1)
		out["type"] = "array"
	}
	if req, ok := m["required"].([]any); ok {
		out["required"] = req
	}
	if _, has := out["type"]; !has {
		out["type"] = "object"
	}
	return out
}

// InputSchema 工具的入参 JSON Schema：参数按位置平铺，请求体放 body 字段
// （body 不摊平：避免和路径/查询参数重名，模型也更容易区分「放哪」）。
func (o ProxyOp) InputSchema() string {
	props := map[string]any{}
	var required []any
	for _, p := range o.Params {
		prop := map[string]any{"type": defaultStr(p.Type, "string")}
		if p.Format != "" {
			prop["format"] = p.Format
		}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		if p.Default != nil {
			prop["default"] = p.Default
		}
		if len(p.Enum) > 0 {
			prop["enum"] = p.Enum
		}
		switch p.In {
		case "path":
			prop["description"] = joinDesc(p.Description, "（路径参数）")
		case "query":
			prop["description"] = joinDesc(p.Description, "（查询参数）")
		case "header":
			prop["description"] = joinDesc(p.Description, "（请求头）")
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	if o.BodySchema != "" {
		var body map[string]any
		if err := json.Unmarshal([]byte(o.BodySchema), &body); err == nil {
			body["description"] = joinDesc(str(body["description"]), "（请求体，JSON）")
			props["body"] = body
			if req, ok := body["required"].([]any); ok && len(req) > 0 {
				// body 必填由 schema 内部的 required 表达，外层不再重复
				_ = req
			}
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	b, _ := json.Marshal(schema)
	return string(b)
}

// Route 工具到 HTTP 请求的映射（落 tools.route）。
type proxyRoute struct {
	Method   string       `json:"method"`
	Path     string       `json:"path"`
	Params   []ProxyParam `json:"params,omitempty"`
	BodyType string       `json:"body_type,omitempty"`
}

// RouteJSON 序列化路由映射。
func (o ProxyOp) RouteJSON(bodyType string) string {
	r := proxyRoute{Method: o.Method, Path: o.Path, BodyType: bodyType, Params: o.Params}
	b, _ := json.Marshal(r)
	return string(b)
}

func decodeProxyRoute(raw string) (*proxyRoute, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: 工具缺少 route 映射（请重新同步该代理）", ErrToolDisabled)
	}
	var r proxyRoute
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("route 映射解析失败: %w", err)
	}
	if r.Method == "" || r.Path == "" {
		return nil, fmt.Errorf("route 映射不完整（缺 method 或 path）")
	}
	return &r, nil
}

// —— 工具名 ——

// opToolName 工具名：优先 operationId，缺失时用 method + path 生成。
// 归一化成 [a-z0-9_]：LLM 调用与能力名前缀（mcp:ns__tool）都要求可解析。
func opToolName(operationID, method, path string) string {
	if s := sanitizeToolName(operationID); s != "" {
		return s
	}
	segs := []string{method}
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		seg = strings.Trim(seg, "{}")
		if seg == "" {
			continue
		}
		segs = append(segs, seg)
	}
	return sanitizeToolName(strings.Join(segs, "_"))
}

func sanitizeToolName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	lastUnderscore := func() bool {
		bs := b.String()
		return len(bs) > 0 && bs[len(bs)-1] == '_'
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			// 驼峰转下划线：getUser → get_user
			if b.Len() > 0 && !lastUnderscore() {
				prev := b.String()[b.Len()-1]
				if prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9' {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r + 32)
		default:
			if b.Len() > 0 && !lastUnderscore() {
				b.WriteByte('_')
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 96 {
		out = out[:96]
	}
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "op_" + out
	}
	return out
}

// —— 小工具 ——

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

func joinDesc(desc, extra string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return strings.TrimPrefix(extra, "（")
	}
	return desc + " " + extra
}

// pickJSONContent 从 requestBody.content 里挑 JSON（或第一个）媒体类型的 schema。
func pickJSONContent(content map[string]any) map[string]any {
	for _, k := range []string{"application/json", "application/*+json"} {
		if m := asMap(content[k]); m != nil {
			if s := asMap(m["schema"]); s != nil {
				return s
			}
		}
	}
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m := asMap(content[k]); m != nil {
			if s := asMap(m["schema"]); s != nil {
				return s
			}
		}
	}
	return nil
}

// loadSpec 读取文档内容：粘贴内容优先，否则带鉴权头抓取 URL。
func (e *proxyEngine) loadSpec(ctx context.Context, s Server, cfg *ProxySpec) (*specDoc, error) {
	text := strings.TrimSpace(cfg.SpecText)
	if text == "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.SpecURL, nil)
		if err != nil {
			return nil, fmt.Errorf("文档地址不合法: %w", err)
		}
		req.Header.Set("Accept", "application/json, application/yaml, text/yaml, */*")
		if err := e.applyAuth(ctx, s, cfg, req); err != nil {
			return nil, err
		}
		resp, err := e.client(cfg).Do(req)
		if err != nil {
			return nil, fmt.Errorf("获取文档失败: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("获取文档失败：HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
		if err != nil {
			return nil, fmt.Errorf("读取文档失败: %w", err)
		}
		if len(body) > maxSpecBytes {
			return nil, fmt.Errorf("文档超过 %d MB 上限", maxSpecBytes>>20)
		}
		text = string(body)
	}
	d, err := parseSpecText([]byte(text))
	if err != nil {
		return nil, err
	}
	if cfg.BaseURL != "" {
		d.baseURL = strings.TrimRight(cfg.BaseURL, "/")
	}
	if d.baseURL == "" {
		return nil, fmt.Errorf("文档没有声明服务地址，请填写 base_url")
	}
	return d, nil
}
