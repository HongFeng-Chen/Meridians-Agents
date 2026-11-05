package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// 一份典型 OpenAPI 3 文档（YAML + $ref + requestBody + path/query 参数）
const openapi3YAML = `
openapi: 3.0.1
info:
  title: 用户中心 API
  version: 1.2.0
servers:
  - url: http://api.internal.example.com/v1
paths:
  /users/{id}:
    get:
      operationId: getUser
      summary: 查询用户
      tags: [user]
      parameters:
        - name: id
          in: path
          required: true
          description: 用户 ID
          schema: {type: string}
        - name: verbose
          in: query
          description: 是否返回详情
          schema: {type: boolean, default: false}
      responses:
        '200': {description: OK}
    put:
      operationId: updateUser
      summary: 更新用户
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/UserPatch'
      responses:
        '200': {description: OK}
  /users:
    post:
      operationId: createUser
      summary: 创建用户
      requestBody:
        content:
          application/json:
            schema: {$ref: '#/components/schemas/UserPatch'}
      responses:
        '200': {description: OK}
components:
  schemas:
    UserPatch:
      type: object
      required: [name]
      properties:
        name: {type: string, description: 姓名}
        age: {type: integer}
        tags:
          type: array
          items: {type: string}
`

// Swagger 2.0（host + basePath + in:body + formData）
const swagger2JSON = `{
  "swagger": "2.0",
  "info": {"title": "订单服务", "version": "2.0.0"},
  "host": "order.example.com",
  "basePath": "/api",
  "schemes": ["http"],
  "paths": {
    "/orders": {
      "post": {
        "operationId": "createOrder",
        "summary": "下单",
        "parameters": [
          {"name": "body", "in": "body", "required": true,
           "schema": {"$ref": "#/definitions/Order"}}
        ]
      }
    },
    "/orders/{orderId}/cancel": {
      "post": {
        "summary": "取消订单",
        "parameters": [
          {"name": "orderId", "in": "path", "required": true, "type": "string"}
        ]
      }
    },
    "/orders/export": {
      "post": {
        "operationId": "exportOrders",
        "parameters": [
          {"name": "format", "in": "formData", "required": true, "type": "string"},
          {"name": "pageSize", "in": "formData", "type": "integer"}
        ]
      }
    }
  },
  "definitions": {
    "Order": {"type": "object", "properties": {"sku": {"type": "string"}}, "required": ["sku"]}
  }
}`

func TestParseOpenAPI3(t *testing.T) {
	doc, err := parseSpecText([]byte(openapi3YAML))
	require.NoError(t, err)
	require.Equal(t, "openapi3", doc.kind)
	require.Equal(t, "用户中心 API", doc.title)
	require.Equal(t, "http://api.internal.example.com/v1", doc.baseURL)
	require.Len(t, doc.ops, 3)

	byName := map[string]ProxyOp{}
	for _, op := range doc.ops {
		byName[op.Name] = op
	}
	get, ok := byName["get_user"]
	require.True(t, ok, "operationId getUser 应归一化为 get_user，实际: %v", keys(byName))
	require.Equal(t, "GET", get.Method)
	require.Equal(t, "/users/{id}", get.Path)
	require.Len(t, get.Params, 2)
	require.True(t, get.Params[0].Required, "path 参数默认必填")
	require.Equal(t, "path", get.Params[0].In)

	// path/query 参数进 properties，请求体放 body
	var schema map[string]any
	require.NoError(t, json.Unmarshal([]byte(get.InputSchema()), &schema))
	props := schema["properties"].(map[string]any)
	require.Contains(t, props, "id")
	require.Contains(t, props, "verbose")
	require.Equal(t, []any{"id"}, schema["required"])

	put := byName["update_user"]
	require.NotEmpty(t, put.BodySchema)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(put.BodySchema), &body))
	// $ref 已解析
	require.Contains(t, body["properties"], "name")
	require.Equal(t, []any{"name"}, body["required"])
	// 入参里有 body 字段
	require.NoError(t, json.Unmarshal([]byte(put.InputSchema()), &schema))
	require.Contains(t, schema["properties"], "body")

	// route 映射可反解
	route, err := decodeProxyRoute(put.RouteJSON("json"))
	require.NoError(t, err)
	require.Equal(t, "PUT", route.Method)
	require.Equal(t, "/users/{id}", route.Path)
}

func TestParseSwagger2(t *testing.T) {
	doc, err := parseSpecText([]byte(swagger2JSON))
	require.NoError(t, err)
	require.Equal(t, "swagger2", doc.kind)
	require.Equal(t, "http://order.example.com/api", doc.baseURL)
	require.Len(t, doc.ops, 3)

	byName := map[string]ProxyOp{}
	for _, op := range doc.ops {
		byName[op.Name] = op
	}
	// 有 operationId 用 operationId；没有则由 method + path 生成
	create, ok := byName["create_order"]
	require.True(t, ok, "实际工具名: %v", keys(byName))
	require.Contains(t, create.BodySchema, "sku")
	// in:body 的 body 参数不重复出现在入参 properties 顶层
	var schema map[string]any
	require.NoError(t, json.Unmarshal([]byte(create.InputSchema()), &schema))
	props := schema["properties"].(map[string]any)
	require.NotContains(t, props, "body_param_only")
	require.Contains(t, props, "body")

	// 缺 operationId → post_orders_orderId_cancel
	cancel, ok := byName["post_orders_order_id_cancel"]
	require.True(t, ok, "实际工具名: %v", keys(byName))
	require.Len(t, cancel.Params, 1)
	require.Equal(t, "orderId", cancel.Params[0].Name)
	require.True(t, cancel.Params[0].Required)

	// formData 合并成请求体
	exportOp, ok := byName["export_orders"]
	require.True(t, ok)
	var form map[string]any
	require.NoError(t, json.Unmarshal([]byte(exportOp.BodySchema), &form))
	require.Contains(t, form["properties"], "format")
	require.Contains(t, form["properties"], "pageSize")
}

func TestParseRejectsUnknownDoc(t *testing.T) {
	_, err := parseSpecText([]byte(`{"foo":"bar"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "无法识别文档版本")

	_, err = parseSpecText([]byte(`openapi: 3.0.0
paths: {}`))
	require.Error(t, err)
}

func TestProxySpecValidate(t *testing.T) {
	// 缺文档来源
	p := &ProxySpec{}
	require.Error(t, p.Validate())

	// login 缺 token_path
	p = &ProxySpec{SpecURL: "https://a/b.json", Auth: ProxyAuth{Type: ProxyAuthLogin, LoginURL: "https://a/login"}}
	require.Error(t, p.Validate())

	// 合法配置：默认超时与 body_type 被补齐
	p = &ProxySpec{SpecURL: "https://a/b.json", Auth: ProxyAuth{Type: ProxyAuthStatic, Token: "t"}}
	require.NoError(t, p.Validate())
	require.Equal(t, 30000, p.Request.TimeoutMS)
	require.Equal(t, "json", p.Request.BodyType)
	require.Equal(t, "Bearer ", p.Auth.Prefix)
}

func TestToolNameSanitize(t *testing.T) {
	cases := map[string]string{
		"getUser":       "get_user",
		"Get_User.Info": "get_user_info",
		"123abc":        "op_123abc",
		"用户查询":          "",
		"a b  c":        "a_b_c",
	}
	for in, want := range cases {
		require.Equal(t, want, sanitizeToolName(in), "输入 %q", in)
	}
}

// —— 执行链路 ——

type capturedReq struct {
	Method        string
	Path          string
	Query         string
	Header        http.Header
	Body          string
	ContentLength int64
}

func newProxyTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, c *capturedReq)) (*httptest.Server, *[]capturedReq) {
	t.Helper()
	var got []capturedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := capturedReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: string(body), ContentLength: r.ContentLength}
		got = append(got, c)
		handler(w, r, &c)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// proxyServerFor 构造测试用代理服务：extra 为追加到配置 JSON 里的字段片段。
func proxyServerFor(t *testing.T, endpoint, extra string) Server {
	t.Helper()
	raw := `{"spec_text":"{}"` + extra + `}`
	cfg, err := DecodeProxySpec(raw)
	require.NoError(t, err)
	encoded, err := EncodeProxySpec(cfg)
	require.NoError(t, err)
	return Server{ID: 1, Name: "p", Namespace: "default", Transport: TransportProxy, Endpoint: endpoint, ProxyConfig: encoded}
}

func TestProxyCallPathQueryBodyAndStaticAuth(t *testing.T) {
	srv, got := newProxyTestServer(t, func(w http.ResponseWriter, r *http.Request, c *capturedReq) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	})
	e := newProxyEngine()
	s := proxyServerFor(t, srv.URL, `,"auth":{"type":"static","token":"secret-token"}`)

	op := ProxyOp{
		Name: "get_user", Method: "GET", Path: "/users/{id}",
		Params: []ProxyParam{{Name: "id", In: "path", Required: true}, {Name: "verbose", In: "query"}},
	}
	tool := Tool{ToolName: "get_user", UniqueName: "default__get_user", Enabled: true, Route: op.RouteJSON("json")}

	res, err := e.Call(context.Background(), s, tool, map[string]any{"id": "u 1", "verbose": true, "extra": "x"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, *got, 1)
	c := (*got)[0]
	require.Equal(t, "GET", c.Method)
	require.Equal(t, "/users/u 1", c.Path) // 空格按 %20 编码后由服务端还原
	require.Contains(t, c.Query, "verbose=true")
	require.Contains(t, c.Query, "extra=x") // 未声明参数 GET 落 query
	require.Equal(t, "Bearer secret-token", c.Header.Get("Authorization"))
}

func TestProxyCallPostBodyAndForm(t *testing.T) {
	srv, got := newProxyTestServer(t, func(w http.ResponseWriter, r *http.Request, c *capturedReq) {
		_, _ = w.Write([]byte(`created`))
	})
	e := newProxyEngine()
	s := proxyServerFor(t, srv.URL, `,"auth":{"type":"none"}`)

	op := ProxyOp{Name: "create_user", Method: "POST", Path: "/users"}
	tool := Tool{ToolName: "create_user", UniqueName: "default__create_user", Enabled: true, Route: op.RouteJSON("json")}
	_, err := e.Call(context.Background(), s, tool, map[string]any{"body": map[string]any{"name": "张三"}})
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"张三"}`, (*got)[0].Body)
	require.Greater(t, (*got)[0].ContentLength, int64(0), "显式 body 必须带 Content-Length，否则部分服务端读不到")
	require.Equal(t, "application/json", (*got)[0].Header.Get("Content-Type"))

	// form 编码 + 未声明参数直接落 body
	s2 := proxyServerFor(t, srv.URL, `,"auth":{"type":"none"},"request":{"body_type":"form"}`)
	tool2 := Tool{ToolName: "export", UniqueName: "default__export", Enabled: true, Route: op.RouteJSON("form")}
	_, err = e.Call(context.Background(), s2, tool2, map[string]any{"format": "csv", "pageSize": 10})
	require.NoError(t, err)
	last := (*got)[len(*got)-1]
	require.Contains(t, last.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
	require.Contains(t, last.Body, "format=csv")
}

func TestProxyCallMissingPathParam(t *testing.T) {
	srv, _ := newProxyTestServer(t, func(w http.ResponseWriter, r *http.Request, c *capturedReq) {})
	e := newProxyEngine()
	s := proxyServerFor(t, srv.URL, `,"auth":{"type":"none"}`)
	op := ProxyOp{Name: "get_user", Method: "GET", Path: "/users/{id}"}
	tool := Tool{ToolName: "get_user", UniqueName: "default__get_user", Enabled: true, Route: op.RouteJSON("json")}
	_, err := e.Call(context.Background(), s, tool, map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "缺少路径参数")
}

func TestProxyLoginTokenAnd401Retry(t *testing.T) {
	var tokenCalls int32
	var apiCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenCalls, 1)
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["username"] != "svc" || in["password"] != "pwd" {
			http.Error(w, `{"msg":"bad"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"tk-1","expires_in":3600}}`))
	})
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&apiCalls, 1)
		// 第一次故意 401，触发换 token 重试
		if n == 1 {
			http.Error(w, `{"msg":"expired"}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tk-1" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"name":"alice"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newProxyEngine()
	cfgJSON := fmt.Sprintf(`{"spec_text":"{}","base_url":"%s/api","auth":{"type":"login","login_url":"%s/login","username":"svc","password":"pwd","token_path":"data.access_token","expire_path":"data.expires_in"}}`, srv.URL, srv.URL)
	s := Server{ID: 7, Name: "p", Namespace: "default", Transport: TransportProxy, Endpoint: srv.URL + "/api", ProxyConfig: cfgJSON}
	tool := Tool{ToolName: "me", UniqueName: "default__me", Enabled: true, Route: ProxyOp{Name: "me", Method: "GET", Path: "/me"}.RouteJSON("json")}

	res, err := e.Call(context.Background(), s, tool, map[string]any{})
	require.NoError(t, err)
	require.False(t, res.IsError, "响应: %s", res.Content[0].Text)
	require.Contains(t, res.Content[0].Text, "alice")
	require.Equal(t, int32(2), atomic.LoadInt32(&tokenCalls), "401 后应刷新一次 token（初次 + 刷新）")
	require.Equal(t, int32(2), atomic.LoadInt32(&apiCalls), "401 后应重试一次")

	// 第二次调用复用缓存 token，不再登录
	_, err = e.Call(context.Background(), s, tool, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&tokenCalls), "第二次调用应复用缓存 token")
}

func TestExtractTokenJSONPath(t *testing.T) {
	raw := []byte(`{"data":{"tokens":[{"value":"abc"}]},"expiresIn":7200}`)
	tk, _, err := extractToken(raw, "data.tokens[0].value", "expiresIn")
	require.NoError(t, err)
	require.Equal(t, "abc", tk)

	_, _, err = extractToken(raw, "data.missing", "")
	require.Error(t, err)
}

func TestBuildToolsFromSpecServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/api-docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(swagger2JSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newProxyEngine()
	s := Server{
		ID: 3, Name: "order", Namespace: "biz", Transport: TransportProxy,
		ProxyConfig: fmt.Sprintf(`{"spec_url":"%s/v3/api-docs"}`, srv.URL),
	}
	tools, cfg, base, err := e.BuildTools(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "http://order.example.com/api", base)
	require.Len(t, tools, 3)
	require.Equal(t, "biz", strings.Split(tools[0].UniqueName, "__")[0], "unique_name 应带命名空间")
	require.Equal(t, 3, len(cfg.Ops), "解析快照应回填到配置")
	require.NotEmpty(t, cfg.FetchedAt)
	require.NotEmpty(t, tools[0].Route)
}

func keys(m map[string]ProxyOp) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
