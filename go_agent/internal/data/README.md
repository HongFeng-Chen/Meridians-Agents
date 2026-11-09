# Data

存储层，按存储类型分目录，每种存储遵循「连接 / models / repo」分层；
`models/` 一律按业务域拆成多个文件，一个对象（表 / 集合 / 键空间）一个文件，
不再放单个大文件。

| 目录 | 存储 | 内容 |
| --- | --- | --- |
| `db/` | MySQL | `db.go` 连接（`Data`）；`models/` PO 表映射；`repo/` 文档、提示词、记忆、技能、MCP 仓储 |
| `redis/` | Redis | `rdb.go` 连接（`RDBData`）；`models/` 键空间定义；`cache_*.go` 各业务缓存实现；`lock.go` 任务 Redlock（Quorum 加锁 + token CAS 释放/续期） |
| `milvusdb/` | Milvus | `client.go` 连接（`Client`）；`models/` 集合 Schema 与索引定义；`repo/` 分块、图谱、记忆、MCP 工具向量 Store |
| `esdb/` `clickhousedb/` | ES / CH | 预留客户端，未接入 wire（启动即强连，按需启用） |
| `third_service/mcpclient/` | MCP | MCP 协议客户端：stdio（子进程）+ Streamable HTTP（JSON/SSE），`*http.Client` 由外部注入；单测见同目录 `client_test.go`，联调桩服务见 `scripts/mock_mcp_server.mjs` |

wire 装配集中在 `data.go` 的 `ProviderSet`；biz 层只依赖接口（ports），不感知具体实现。

## models 的文件划分

**`db/models/`** — MySQL PO，按表域拆分，PO 与 `TableName()` 同文件：

| 文件 | 表 |
| --- | --- |
| `document.go` | `rag_documents`（`tenant_id` 默认 `public`，列表/检索按租户+用户过滤） |
| `memory.go` | `memories`（仅 `user_id`，`tenant_id` 待补，见 HANDOVER 5.5） |
| `prompt.go` | `prompt_templates`、`prompt_versions` |
| `skill.go` | `skills`、`skill_files`、`skill_hubs` |
| `mcp.go` | `mcp_servers`、`tools`（MCP 服务与工具元数据；headers/args/env 是 JSON 列，空集合必须写 `{}` / `[]`，不能写空串） |
| `llm.go` | `llm_providers`（数据源配置）、`llm_calls`（调用历史：token 用量/耗时/模型/task_id/tenant_id 关联，Manager 异步写入） |
| `agentfactory.go` | `agent_definitions`、`agent_tasks`（任务池：pending/running/paused，带 `tenant_id`）、`agent_task_history`（终态归档，`FinishTask` 事务迁移 / `ReviveTask` 复活）、`agent_task_events`、`agent_task_checkpoints`（步骤/节点级快照）；capabilities 等是 JSON 列，任务时间列可空、零值写 NULL |
| `doc.go` | 包说明 |

**`milvusdb/models/`** — 集合 Schema，一个集合一个文件（字段常量 + 构造函数同文件）：

| 文件 | 内容 |
| --- | --- |
| `chunk.go` | RAG 主集合（dense + BM25 稀疏） |
| `entity.go` | GraphRAG 实体集合 |
| `relation.go` | GraphRAG 关系边集合 |
| `memory.go` | 长期记忆集合 |
| `tool.go` | MCP 工具集合（只存 `tool_id + dense + server_id + ns + is_enabled`，详情在 MySQL/Redis） |
| `agent.go` | Agent 向量注册表（只存 `agent_id + dense + is_enabled + source`，定义详情在 MySQL/Redis） |
| `index.go` | 索引定义（HNSW / BM25 参数，各集合共用） |
| `doc.go` | 包说明 |

**`redis/models/`** — Redis 的 model 是「键空间」（Redis 没有表结构），
这里是键前缀、键构造与失效时的 SCAN 模式：

| 文件 | 键 |
| --- | --- |
| `answer.go` | `rag:ans:`（问答缓存）、`rag:lock:ingest:`（入库锁） |
| `memory.go` | `mem:short:{session_id}`（短期会话）、单会话保留条数 |
| `prompt.go` | `prompt:ver:{key}`（提示词生效版本） |
| `mcp.go` | `mcp:tool:{id}`（工具完整对象）、`mcp:status:{id}`（运行状态）、`ns:tools:{ns}`（命名空间索引 Set） |
| `agentfactory.go` | `af:agent:{id}`（定义缓存）、`af:task:{id}`（运行中任务快照，实时轮询）、`af:queue:tasks`（任务队列 List，LPUSH/BRPOP）、`af:lock:task:{id}`（任务执行 Redlock，值 = 随机 token） |
| `key.go` | 前缀 → SCAN 模式的统一转换 |
| `doc.go` | 包说明 |

缓存的键由谁定：**带领域语义的键（什么算同一个问题、哪个 doc 的锁）由 biz 生成**
（`rag.AnswerCacheKey` / `rag.IngestLockKey`），data 层只按前缀做批量失效；
前缀登记在 `redis/models`，两边一致性由 `redis/keys_contract_test.go` 守着——
前缀漂移会让 `Invalidate` 静默失效，属于最难排查的一类问题。

## GORM `Updates(map[string]any{...})` 的列名陷阱

GORM 的 **map 更新不走 struct tag 映射，key 直接当列名**：
`Updates(map[string]any{"flow": ...})` 会拼出 `SET flow = ?`，而表里叫 `flow_json`，
运行时报 `Error 1054 (42S22): Unknown column 'flow' in 'field list'`——编译期发现不了。

约定：

- 能用 struct 更新就用 struct（走 tag 映射）；确有「只更新非零子集」需求才用 map。
- 用 map 时必须逐字对齐 `db/models/**` 的 `gorm:"column:..."`。
- 新增/修改 map 更新后跑 `go test ./internal/data/db/repo/ -run TestMapUpdateKeysMatchModelColumns`；
  该测试扫描 repo 层所有 map 字面量 key 与全部模型列名比对，写错即失败。
- 空值与零值语义：`""` 与 `NULL` 不等价。`flow_json` 允许为空（空编排列值为 NULL），
  业务侧「清空编排」需显式写 `NULL` 而不是跳过字段。
