# biz · 业务用例层

本目录是 Kratos DDD 的 **biz 层**：业务用例、领域模型（DO）、repo 接口、哨兵错误。
**只依赖接口**（每个包的 `ports.go` 或主文件顶部的 `xxxRepo interface`），**不 import `internal/data`**。

> 详细交接（模块明细、时序图、跨模块协作、已知问题、排障）见 [`HANDOVER.md`](./HANDOVER.md)。
> 本文件只做一页速查。

## 模块

| 模块 | 一句话职责 | 对外入口 | 详细 |
| --- | --- | --- | --- |
| `conversation` | 会话目录、消息、LLM 自主编排、多角色辩论 | `/api/conversations/**` | HANDOVER 2.1 |
| `agentfactory` | Agent 定义 / 任务 / 13 种执行图（含 auto）/ worker 池 / cron / 优雅停机 | `/api/agentfactory/**` | HANDOVER 2.2 |
| `harness` | 评测 Harness：用例集 → 异步运行 → 断言 + LLM 评审 → 报告；运行绑定快照/数据集版本，带心跳回收 | `/api/harness/**` | HANDOVER 2.3 |
| `memory` | 短期 Redis + 长期 MySQL/Milvus + 记忆图谱 | `/api/memory/**` | HANDOVER 2.4 |
| `rag` + `rag/graphrag` | 入库、混合检索问答、图谱构建与召回 | `/api/ingest` `/api/query` `/api/graph/**` | HANDOVER 2.5、`rag/DESIGN.md` |
| `prompt` | 模板版本化、激活回滚、渲染 | `/api/prompts/**` | HANDOVER 2.6 |
| `mcp` | MCP 服务/工具生命周期、语义召回、调用管线 | `/api/mcp/**` | HANDOVER 2.7 |
| `platformmcp` | 平台只读 MCP 工具（任务/会话/链路/历史 7 个） | `/mcp/platform` | HANDOVER 5.7、`../server/MCP网关方案.md` |
| `llm` | 多数据源、场景路由、故障切换 | `/api/llm/**` | HANDOVER 2.8 |
| `skill` | 技能安装启停、市场下载、落盘、按 URL 装包 | `/api/skills/**` | HANDOVER 2.9 |
| `aiplatform` | 千问 / 百炼平台登记、目录搜索、MCP 一键接入（复用 `mcp.UseCase`） | `/api/aiplatform/**` | HANDOVER 2.13 |
| `history` | 统一操作历史（5 类召回 + MCP 调用） | `/api/histories` | HANDOVER 2.10 |
| `org` | 系统管理：多租户（tenants）+ 成员名册（users），规则在用例里、repo 只存取 | `/api/admin/**` | `org/org.go` 文件头 |
| `space` | 知识空间：树形组织 + 继承 + 显式共享，给 RAG 与资源授权算白名单 | `/api/v2/spaces*` | `docs/architecture-v2.md` §6 |
| `resource` | 统一资源模型与资源级授权：默认拒绝、显式 deny 优先、跨租户硬隔离 | `/api/v2/resources*` | `docs/architecture-v2.md` §5 |
| `usage` | 成本中心：追加式用量 + 多维聚合；LLM 自动记账的落点 | `/api/v2/usage/*` | `docs/architecture-v2.md` §7 |
| `authn` | 账号与登录：用户名密码 + LDAP/AD | `/api/auth/**` | `authn/authn.go` 文件头 |
| `audit` | 审计事件：授权判定 / 管理面变更 / 高危工具调用 | `/api/admin/audits` | `internal/server/ops.go` |
| `embedder` | OpenAI 兼容 Embedding 客户端（内部能力） | — | HANDOVER 2.11 |

V2 的执行链路（租约 / 重试 / 幂等 / 快照 / 委派 / 二次授权）在 `agentfactory`，
可观测（Trace / 成本）是横切的：`internal/pkg/trace` + `llm.Manager.callOnce` + `usage`。
完整设计、上线 SQL 与已知边界见 [`docs/architecture-v2.md`](../docs/architecture-v2.md)。

## 允许的依赖方向

编排型模块可以调能力型模块，反向不允许：

```text
conversation → agentfactory / memory / rag / mcp / llm
agentfactory → mcp / llm / embedder
rag          → prompt / llm / embedder / graphrag
memory       → prompt / llm / embedder / rag / graphrag
harness      → （通过 Runner 端口）agentfactory / llm
```

## 四条不许破坏的约定

1. **ctx 传身份**：`history.WithTask` / `WithConv` 都是读改写，可叠加；worker 用 `task.ConvID` 回填，
   子任务继承父会话。改这块跑 `history_test.go` + `TestConvIDInheritance`。
2. **状态机只能按边迁移**：任务任一时刻只在活跃表或历史表之一，`FinishTask` / `ReviveTask` 单事务，
   `ClaimTask` 原子；其余状态变更走 `TransitionTask`（CAS），合法边见 `task_state.go`。
   已接入租户的查询带 `(tenant_id, user_id)`。
3. **召回 best-effort**：记忆 / RAG / GraphRAG 失败只写 `meta.*_error`，不能把对话打挂。
4. **LLM 调用只走 `Manager.callOnce`**：一次真实请求 = 一条 `llm.chat` span = 一条 `usage_records`；
   归属用 `llm.WithMeta`（按字段合并，零值不改），`TaskID` 与 `RefKind/RefID` 各管一类场景。

## 装配与测试

- 构造与接口绑定在 `biz.go`（wire ProviderSet）；`data` 侧实现由 `data.go` 绑定。
- 单测用**手写 fakeRepo**（不引 mock 框架）：接口一改，fake 先编译失败——刻意设计，强制更新所有实现。
- `go test ./internal/biz/...` 应全绿；新增模块按 `HANDOVER.md` 第 4 章 checklist 走。
