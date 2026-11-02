package service

// 契约一致性测试：OpenAPI 里声明的每条 REST 路由都必须由生成代码真实注册到
// HTTP server 上（反过来也不能多）。
//
// 背景：路由表现在完全由 api/*/v1/*.proto 的 google.api.http 注解生成，
// 手写路由已经移除。但仍有两类漂移可能：
//  1. 改了 proto 却忘了 `make api`，生成的 *_http.pb.go 是旧的；
//  2. 新增了 proto 服务却忘了在 internal/server/http.go 里注册。
// 这里用「生成代码 + 空实现」起一个真实 server，把它注册出的路由与
// openapi.yaml（同一份 proto 的产物）逐条比对，两类漂移都能挡住。
//
// 之所以能传 nil：RegisterXxxServiceHTTPServer 只是把 handler 闭包挂到
// 路由上，注册阶段不会调用实现；编译期的接口断言（各 service 文件里的
// var _ v1.XxxHTTPServer = (*XxxService)(nil)）保证方法集完整。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	agentfactoryv1 "go_agent/api/agentfactory/v1"
	aiplatformv1 "go_agent/api/aiplatform/v1"
	conversationv1 "go_agent/api/conversation/v1"
	harnessv1 "go_agent/api/harness/v1"
	historyv1 "go_agent/api/history/v1"
	llmv1 "go_agent/api/llm/v1"
	mcpv1 "go_agent/api/mcp/v1"
	memoryv1 "go_agent/api/memory/v1"
	promptv1 "go_agent/api/prompt/v1"
	ragv1 "go_agent/api/rag/v1"
	skillv1 "go_agent/api/skill/v1"

	khttp "github.com/go-kratos/kratos/v3/transport/http"
	"gopkg.in/yaml.v3"
)

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true,
}

func routeKey(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

// registeredRoutes 用生成代码把各服务的路由挂到一个空 server 上并收集。
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	srv := khttp.NewServer()
	ragv1.RegisterRagServiceHTTPServer(srv, (*RagService)(nil))
	promptv1.RegisterPromptServiceHTTPServer(srv, (*PromptService)(nil))
	memoryv1.RegisterMemoryServiceHTTPServer(srv, (*MemoryService)(nil))
	llmv1.RegisterLLMServiceHTTPServer(srv, (*LLMService)(nil))
	skillv1.RegisterSkillServiceHTTPServer(srv, (*SkillService)(nil))
	mcpv1.RegisterMCPServiceHTTPServer(srv, (*MCPService)(nil))
	agentfactoryv1.RegisterAgentFactoryServiceHTTPServer(srv, (*AgentFactoryService)(nil))
	llmv1.RegisterLLMCallQueryServiceHTTPServer(srv, (*LLMCallQueryService)(nil))
	historyv1.RegisterHistoryServiceHTTPServer(srv, (*HistoryService)(nil))
	conversationv1.RegisterConversationServiceHTTPServer(srv, (*ConversationService)(nil))
	aiplatformv1.RegisterAIPlatformServiceHTTPServer(srv, (*AIPlatformService)(nil))
	harnessv1.RegisterHarnessServiceHTTPServer(srv, (*HarnessService)(nil))

	out := map[string]bool{}
	err := srv.WalkRoute(func(ri khttp.RouteInfo) error {
		out[routeKey(ri.Method, ri.Path)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("遍历已注册路由失败: %v", err)
	}
	return out
}

// declaredRoutes 读取 openapi.yaml 中 /api 前缀的路径与方法。
func declaredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "..", "api", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败（先执行 make api）: %v", path, err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("解析 openapi.yaml 失败: %v", err)
	}
	out := map[string]bool{}
	for p, item := range spec.Paths {
		if !strings.HasPrefix(p, "/api/") {
			continue
		}
		for method := range item {
			if httpMethods[strings.ToLower(method)] {
				out[routeKey(method, p)] = true
			}
		}
	}
	return out
}

func TestProtoRoutesAreRegistered(t *testing.T) {
	impl := registeredRoutes(t)
	declared := declaredRoutes(t)

	if len(declared) < 91 || len(impl) < 91 {
		t.Fatalf("路由数量异常：openapi=%d registered=%d（预期 91+，检查是否漏注册服务）",
			len(declared), len(impl))
	}

	var missing, extra []string
	for r := range declared {
		if !impl[r] {
			missing = append(missing, r)
		}
	}
	for r := range impl {
		if !declared[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("OpenAPI 声明了但服务未注册 %d 条:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("服务注册了但 OpenAPI 未声明 %d 条（是否忘了 make api）:\n  %s", len(extra), strings.Join(extra, "\n  "))
	}
	t.Logf("契约路由校验通过：openapi=%d registered=%d", len(declared), len(impl))
}

// TestPathParamRoutesLast 路由表里静态路径必须先于同层 {param} 注册，
// 否则 /api/prompts/stats 会被 /api/prompts/{key} 抢先匹配。
// gorilla/mux 按注册顺序匹配，这里用注册顺序做断言。
func TestPathParamRoutesLast(t *testing.T) {
	srv := khttp.NewServer()
	ragv1.RegisterRagServiceHTTPServer(srv, (*RagService)(nil))
	promptv1.RegisterPromptServiceHTTPServer(srv, (*PromptService)(nil))
	memoryv1.RegisterMemoryServiceHTTPServer(srv, (*MemoryService)(nil))
	llmv1.RegisterLLMServiceHTTPServer(srv, (*LLMService)(nil))
	skillv1.RegisterSkillServiceHTTPServer(srv, (*SkillService)(nil))
	mcpv1.RegisterMCPServiceHTTPServer(srv, (*MCPService)(nil))
	agentfactoryv1.RegisterAgentFactoryServiceHTTPServer(srv, (*AgentFactoryService)(nil))
	llmv1.RegisterLLMCallQueryServiceHTTPServer(srv, (*LLMCallQueryService)(nil))
	historyv1.RegisterHistoryServiceHTTPServer(srv, (*HistoryService)(nil))
	conversationv1.RegisterConversationServiceHTTPServer(srv, (*ConversationService)(nil))

	var order []string
	if err := srv.WalkRoute(func(ri khttp.RouteInfo) error {
		order = append(order, routeKey(ri.Method, ri.Path))
		return nil
	}); err != nil {
		t.Fatalf("遍历路由失败: %v", err)
	}

	// 同一 method + 同一层级下，出现 {param} 之后不允许再出现静态路径
	seenParam := map[string]bool{} // "GET /api/prompts" -> true
	for _, r := range order {
		parts := strings.SplitN(r, " ", 2)
		method, path := parts[0], parts[1]
		segs := strings.Split(strings.Trim(path, "/"), "/")
		if len(segs) == 0 {
			continue
		}
		prefix := method + " /" + strings.Join(segs[:len(segs)-1], "/")
		last := segs[len(segs)-1]
		if strings.HasPrefix(last, "{") {
			seenParam[prefix] = true
			continue
		}
		if seenParam[prefix] {
			t.Errorf("%s 注册在同层的 {param} 路由之后，会被抢先匹配，请在 proto 中把它声明在更前面", r)
		}
	}
}
