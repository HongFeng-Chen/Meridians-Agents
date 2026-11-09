# api · 接口契约

本目录是**对外接口的唯一定义源**。所有 HTTP 接口都由这里的 `.proto` 描述，
生成物供三方使用：

```
api/<domain>/v1/*.proto          ← 手写：路径、请求/响应结构、字段名、注释
        │  buf generate（make api）
        ├─→ *_pb.go               ← 消息类型（Go 服务端 / 其他 Go 服务可直接引用）
        ├─→ *_grpc.pb.go          ← gRPC 桩（服务端接口 + 客户端）
        ├─→ *_http.pb.go          ← Kratos v3 HTTP 路由表 + 客户端
        └─→ api/openapi.yaml      ← OpenAPI 3（11 个已接线 service 合并，上传接口由脚本补成 multipart）
                 │  node scripts/gen-sdk.mjs（make sdk）
                 └─→ frontend/src/api/generated/   ← 前端 SDK（function + JSDoc 类型）
```

- 服务端实现是 `internal/service/*.go`：**直接实现生成的服务接口**，再由
  `internal/server/http.go` / `grpc.go` 用 `RegisterXxxHTTPServer` / `RegisterXxxGRPCServer`
  注册。同一份实现同时对外提供 REST 与 gRPC，没有手写路由表，协议即实现。
- 契约与实现的一致性由两个层面保证：编译期（各 service 文件里的
  `var _ v1.XxxHTTPServer = (*XxxService)(nil)` 断言）+ 测试期
  （`internal/service/contract_test.go` 把「生成代码注册的路由」与
  `api/openapi.yaml` 逐条比对）。
- 前端不再手写 URL：`frontend/src/api.js` 只做旧签名兼容，页面统一调用
  `rag / prompt / memory / llm / skill / mcp / agentfactory / conversation / history / aiplatform / harness`
  十个命名空间。

> 例外：`/mcp/platform` 是平台内置的 **MCP 协议端点**（JSON-RPC 2.0，Streamable HTTP），不是 REST，
> 因此**不在 proto / openapi.yaml 里**。它的工具定义在 `internal/biz/platformmcp`，协议层在
> `internal/server/mcp.go`，详见 `internal/server/MCP网关方案.md`。

## 目录

| 包 | 域 | 对应业务代码 | 操作数 |
| --- | --- | --- | --- |
| `rag.v1` | RAG + GraphRAG + 文档管理 | `internal/biz/rag`、`biz/rag/graphrag` | 11 |
| `prompt.v1` | 提示词模板与版本 + 提示词工程 + 版本评测得分 | `internal/biz/prompt` | 18 |
| `memory.v1` | 短期/长期记忆、召回问答 | `internal/biz/memory` | 18 |
| `llm.v1`（`LLMService`） | 大模型数据源管理 | `internal/biz/llm` | 14 |
| `llm.v1`（`LLMCallQueryService`） | 调用历史、token 用量与下钻排行 | `internal/biz/llm` | 3 |
| `skill.v1` | 技能与技能市场 | `internal/biz/skill` | 21 |
| `harness.v1` | 评测 Harness（用例集 / 用例 / 运行 / 报告） | `internal/biz/harness` | 11 |
| `mcp.v1` | MCP 服务与工具管理面 | `internal/biz/mcp` | 22 |
| `agentfactory.v1` | Agent 低代码工厂 + 异步任务 + 审批 | `internal/biz/agentfactory` | 19 |
| `conversation.v1` | 会话目录 + 消息 + 辩论 | `internal/biz/conversation` | 16 |
| `history.v1` | 统一操作历史 | `internal/biz/history` | 2 |
| `aiplatform.v1` | 外部 AI 平台（千问 / 百炼 MCP 广场） | `internal/biz/aiplatform` | 8 |
| `todo.v1` | Kratos 脚手架样例（**未接线**，不参与契约校验） | — | 7 |

合计 **163** 个对外操作（不含 `todo.v1`），全部挂在 `/api` 下；另有 1 个非 REST 端点 `/mcp/platform`。

## 约定

**1. 字段名即 JSON 键（snake_case）**
每个字段都显式声明 `json_name`，例如 `string doc_id = 1 [json_name = "doc_id"];`。
后端用 `encoding/json` + Go 的 snake_case tag 输出，protojson 也按该名字序列化，
两端不需要任何命名转换。新增字段时**必须**同样声明 `json_name`。

**2. 只增字段的语义约定（近期新增）**

- `IngestResult.purged` / `UploadAndIngestReply.purged`：同 `doc_id` 重复入库时被顶掉的旧分块数。
  默认 0 = 旧行为（首次入库），前端可据此提示「已替换旧版本」。
- `MCPServer.has_credentials`：服务是否配置了凭证。读接口**不再回显** `headers`/`env`/`proxy_config`
  的真实值，只给这个布尔；更新时传回掩码字面量表示「保持原值」。
- 任务 / 文档 / 操作历史 / LLM 调用记录的列表与统计都按 `(tenant_id, user_id)` 过滤：
  同一个 `user_id` 在不同租户下互不可见。管理端看全量需要 `audit:read` 权限
  （`memories` 目前仍只有 `user_id`，见 HANDOVER 5.5）。

**3. 类型映射的已知偏差**

| proto | 实际 JSON | 说明 |
| --- | --- | --- |
| `int32` | number | 计数、下标、维度 |
| `int64` | number | `id`、`duration`（纳秒）。proto3 的 JSON 规范会把它序列化成字符串，本项目后端输出的是数字，前端按 number 处理 |
| `string`（时间） | RFC3339 字符串 | 与 Go `time.Time` 的 JSON 一致，不用 `google.protobuf.Timestamp` |
| `optional string` | string \| null | 如 `expires_at`、`activate`，用于「不传」与「传空」的语义区分 |

**4. multipart 上传**
`ExtractText` / `UploadAndIngest` 同时支持两种调用方式：

| 调用方式 | 文件怎么传 | 其余字段 |
| --- | --- | --- |
| HTTP | `multipart/form-data`，文件放 `file` 部分 | query 参数（`mode` / `doc_id` / `source`） |
| gRPC | `bytes file` + `string name` | 消息里的同名字段 |

proto 里写的是 `bytes file`（gRPC 语义正确），`body: "file"` 让生成代码把请求体
绑到该字段；HTTP 侧的解析由 `internal/pkg/httpx.RequestDecoder` 完成（Kratos 默认
解码器不认 multipart/form-data），它把文件写进字段并挂到请求 ctx 上供 handler 取文件名。

`protoc-gen-openapi` 不支持 `bytes`，会把请求体降级成空 JSON，因此 `make api` 末尾
会跑 `scripts/patch_openapi.py`，把这两条接口的契约补成真实的 `multipart/form-data`
（`file: type string, format binary`），前端 SDK 据此产出 `FormData` 调用。

**5. 无 body 的 POST**
启停、设为默认、回滚、清理过期等操作没有请求体，契约里不写 `body`，
参数全部走 query/path。gnostic 会给出 `WARN: POST ... does not declare a body`，
属预期。

**6. 路由声明顺序**
gorilla/mux 按注册顺序匹配，同层里静态路径必须排在 `{param}` 之前，否则
`/api/prompts/stats` 会被 `/api/prompts/{key}` 抢走。因此 proto 里要把静态路径的 rpc
写在前面（`contract_test.go` 的 `TestPathParamRoutesLast` 会守住这条规则）。

**7. 错误**
handler 返回 Kratos error，由 ErrorEncoder 统一输出：

| 场景 | HTTP | body |
| --- | --- | --- |
| 参数不合法 | 400 | `{"code":400,"reason":"INVALID_ARGUMENT","message":"..."}` |
| 资源不存在 | 404 | `{"code":404,"reason":"NOT_FOUND","message":"..."}` |
| 冲突（重名） | 409 | `{"code":409,"reason":"ALREADY_EXISTS","message":"..."}` |
| 内部错误 | 500 | `{"code":500,"reason":"INTERNAL","message":"..."}` |

同一份 error 经 gRPC 返回时是等价的 status code（404 → `NotFound`）。
前端 `ApiError` 读 `message` 展示，`status` 用于分支判断。
「必填校验」由 `fieldbehavior` 中间件兜底，缺字段时 `reason` 为 `VALIDATOR`。

## 注册方式（HTTP + gRPC）

每个 proto service 会生成两套服务端接口和两个注册函数：

```go
// 生成的 *_http.pb.go
type RagServiceHTTPServer interface { Health(context.Context, *HealthRequest) (*HealthReply, error); ... }
func RegisterRagServiceHTTPServer(s *http.Server, srv RagServiceHTTPServer)

// 生成的 *_grpc.pb.go
type RagServiceServer interface { Health(context.Context, *HealthRequest) (*HealthReply, error); ... }
func RegisterRagServiceServer(s *grpc.Server, srv RagServiceServer)
```

`internal/service/<domain>_service.go` 用同一个结构体把两者都实现掉
（嵌入 `v1.UnimplementedXxxServiceServer` 满足 gRPC 的前向兼容要求），
再用编译期断言防止方法集漏实现：

```go
var (
    _ v1.RagServiceHTTPServer = (*RagService)(nil)
    _ v1.RagServiceServer     = (*RagService)(nil)
)
```

装配在 `internal/server` 里完成，两条传输层各自注册一遍：

```go
// internal/server/http.go
srv := http.NewServer(opts...)
ragv1.RegisterRagServiceHTTPServer(srv, ragSvc)
promptv1.RegisterPromptServiceHTTPServer(srv, promptSvc)
...

// internal/server/grpc.go
srv := grpc.NewServer(opts...)
ragv1.RegisterRagServiceServer(srv, ragSvc)
promptv1.RegisterPromptServiceServer(srv, promptSvc)
...
```

好处是三种调用方式行为一致：

| 入口 | 例子 |
| --- | --- |
| REST | `GET /api/rag/...`、`POST /api/upload` |
| gRPC | `rag.v1.RagService/Health`（`:9000`，`grpc.NewClient` + 生成的客户端） |
| Go 进程内 | 直接持有 `*service.RagService` 调用方法 |

新增一个 service 的注册成本只有两步：proto 里声明 `service` + `make api`，
然后在 `internal/server/{http,grpc}.go` 各加一行 `RegisterXxx`。

## 操作清单

> 由 `api/openapi.yaml` 自动汇总，与 proto 一一对应。

### LLMService

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| POST | `/api/llm/chat` | Chat | 调试对话（管理端手动调用） |
| GET | `/api/llm/current` | GetCurrent | 当前生效的数据源与模型 |
| GET | `/api/llm/providers` | ListProviders | 数据源列表 |
| POST | `/api/llm/providers` | CreateProvider | 新建数据源 |
| DELETE | `/api/llm/providers/{name}` | DeleteProvider | 删除数据源（唯一启用的数据源不允许删除） |
| GET | `/api/llm/providers/{name}` | GetProvider | 数据源详情 |
| PATCH | `/api/llm/providers/{name}` | UpdateProvider | 修改数据源 |
| POST | `/api/llm/providers/{name}/default` | SetDefaultProvider | 设为默认数据源 |
| POST | `/api/llm/providers/{name}/disable` | DisableProvider | 停用数据源 |
| POST | `/api/llm/providers/{name}/enable` | EnableProvider | 启用数据源 |
| GET | `/api/llm/providers/{name}/models` | ListRemoteModels | 拉取远端 /models（不落库，供前端勾选） |
| POST | `/api/llm/providers/{name}/test` | TestProvider | 连通性测试（会真实发起一次对话） |
| GET | `/api/llm/scenes` | ListScenes | 内置场景清单 |
| GET | `/api/llm/stats` | GetLLMStats | 数据源规模 |

### MemoryService

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| POST | `/api/memory/answer` | Answer | 基于记忆的多轮问答 |
| GET | `/api/memory/info` | GetMemoryInfo | 运行参数 |
| GET | `/api/memory/long` | ListLong | 长期记忆列表 |
| POST | `/api/memory/long` | Remember | 写入长期记忆（向量化 + 可选图谱抽取） |
| POST | `/api/memory/long/purge` | PurgeExpired | 清理已过期记忆 |
| GET | `/api/memory/long/stats` | GetMemoryStats | 规模统计 |
| DELETE | `/api/memory/long/{id}` | DeleteLong | 删除单条长期记忆（含向量与关联图谱） |
| GET | `/api/memory/long/{id}` | GetLong | 单条长期记忆 |
| POST | `/api/memory/recall` | Recall | 向量 + 图谱召回 |
| DELETE | `/api/memory/short` | ClearShort | 清空会话（删除 Redis key） |
| GET | `/api/memory/short` | ListShort | 读取会话历史 |
| POST | `/api/memory/short` | AppendShort | 追加一条会话消息 |

### PromptService

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| GET | `/api/prompts` | ListPrompts | 模板列表 |
| POST | `/api/prompts` | CreatePrompt | 新建模板并写入首个版本（默认立即生效） |
| GET | `/api/prompts/builtins` | ListBuiltins | 代码内置模板清单 |
| GET | `/api/prompts/stats` | GetPromptStats | 模板与版本数量 |
| DELETE | `/api/prompts/{key}` | DeletePrompt | 删除模板（内置模板不可删除，用 ResetPrompt 恢复） |
| GET | `/api/prompts/{key}` | GetPrompt | 模板详情（含当前生效版本） |
| PATCH | `/api/prompts/{key}` | UpdatePrompt | 修改模板元信息 |
| POST | `/api/prompts/{key}/activate` | ActivatePrompt | 激活指定版本 |
| POST | `/api/prompts/{key}/render` | RenderPrompt | 渲染预览（不调用大模型） |
| POST | `/api/prompts/{key}/reset` | ResetPrompt | 恢复内置默认内容（追加为新版本） |
| POST | `/api/prompts/{key}/rollback` | RollbackPrompt | 回滚到上一个版本 |
| GET | `/api/prompts/{key}/versions` | ListVersions | 版本历史 |
| POST | `/api/prompts/{key}/versions` | PublishVersion | 发布新版本 |
| GET | `/api/prompts/{key}/versions/{version}` | GetVersion | 查询指定版本 |
| POST | `/api/prompts/studio/score` | ScorePrompt | 提示词打分（LLM：五维评分 + 问题 + 建议） |
| POST | `/api/prompts/studio/generate` | GeneratePrompt | 按需求描述生成角色提示词（LLM） |
| POST | `/api/prompts/studio/optimize` | OptimizePrompt | 优化提示词（LLM：改后稿 + 改动说明 + 前后分） |

### RagService

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| DELETE | `/api/admin/docs` | DeleteDoc | 删除文档（向量分块 + 摘图谱引用 + 元数据 + 缓存） |
| GET | `/api/admin/docs` | ListDocs | 文档列表 |
| GET | `/api/admin/stats` | GetStats | 系统概览 |
| POST | `/api/extract` | ExtractText | 解析文件为纯文本，不入库（前端预览/编辑用） |
| GET | `/api/formats` | ListFormats | 支持的文件格式清单 |
| POST | `/api/graph/ingest` | GraphIngest | 图谱构建：LLM 抽取实体与关系后入图集合 |
| POST | `/api/graph/query` | GraphQuery | 图谱问答：实体召回 + 多跳扩展 + 原文证据 |
| GET | `/api/health` | Health | 服务健康检查与运行参数 |
| POST | `/api/ingest` | Ingest | 文本入库：分块 -> 向量化 -> Milvus -> MySQL 元数据 -> 清缓存 |
| POST | `/api/query` | Query | 混合检索问答（Dense + BM25，RRF 融合） |
| POST | `/api/upload` | UploadAndIngest | 上传文件并直接入库 |

### SkillService

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| GET | `/api/skillhub/hubs` | ListSkillhubHubs | 技能市场列表（语义入口） |
| GET | `/api/skillhub/search` | SearchSkillhub | 搜索技能市场（与 SearchHubSkills 等价的语义入口） |
| GET | `/api/skills` | ListSkills | 技能列表 |
| POST | `/api/skills` | InstallSkill | 手动安装/上传技能（同名视为升级） |
| GET | `/api/skills/from-hub` | SearchHubSkills | 搜索技能市场 |
| POST | `/api/skills/from-hub` | InstallFromHub | 从技能市场安装 |
| POST | `/api/skills/import-url` | ImportSkillFromURL | 从 URL 安装技能包（zip / SKILL.md / JSON，千问这类只给下载链接的来源） |
| GET | `/api/skills/hubs` | ListHubs | 技能市场列表 |
| POST | `/api/skills/hubs` | CreateHub | 新增技能市场 |
| DELETE | `/api/skills/hubs/{name}` | DeleteHub | 删除技能市场 |
| PATCH | `/api/skills/hubs/{name}` | UpdateHub | 修改技能市场 |
| POST | `/api/skills/hubs/{name}/default` | SetDefaultHub | 设为默认市场 |
| GET | `/api/skills/stats` | GetSkillStats | 规模统计 |
| POST | `/api/skills/sync` | SyncSkills | 把启用技能落盘到 sync_dir |
| DELETE | `/api/skills/{name}` | UninstallSkill | 卸载技能 |
| GET | `/api/skills/{name}` | GetSkill | 技能详情（含 SKILL.md 与文件正文） |
| POST | `/api/skills/{name}/disable` | DisableSkill | 停用技能 |
| POST | `/api/skills/{name}/enable` | EnableSkill | 启用技能 |
| GET | `/api/skills/{name}/file` | ReadSkillFile | 读取单个文件正文 |
| GET | `/api/skills/{name}/files` | ListSkillFiles | 技能文件清单（只返回路径与大小） |

### MCPService

`/api/mcp/servers/sync`、`/api/mcp/tools/search`、`/api/mcp/tools/call` 三条静态路径
声明在 `{id}` 之前（gorilla/mux 按注册顺序匹配），`contract_test.go` 有守卫。

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| GET | `/api/mcp/namespaces` | ListMCPNamespaces | 命名空间清单（过滤下拉用） |
| GET | `/api/mcp/servers` | ListMCPServers | 服务列表（含运行状态） |
| POST | `/api/mcp/servers` | CreateMCPServer | 新增服务 |
| POST | `/api/mcp/servers/sync` | SyncAllMCPServers | 同步全部启用服务的工具 |
| GET | `/api/mcp/servers/{id}` | GetMCPServer | 服务详情（含工具明细） |
| PUT | `/api/mcp/servers/{id}` | UpdateMCPServer | 更新服务配置 |
| DELETE | `/api/mcp/servers/{id}` | DeleteMCPServer | 删除服务（连带工具、向量、缓存） |
| PUT | `/api/mcp/servers/{id}/client` | SetMCPClient | 运行时替换 http.Client 参数并重连（不落库） |
| POST | `/api/mcp/servers/{id}/connect` | ConnectMCPServer | 建立连接（http 模式使用运行时注入的 client） |
| POST | `/api/mcp/servers/{id}/disconnect` | DisconnectMCPServer | 断开连接 |
| POST | `/api/mcp/servers/{id}/ping` | PingMCPServer | 探活（未连接时先连接） |
| POST | `/api/mcp/servers/{id}/sync` | SyncMCPServer | 同步工具清单：MySQL → Embedding → Milvus → Redis |
| POST | `/api/mcp/servers/{id}/toggle` | ToggleMCPServer | 启停服务 |
| GET | `/api/mcp/stats` | GetMCPStats | 规模统计 |
| GET | `/api/mcp/tools` | ListMCPTools | 工具列表 |
| POST | `/api/mcp/tools/call` | CallMCPToolByName | 按唯一名调用工具 |
| GET | `/api/mcp/tools/search` | SearchMCPTools | 工具语义召回 |
| GET | `/api/mcp/tools/{id}` | GetMCPTool | 工具详情 |
| DELETE | `/api/mcp/tools/{id}` | DeleteMCPTool | 删除工具 |
| POST | `/api/mcp/tools/{id}/call` | CallMCPTool | 调用工具 |
| POST | `/api/mcp/tools/{id}/toggle` | ToggleMCPTool | 启停工具 |

### LLMService（调用历史）

`/api/llm/calls`、`/api/llm/calls/stats` 由 `LLMCallQueryService` 提供。`llm.Manager`
在每次数据源尝试后**异步**写一条 `llm_calls`（provider/model/scene/purpose/task_id/
agent_key/温度/耗时/ok/错误/字符数/token 三项）——业务通过 `llm.WithMeta(ctx, CallMeta{...})`
把 task_id 塞进 context，记录即自动关联；只记字符数不落 prompt 内容。

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| GET | `/api/llm/calls` | ListLLMCalls | 调用历史（task_id/model/provider/purpose/status 过滤） |
| GET | `/api/llm/calls/stats` | GetLLMCallStats | 统计（token 三项、失败数、按模型聚合 TOP10） |

### AgentFactoryService

`/api/agentfactory/agents/create-by-llm`、`/api/agentfactory/agents/search`、
`/api/agentfactory/agents/sync-vectors` 声明在 `{id}` 之前，`/api/agentfactory/tasks/stats`
声明在 `/api/agentfactory/tasks/{id}` 之前（gorilla/mux 按注册顺序匹配）。
设计文档见 `internal/biz/agentfactory/DESIGN.md`。

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| GET | `/api/agentfactory/stats` | GetAgentFactoryStats | 规模统计（Agent/任务/能力） |
| GET | `/api/agentfactory/capabilities` | ListCapabilities | 可用能力清单（白名单选择器） |
| GET | `/api/agentfactory/agents` | ListAgentDefs | Agent 定义列表 |
| POST | `/api/agentfactory/agents` | CreateAgentDef | 人工低代码创建 |
| POST | `/api/agentfactory/agents/create-by-llm` | CreateAgentByLLM | LLM 元编程造 Agent（默认沙箱） |
| GET | `/api/agentfactory/agents/search` | SearchAgentDefs | 语义召回（Milvus 向量注册表） |
| POST | `/api/agentfactory/agents/sync-vectors` | SyncAgentVectors | 全量修复向量注册表 |
| GET | `/api/agentfactory/agents/{id}` | GetAgentDef | 定义详情 |
| PUT | `/api/agentfactory/agents/{id}` | UpdateAgentDef | 更新定义（版本 +1） |
| DELETE | `/api/agentfactory/agents/{id}` | DeleteAgentDef | 删除定义（连带向量缓存） |
| POST | `/api/agentfactory/agents/{id}/toggle` | ToggleAgentDef | 启停定义 |
| POST | `/api/agentfactory/tasks` | SubmitTask | 提交异步任务（Redis 队列 → worker） |
| GET | `/api/agentfactory/tasks/stats` | GetTaskStats | 任务统计 |
| GET | `/api/agentfactory/tasks` | ListTasks | 任务列表（可按父任务过滤链路树） |
| GET | `/api/agentfactory/tasks/{id}` | GetTask | 任务详情（运行中优先 Redis 快照） |
| POST | `/api/agentfactory/tasks/{id}/cancel` | CancelTask | 取消（pending 直接置态 / running 中断） |
| POST | `/api/agentfactory/tasks/{id}/retry` | RetryTask | 重试终态任务（trace 保留，清空旧 checkpoint） |
| POST | `/api/agentfactory/tasks/{id}/approval` | ApproveTask | 审批挂起任务：`{decision: approve/reject, comment?}`；批准后回 pending 续跑 |
| GET | `/api/agentfactory/tasks/{id}/events` | ListTaskEvents | 任务事件流（trace 链路检索） |

### ConversationService

会话 id 即任务 id；`/api/conversations/{id}/debates/current` 下的静态段
（`pause` / `resume` / `roles` / `stop`）声明在 `/api/conversations/{id}` 之后仍能命中，
是因为它们比 `{id}` 多一层路径段，不构成前缀冲突。

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| GET | `/api/conversations` | ListConversations | 会话目录 |
| POST | `/api/conversations` | CreateConversation | 新建会话 |
| GET | `/api/conversations/{id}` | GetConversation | 会话详情 |
| PUT | `/api/conversations/{id}` | UpdateConversation | 改标题等 |
| DELETE | `/api/conversations/{id}` | DeleteConversation | 删除会话 |
| POST | `/api/conversations/{id}/chat` | Chat | 一轮对话（调用 Agent / MCP / 检索） |
| POST | `/api/conversations/{id}/debates` | CreateDebate | 发起多角色辩论 |
| GET | `/api/conversations/{id}/debates/current` | GetCurrentDebate | 当前辩论状态（前端轮询） |
| POST | `/api/conversations/{id}/debates/current/pause` | PauseDebate | 暂停 |
| POST | `/api/conversations/{id}/debates/current/resume` | ResumeDebate | 继续 |
| POST | `/api/conversations/{id}/debates/current/roles` | AddDebateRole | 增加角色 |
| POST | `/api/conversations/{id}/debates/current/roles/remove` | RemoveDebateRole | 移除角色 |
| POST | `/api/conversations/{id}/debates/current/stop` | StopDebate | 叫停并汇总 |
| GET | `/api/conversations/{id}/messages` | ListMessages | 消息列表 |
| POST | `/api/conversations/{id}/messages` | AppendMessage | 追加消息 |

### HistoryService

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| GET | `/api/histories` | ListHistories | 统一操作历史（mcp / memory / rag / skill 等） |
| GET | `/api/histories/stats` | GetHistoryStats | 按类型计数 |

### AIPlatformService

外部 AI 平台接入域：把「千问 / 阿里云百炼 / 自建 MCP 广场」登记成数据源，
从平台目录搜索 MCP 服务并一键接成本地 MCP 服务；目录接口不可用时可按 `mcpCode` 直接接入。
千问形态：`https://dashscope.aliyuncs.com/api/v1/mcps/{code}/mcp`，
鉴权 `Authorization: Bearer $DASHSCOPE_API_KEY`。**API Key 只进不出**，读取接口只返回 `has_key`。

| 方法 | 路径 | RPC | 说明 |
| --- | --- | --- | --- |
| GET | `/api/aiplatform/platforms` | ListPlatforms | 平台列表（可按关键字 / 启停过滤） |
| POST | `/api/aiplatform/platforms` | CreatePlatform | 新增平台（千问只需 `name=qwen` + `api_key`，`kind` 省略时按名称推断） |
| PATCH | `/api/aiplatform/platforms/{name}` | UpdatePlatform | 修改平台（未传字段不改，`api_key` 留空不改） |
| DELETE | `/api/aiplatform/platforms/{name}` | DeletePlatform | 删除平台（不影响已接入的 MCP 服务） |
| POST | `/api/aiplatform/platforms/{name}/default` | SetDefaultPlatform | 设为默认平台 |
| POST | `/api/aiplatform/platforms/{name}/test` | TestPlatform | 连通性测试（目录可用或接入模板可用即算可用） |
| GET | `/api/aiplatform/mcps` | SearchMCP | 搜索平台目录里的 MCP 服务 |
| POST | `/api/aiplatform/mcps` | ImportMCP | 接入为本地 MCP 服务并同步工具 |

`/api/aiplatform/platforms`（集合）声明在 `/api/aiplatform/platforms/{name}`（单体）之前
（gorilla/mux 按注册顺序匹配），`contract_test.go` 有守卫。
接入动作复用 `biz/mcp.UseCase` 的「创建 → 连接 → 同步工具」，失败回滚删除，不重复实现。

### HarnessService

评测 Harness：用例集（被测对象）→ 异步运行 → 规则断言 + LLM 评审 → 报告。
被测对象 `agent:<agent_key>`（走执行图，产生完整 trace）或 `model:<模型名>`（模型直连）。

| 方法 | 路径 | 操作 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/harness/sets` | ListSets | 用例集列表（附启用用例数） |
| POST | `/api/harness/sets` | SaveSet | 新建（id=0）/ 更新用例集 |
| GET | `/api/harness/sets/{id}` | GetSet | 用例集详情（含全部用例） |
| DELETE | `/api/harness/sets/{id}` | DeleteSet | 删除用例集（有进行中运行时 409） |
| POST | `/api/harness/cases` | SaveCase | 新建 / 更新用例（断言规则七类） |
| DELETE | `/api/harness/cases/{id}` | DeleteCase | 删除用例 |
| POST | `/api/harness/sets/{set_id}/runs` | StartRun | 启动评测（异步，立即返回运行行） |
| POST | `/api/harness/runs/{id}/stop` | StopRun | 停止运行（用例边界生效） |
| GET | `/api/harness/runs` | ListRuns | 运行列表（按用例集 / 状态过滤） |
| GET | `/api/harness/runs/{id}` | GetRun | 运行报告（运行行 + 逐例结果） |
| DELETE | `/api/harness/runs/{id}` | DeleteRun | 删除运行及其逐例结果 |

## 工作流

```bash
# 1. 改 proto
vim api/rag/v1/rag.proto

# 2. 重新生成 Go 代码 + OpenAPI（含上传接口的 multipart 补丁）
make api

# 3. 实现接口方法（新增操作时），然后校验契约一致性
make check        # = go test ./internal/service/ -run 'TestProtoRoutesAreRegistered|TestPathParamRoutesLast'

# 4. 重新生成前端 SDK 并跑 SDK 测试（在 go_agent 目录）
make sdk          # = cd ../frontend && node scripts/gen-sdk.mjs && node scripts/test-sdk.mjs

# 5. 前端构建
cd ../frontend && npm run build
```

新增一个接口的完整清单：

1. `api/<domain>/v1/*.proto` 加 `message` + `rpc`（带 `google.api.http`，注意静态路径写在 `{param}` 前面）；
2. `internal/service/<domain>_service.go` 实现该方法（方法名与签名由生成接口决定，缺了编译不过）；
3. `internal/server/{http,grpc}.go` 里该服务已经注册过，新增方法无需再动装配；
4. `make api && make check`（不一致会直接列出来）；
5. `make sdk`，前端调用 `rag.newThing({...})` 即可，无需手写路径。

## 依赖与工具

`buf.gen.yaml` 使用**本地插件**（不走 `go run`，避免每台机器重复下载）：

```bash
make init     # 安装 wire / buf / protoc-gen-go / -go-grpc / -go-http(v3) / -openapi
```

`protoc-gen-go-http` 必须是 **kratos v3 分支**（`.../cmd/protoc-gen-go-http/v3`），
v2 版本生成的是 `kratos/v2` 的 import，与本项目不兼容。

> 根目录的 `openapi.yaml` 是脚手架自带的 Todo 示例产物，已不再生成；
> 现行契约文档是 `api/openapi.yaml`。
