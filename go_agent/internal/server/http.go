package server

import (
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
	"go_agent/internal/conf"
	"go_agent/internal/pkg/httpx"
	"go_agent/internal/service"

	"github.com/go-kratos/kratos/v3/middleware/recovery"
	"github.com/go-kratos/kratos/v3/middleware/validate"
	"github.com/go-kratos/kratos/v3/transport/http"

	"go.einride.tech/aip/fieldbehavior"
	"google.golang.org/protobuf/proto"
)

// NewHTTPServer new an HTTP server.
//
// 所有 REST 路由都由 api/*/v1/*.proto 的 google.api.http 注解生成，
// 通过生成的 RegisterXxxServiceHTTPServer 注册——不再手写路由表，
// 协议即实现，新增接口只需改 proto 再重新生成。
func NewHTTPServer(
	c *conf.Server,
	ragSvc *service.RagService,
	promptSvc *service.PromptService,
	memorySvc *service.MemoryService,
	llmSvc *service.LLMService,
	skillSvc *service.SkillService,
	mcpSvc *service.MCPService,
	afSvc *service.AgentFactoryService,
	llmCallSvc *service.LLMCallQueryService,
	hisSvc *service.HistoryService,
	convSvc *service.ConversationService,
	platformSvc *service.PlatformMCPService,
	aiPlatformSvc *service.AIPlatformService,
	harnessSvc *service.HarnessService,
) *http.Server {
	var opts = []http.ServerOption{
		// multipart 解码器：/api/extract、/api/upload 走文件上传，
		// Kratos 默认解码器不认 multipart/form-data（详见 internal/pkg/httpx）
		http.RequestDecoder(httpx.RequestDecoder),
		http.Middleware(
			recovery.Recovery(),
			validate.Validator(func(req any) error {
				if msg, ok := req.(proto.Message); ok {
					if err := fieldbehavior.ValidateRequiredFields(msg); err != nil {
						return err
					}
				}
				return nil
			}),
		),
	}
	if c.GetHttp().GetNetwork() != "" {
		opts = append(opts, http.Network(c.GetHttp().GetNetwork()))
	}
	if c.GetHttp().GetAddr() != "" {
		opts = append(opts, http.Address(c.GetHttp().GetAddr()))
	}
	if c.GetHttp().GetTimeout() != nil {
		opts = append(opts, http.Timeout(c.GetHttp().GetTimeout().AsDuration()))
	}
	srv := http.NewServer(opts...)

	// /api/health、/api/ingest、/api/query、/api/graph/*、/api/admin/*、/api/upload …
	ragv1.RegisterRagServiceHTTPServer(srv, ragSvc)
	// /api/prompts/**（列表、版本发布、激活回滚、渲染预览）
	promptv1.RegisterPromptServiceHTTPServer(srv, promptSvc)
	// /api/memory/**（短期会话、长期记忆、向量召回、图谱、问答）
	memoryv1.RegisterMemoryServiceHTTPServer(srv, memorySvc)
	// /api/llm/**（数据源 CRUD、默认切换、连通性测试、调试对话）
	llmv1.RegisterLLMServiceHTTPServer(srv, llmSvc)
	// /api/skills/**、/api/skillhub/**（安装启停、文件、市场搜索与下载）
	skillv1.RegisterSkillServiceHTTPServer(srv, skillSvc)
	// /api/mcp/**（服务配置、连接探活、工具同步、语义召回、工具调用）
	mcpv1.RegisterMCPServiceHTTPServer(srv, mcpSvc)
	// /api/agentfactory/**（Agent 定义、LLM 造 Agent、任务提交与 trace 链路）
	agentfactoryv1.RegisterAgentFactoryServiceHTTPServer(srv, afSvc)
	// /api/llm/calls/**（LLM 调用历史：token 用量、任务关联、按模型聚合）
	llmv1.RegisterLLMCallQueryServiceHTTPServer(srv, llmCallSvc)
	// /api/histories/**（统一操作历史：MCP 调用 / 技能使用 / 记忆召回 / RAG / GraphRAG 召回）
	historyv1.RegisterHistoryServiceHTTPServer(srv, hisSvc)
	// /api/conversations/**（会话目录 + 消息链路，会话 id = task id）
	conversationv1.RegisterConversationServiceHTTPServer(srv, convSvc)
	// /api/aiplatform/**（外部 AI 平台：千问/百炼 MCP 广场搜索与一键接入）
	aiplatformv1.RegisterAIPlatformServiceHTTPServer(srv, aiPlatformSvc)
	harnessv1.RegisterHarnessServiceHTTPServer(srv, harnessSvc)
	// /mcp/platform（平台内置 MCP 端点：任务中心 / 会话 / 链路数据，只读、仅本机）
	registerPlatformMCP(srv, platformSvc)
	return srv
}
