package data

import (
	"os"
	"strconv"
	"strings"
	"time"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/aiplatform"
	"go_agent/internal/biz/conversation"
	"go_agent/internal/biz/harness"
	"go_agent/internal/biz/history"
	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/memory"
	"go_agent/internal/biz/prompt"
	"go_agent/internal/biz/rag"
	"go_agent/internal/biz/resource"
	"go_agent/internal/biz/skill"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	dbrepo "go_agent/internal/data/db/repo"
	"go_agent/internal/data/harnessrunner"
	"go_agent/internal/data/image_generations"
	"go_agent/internal/data/milvusdb"
	mvrepo "go_agent/internal/data/milvusdb/repo"
	"go_agent/internal/data/redis"
	aiplatformclient "go_agent/internal/data/third_service/aiplatform"
	"go_agent/internal/data/third_service/mcpclient"
	"go_agent/internal/data/third_service/skillhub"
	"go_agent/internal/data/video_generations"

	"github.com/google/wire"
)

// ProviderSet is data providers.
//
// 存储分层（每种存储：连接在根包，model 与 repo 分离）：
//   - db      ：MySQL   —— db（连接）/ db/models（PO）/ db/repo（仓储）
//   - redis   ：Redis   —— redis（连接 + 各业务 cache）
//   - milvusdb：Milvus  —— milvusdb（连接）/ milvusdb/models（集合 Schema）/ milvusdb/repo（Store）
//
// 说明：esdb / clickhousedb 暂未接入 wire——它们的构造函数会在启动时强制连接
// 本机的 ES / ClickHouse，未部署时会导致整个进程启动失败。需要时再显式加入。
var ProviderSet = wire.NewSet(
	db.NewData,
	redis.NewRDBData,

	// 向量库：一条 gRPC 连接同时服务主集合、图谱集合与记忆集合
	milvusdb.New,
	mvrepo.NewChunkStore,
	mvrepo.NewGraphStore,
	mvrepo.NewMemoryStore,

	// MySQL 文档元数据 + Redis 缓存/锁
	dbrepo.NewDocumentRepo,
	// 知识空间与统一资源（V2）：空间树/共享、资源元数据/授权/版本
	dbrepo.NewSpaceRepo,
	dbrepo.NewResourceRepo,
	dbrepo.NewUsageRepo,
	redis.NewAnswerCache,
	wire.Bind(new(rag.AnswerCache), new(*redis.AnswerCache)),
	wire.Bind(new(rag.Locker), new(*redis.AnswerCache)),

	// 提示词工程：MySQL 存模板与版本，Redis 缓存生效版本
	dbrepo.NewPromptRepo,
	redis.NewPromptCache,
	wire.Bind(new(prompt.Cache), new(*redis.PromptCache)),

	// 记忆系统：MySQL 长期记忆 + Redis 短期会话 + Milvus 记忆向量
	// （NewMemoryRepo / NewMemoryStore 直接返回 biz 接口类型，无需额外 Bind）
	dbrepo.NewMemoryRepo,
	redis.NewShortTermStore,
	wire.Bind(new(memory.ShortTermStore), new(*redis.ShortTermStore)),

	// 大模型多数据源：MySQL 存数据源配置，Manager 实现 rag.Chatter 供业务使用；
	// llm_calls 记录每次调用的 token 用量/耗时/模型，异步写入
	dbrepo.NewLLMProviderRepo,
	dbrepo.NewLLMCallRepo,
	wire.Bind(new(llm.CallRepo), new(*dbrepo.LLMCallRepo)),
	// 成本中心：LLM 调用结束把 tokens/成本写进 usage_records（账单，长期保留）
	dbrepo.NewCostSink,
	wire.Bind(new(llm.CostSink), new(*dbrepo.CostSink)),
	llm.NewManager,
	wire.Bind(new(rag.Chatter), new(*llm.Manager)),
	// 媒体生成（文生图 / 图生图 / 视频生成）：image_generations / video_generations
	// 记录表 + 厂商协议适配，MediaManager 复用 Manager 的数据源路由
	image_generations.NewImageRepo,
	image_generations.NewImageClient,
	wire.Bind(new(llm.ImageGenClient), new(*image_generations.ImageClient)),
	video_generations.NewVideoRepo,
	video_generations.NewVideoClient,
	wire.Bind(new(llm.VideoGenClient), new(*video_generations.VideoClient)),
	llm.NewMediaManager,
	wire.Bind(new(agentfactory.MediaPort), new(*llm.MediaManager)),
	// 提示词工程（打分/生成/优化）同样复用 llm.Manager，可指定数据源
	wire.Bind(new(prompt.LLM), new(*llm.Manager)),

	// 技能管理：MySQL 存技能与市场，skillhub 为技能市场 HTTP 客户端
	dbrepo.NewSkillRepo,
	skillhub.New,
	wire.Bind(new(skill.HubClient), new(*skillhub.Client)),
	skill.New,

	// 审计事件：audit_events 表（授权判定 / 管理面变更 / 高危工具调用），异步批量写入
	dbrepo.NewAuditRepo,

	// 统一操作历史：op_histories 表，五类「用到了什么」的记录
	// （MCP 调用 / 技能使用 / 记忆召回 / RAG 召回 / GraphRAG 召回），异步写入
	dbrepo.NewHistoryRepo,
	wire.Bind(new(history.Repo), new(*dbrepo.HistoryRepo)),

	// 会话域：conversations / conversation_messages 两表，目录 + 消息链路
	dbrepo.NewConversationRepo,
	wire.Bind(new(conversation.Repo), new(*dbrepo.ConversationRepo)),

	// MCP 服务与工具：MySQL 存配置与元数据，Milvus 存工具向量，
	// Redis 缓存工具对象与运行状态；客户端按 transport 动态选择，
	// http 模式的 *http.Client 由 Provider 按实例参数构造，不落库。
	dbrepo.NewMCPRepo,
	mvrepo.NewToolStore,
	redis.NewMCPCache,
	wire.Bind(new(mcp.Cache), new(*redis.MCPCache)),
	mcpclient.NewDialer,
	wire.Bind(new(mcp.Dialer), new(*mcpclient.Dialer)),
	mcpclient.NewHTTPClientProvider,
	wire.Bind(new(mcp.ClientProvider), new(*mcpclient.HTTPClientProvider)),
	mcp.New,

	// AgentFactory：Agent 定义存 MySQL，向量注册表存 Milvus，Redis 缓存定义与任务快照 + 任务队列；
	// LLM 走 llm.Manager（Chatter），MCP 工具能力复用 mcp.UseCase（ToolPort），
	// 向量化复用 embedder.EmbeddingClient。异步任务由 worker 池消费 Redis 队列。
	dbrepo.NewAgentFactoryRepo,
	mvrepo.NewAgentStore,
	redis.NewAgentFactoryCache,
	wire.Bind(new(agentfactory.Cache), new(*redis.AgentFactoryCache)),
	redis.NewTaskLock,
	wire.Bind(new(agentfactory.TaskLock), new(*redis.TaskLock)),
	NewAgentFactoryOptions,
	wire.Bind(new(agentfactory.ToolPort), new(*mcp.UseCase)),
	wire.Bind(new(agentfactory.Chatter), new(*llm.Manager)),
	// 执行体注册表（worker_instances）：多副本部署时靠心跳判定失联、回收任务
	dbrepo.NewWorkerRepo,
	NewAgentFactoryUseCase,

	// 评测 Harness：四张表存 MySQL（sets/cases/runs/run_items），
	// 被测对象执行器按 target 分派——agent: 走 agentfactory.RunOnce（同步执行一次），
	// model: 走 llm.Manager 直连；LLM Judge 复用 llm.Manager（Chatter）。
	dbrepo.NewHarnessRepo,
	harnessrunner.New,
	wire.Bind(new(harness.Runner), new(*harnessrunner.Runner)),
	wire.Bind(new(harness.Chatter), new(*llm.Manager)),
	harness.New,

	// 外部 AI 平台（千问 / 百炼 / 自建 MCP 广场）：配置存 MySQL，
	// 目录接口走 aiplatform HTTP 客户端；接入动作复用 mcp.UseCase，不重复实现。
	dbrepo.NewAIPlatformRepo,
	aiplatformclient.New,
	wire.Bind(new(aiplatform.Catalog), new(*aiplatformclient.Client)),

	// 系统管理：tenants / users 两表（租户隔离域 + 成员名册）。
	// 只做管理不参与认证：身份仍来自 JWT / 静态令牌。
	dbrepo.NewOrgRepo,
)

// NewAgentFactoryOptions 从配置读 worker 池与护栏参数。
//
// 执行链路参数（租约 / 心跳 / 超时 / 重试）走环境变量而不是 proto：
// 它们是需要按部署形态调整的运维旋钮（k8s 里活不过一次滚动重启），
// 而且几乎不随版本变化，进配置协议只会让 proto 与前端配置页一起膨胀。
func NewAgentFactoryOptions(c *conf.Data, sec *conf.Security) agentfactory.Options {
	af := c.GetAgentfactory()
	b := sec.GetBudget()
	return agentfactory.Options{
		WorkerCount:     int(af.GetWorkerCount()),
		MaxAgents:       int(af.GetMaxAgents()),
		DefaultMaxTurns: int(af.GetDefaultMaxTurns()),
		ShutdownTimeout: af.GetShutdownTimeout().AsDuration(),
		MaxParallel:     int(b.GetMaxParallel()),
		BudgetTokens:    b.GetMaxTokens(),
		BudgetCalls:     b.GetMaxCalls(),

		LeaseTTL:           envDuration("AGENT_LEASE_TTL", 60*time.Second),
		HeartbeatInterval:  envDuration("AGENT_HEARTBEAT_INTERVAL", 15*time.Second),
		HeartbeatTTL:       envDuration("AGENT_HEARTBEAT_TTL", 45*time.Second),
		TaskTimeout:        envDuration("AGENT_TASK_TIMEOUT", 30*time.Minute),
		SweepInterval:      envDuration("AGENT_SWEEP_INTERVAL", 20*time.Second),
		MaxAttempts:        envIntValue("AGENT_TASK_MAX_ATTEMPTS", 3),
		WorkerKey:          strings.TrimSpace(os.Getenv("AGENT_WORKER_KEY")),
		MaxDelegationDepth: envIntValue("AGENT_MAX_DELEGATION_DEPTH", agentfactory.MaxDelegationDepth),
	}
}

// NewAgentFactoryUseCase 生产装配：在基础依赖之外注入执行体注册表。
// 单独包一层是为了让 agentfactory.New 保持 10 参数签名——单测大量直接构造用例，
// 不想为了一个可选端口把所有测试的构造调用都改一遍。
func NewAgentFactoryUseCase(repo agentfactory.Repo, vec agentfactory.VectorStore, cache agentfactory.Cache, lock agentfactory.TaskLock, chat agentfactory.Chatter, emb agentfactory.Embedder, tools agentfactory.ToolPort, skills agentfactory.SkillPort, prompts agentfactory.PromptSaver, media agentfactory.MediaPort, workers agentfactory.WorkerRepo, res *resource.UseCase, opt agentfactory.Options) (*agentfactory.UseCase, error) {
	uc, err := agentfactory.NewWithWorkers(repo, vec, cache, lock, chat, emb, tools, skills, prompts, media, workers, opt)
	if err != nil {
		return nil, err
	}
	// 运行时二次授权：任务执行 / 工具调用 / 委派前重新按当前授权判权。
	// 注入失败不影响构造（nil 即关闭），因此这里只需直接设置。
	uc.SetResourceAuthorizer(res)
	return uc, nil
}

// envDuration 读时长型环境变量（"90s" / "5m"），解析失败或缺失走默认值
func envDuration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// envIntValue 读整型环境变量，非法值走默认
func envIntValue(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > agentfactory.MaxAttemptsLimit {
		return agentfactory.MaxAttemptsLimit
	}
	return n
}
