package server

import (
	nethttp "net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	adminv1 "go_agent/api/admin/v1"
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
	"go_agent/internal/biz/audit"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/health"
	"go_agent/internal/pkg/httpx"
	"go_agent/internal/pkg/mediaauth"
	"go_agent/internal/pkg/metrics"
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
	sec *conf.Security,
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
	adminSvc *service.AdminService,
	auditUC *audit.UseCase,
) *http.Server {
	g := newGate(sec)
	warnInsecureExposure(sec, c.GetHttp().GetAddr())
	configureMediaAuth(sec)
	var opts = []http.ServerOption{
		// multipart 解码器：/api/extract、/api/upload 走文件上传，
		// Kratos 默认解码器不认 multipart/form-data（详见 internal/pkg/httpx）
		http.RequestDecoder(httpx.RequestDecoder),
		// 中间件顺序：越靠外越先执行。
		// recovery 兜 panic → accessLog/metrics 观测全部请求 →
		// auth 定身份 → ratelimit 按租户计量 → validate 校验必填字段。
		http.Middleware(
			recovery.Recovery(),
			accessLog(),
			metricsMiddleware(metrics.Default),
			g.middleware(),
			g.rateLimit(),
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
	// /api/admin/tenants、/api/admin/users（系统管理：多租户 + 成员名册）
	adminv1.RegisterAdminServiceHTTPServer(srv, adminSvc)
	// /mcp/platform（平台内置 MCP 端点：任务中心 / 会话 / 链路数据，只读、仅本机）
	registerPlatformMCP(srv, platformSvc)
	// /api/mcp/tool-approvals/**（高危工具调用的审批：列表 / 批准 / 驳回）、
	// /api/admin/audits（审计事件查询）——运维面接口，管理员限定
	registerOpsRoutes(srv, mcpSvc, auditUC)
	// /api/files/**（聊天附件静态访问：data/uploads 下的图片 / 视频）
	// 这条路由是原始 net/http 注册的，不走上面的认证中间件——鉴权在 handler 里做
	srv.HandlePrefix("/api/files/", filesHandler(g))
	// /healthz（MySQL / Redis / Milvus 探活，供 LB 摘流）、/metrics（Prometheus 抓取）
	srv.Handle("/healthz", health.Handler())
	srv.Handle("/metrics", metrics.Default.Handler())
	return srv
}

// configureMediaAuth 附件地址签名。
//
// 认证开启时用 JWT 密钥给附件地址签名；没配 JWT 密钥（纯静态令牌部署）
// 就用进程内随机密钥——地址在读取消息时现签，重启后重新签，不影响历史消息。
func configureMediaAuth(sec *conf.Security) {
	a := sec.GetAuth()
	if !a.GetEnabled() {
		mediaauth.Configure("") // 开发态不签名：打开就能看图
		return
	}
	if key := strings.TrimSpace(a.GetJwtSecret()); key != "" {
		mediaauth.Configure(key)
		return
	}
	mediaauth.ConfigureRandom()
}

// filesHandler 聊天附件静态文件服务。
// 只暴露 data/uploads 目录；http.FileServer 自带路径穿越防护；
// 目录请求一律 404（不提供目录列表），未知扩展名按二进制流返回。
//
// 鉴权（认证开启时）：带有效签名（服务端读消息时补上）或带有效令牌才放行。
// <img src>/<video src> 带不了 Authorization 头，所以令牌也可以走 ?token=。
func filesHandler(g *gate) nethttp.Handler {
	fs := nethttp.FileServer(nethttp.Dir(mediaRootDir))
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/api/files/")
		if rel == "" || strings.HasSuffix(rel, "/") {
			nethttp.NotFound(w, r)
			return
		}
		if !g.allowMedia(r) {
			// 不区分「没签名」和「签名不对」，避免给枚举者反馈
			w.Header().Set("Cache-Control", "no-store")
			nethttp.Error(w, "未认证：附件地址需要签名或有效令牌", nethttp.StatusUnauthorized)
			return
		}
		if _, err := os.Stat(filepath.Join(mediaRootDir, filepath.FromSlash(path.Clean(rel)))); err != nil {
			nethttp.NotFound(w, r)
			return
		}
		// Content-Type 由扩展名推断（Windows 上 mime 注册可能缺失，这里兜底常见类型）
		ext := strings.ToLower(path.Ext(rel))
		if ct := mediaMimeTypes[ext]; ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		// 附件按会话轮次基本不变，允许浏览器缓存 1 天
		w.Header().Set("Cache-Control", "public, max-age=86400")
		nethttp.StripPrefix("/api/files/", fs).ServeHTTP(w, r)
	})
}

// mediaRootDir 附件的磁盘根目录（与 biz/conversation 的 mediaRoot 一致；测试里会改）
var mediaRootDir = "data/uploads"

// mediaMimeTypes 附件常见扩展名 → Content-Type（与 biz/conversation 白名单一致）
var mediaMimeTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".m4v": "video/x-m4v",
}
