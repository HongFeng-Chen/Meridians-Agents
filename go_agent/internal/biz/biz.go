package biz

import (
	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/aiplatform"
	"go_agent/internal/biz/conversation"
	"go_agent/internal/biz/embedder"
	"go_agent/internal/biz/history"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/memory"
	"go_agent/internal/biz/platformmcp"
	"go_agent/internal/biz/prompt"
	"go_agent/internal/biz/rag"

	"github.com/google/wire"
)

// ProviderSet is biz providers.
var ProviderSet = wire.NewSet(
	// 提示词工程（模板版本化 + 渲染，RAG/GraphRAG/Memory 共用）
	prompt.New,
	// 提示词工程 LLM 能力（打分/生成/优化）；LLM 端口绑定在 data.ProviderSet
	prompt.NewStudio,
	// LLM 创建 Agent 时把角色提示词沉淀为提示词工程模板（agent.<key>.system）
	wire.Bind(new(agentfactory.PromptSaver), new(*prompt.UseCase)),
	wire.Bind(new(rag.Prompter), new(*prompt.UseCase)),

	// RAG 用例（混合检索问答 + GraphRAG 图谱问答）
	rag.NewRag,

	// 记忆系统（短期 Redis / 长期 MySQL + Milvus 向量召回 + GraphRAG 图谱）
	memory.New,

	// MCP 服务与工具管理（运行时实例 + 工具向量召回）
	// mcp.New 在 data.ProviderSet 里提供（依赖 dialer/client 等三方连接件），
	// 这里只补 Embedder 绑定；history.Recorder 由下方 history.UseCase 实现。
	wire.Bind(new(mcp.Embedder), new(*embedder.EmbeddingClient)),

	// 统一操作历史：MCP 调用 / 技能使用 / 记忆召回 / RAG / GraphRAG 召回
	// 五类记录落 op_histories，消费方经 history.Recorder 注入（可为 nil 不记录）
	history.New,
	wire.Bind(new(history.Recorder), new(*history.UseCase)),

	// 会话域：会话目录 + 消息链路；Chat 由 LLM 自主编排 Agent / MCP 工具，
	// 并自动召回 Memory / RAG / GraphRAG 上下文（依赖在 chat.go ChatDeps）
	conversation.NewUseCase,
	wire.Struct(new(conversation.ChatDeps), "*"),

	// 平台内置 MCP 工具（任务中心 / 会话 / 链路数据，只读）：
	// 直接复用上面三个用例，能力名为 mcp:platform__<tool>
	// 三个数据源接口由 agentfactory / conversation / history 用例实现，
	// 绑定声明在 cmd/go_agent/wire.go（与 aiplatform 的绑定放在一起）。
	platformmcp.New,

	// 外部 AI 平台（千问 / 百炼 MCP 广场）：目录搜索 + 一键接入。
	// 接入动作直接落到 mcp.UseCase（建服务 / 连接 / 同步工具），技能源复用 skill.UseCase 的市场能力。
	// 具体实现（*mcp.UseCase / *skill.UseCase）在 data.ProviderSet 里构造，
	// 因此接口绑定放在 cmd/go_agent/wire.go 的 wire.Build 中统一声明。
	aiplatform.New,

	// 模型客户端
	embedder.NewEmbedding,
	wire.Bind(new(rag.Embedder), new(*embedder.EmbeddingClient)),
	wire.Bind(new(agentfactory.Embedder), new(*embedder.EmbeddingClient)),
	// rag.Chatter 绑定在 data.ProviderSet（指向 llm.Manager 多数据源管理器），
	// llm.Client 作为 conf 兜底实现由 llm.NewManager 内部持有，不在此重复绑定。
)
