# 仓库协作契约（AGENTS.md）

> 本文件说明**在本仓库里改代码必须遵守的规则**，面向接手的人与 AI 编码助手。
> 想先了解「这个系统能干什么」看 `README.md`；想接手 biz 层看 `internal/biz/HANDOVER.md`。
> `CLAUDE.md` 与本文件内容一致（同一份契约的两个入口）。

## 1. 这是什么

`go_agent` 是 Kratos v3 四层（proto → service → biz → data）的单体服务，同时对外提供 HTTP 与 gRPC，
一个进程内托管 18 个业务域（会话、Agent 工厂、RAG/GraphRAG、记忆、提示词、MCP、LLM、技能、历史、
平台 MCP、外部 AI 平台、评测 Harness、知识空间、统一资源、成本中心、账号、租户名册、审计）。
前端在仓库上一级 `frontend/`（React 19 + Vite），只通过 `/api` 与 `/mcp/platform` 访问后端。
V2 的执行链路、快照授权、可观测与评估细节见 `docs/architecture-v2.md`。

## 2. 常用命令

```bash
go run ./cmd/go_agent -conf ./configs   # 本地启动（-conf 传目录，不是文件）
go build ./...                           # 编译检查
go test ./...                            # 全量单测
make api                                # proto → pb.go / grpc / http / openapi.yaml（+ multipart 补丁）
make config                             # conf.proto → conf.pb.go
make check                              # 生成路由 与 openapi.yaml 一致性校验
make sdk                                # openapi.yaml → 前端 SDK
```

改完代码至少跑 `go build ./... && go test ./...`；动到路由或 proto 必须 `make api` 后再 `make check`。

## 3. 目录与依赖方向

```text
api/<domain>/v1/      proto 定义与生成物（对外唯一契约）
cmd/go_agent/         入口：wire 装配 + worker 生命周期 + 平台 MCP 自注册
configs/              运行配置（${ENV:default} 展开；不落明文密钥）
internal/conf/        配置 proto（make config 生成）
internal/service/     传输适配：DTO ↔ DO、参数校验、错误映射
internal/biz/         业务用例、领域模型（DO）、repo 接口、typing 错误
internal/data/        存储实现（MySQL / Redis / Milvus / MCP 客户端 / 技能市场）
internal/server/      HTTP / gRPC 装配 + 平台 MCP 端点 + /api/v2 手写路由
internal/pkg/         loader、httpx、trace（OTel）、principal、authz、budget、egress 等无业务工具
```

```text
client ──► DTO ──► service ──► DO ──► biz ──► DO ──► data ──► PO ──► storage
                                 ▲              ▲
                                 │ 声明接口      │ 实现接口
```

| 层 | 拥有 | 边界说什么 | 禁止 |
| --- | --- | --- | --- |
| `service` | 无 | DTO ↔ DO | 碰 PO、碰存储客户端、写业务规则 |
| `biz` | DO + repo 接口 | DO | import `internal/data`、碰 DTO（错误枚举除外） |
| `data` | PO | DO ↔ PO | 碰 DTO、把驱动类型泄漏出去 |
| `server` | 无 | — | 写业务逻辑、做参数翻译 |

- `biz` 里出现 `gorm` / `milvusclient` / `redis` 字样，基本可判定分层被破坏。
- 具体实现只在 `cmd`（wire）或 `data.go` / `biz.go` 的 ProviderSet 里拼装。
- 新模块按 `internal/biz/HANDOVER.md` 第 4 章 checklist 走：DTO → DO+接口 → repo → service → 注册 → `make all`。

## 4. 生成物与手写边界

| 文件 | 规则 |
| --- | --- |
| `*.pb.go`、`*_grpc.pb.go`、`*_http.pb.go`、`*_validate.pb.go` | 只由 `make api` 生成，**禁止手改** |
| `internal/conf/conf.pb.go` | 只由 `make config` 生成 |
| `cmd/**/wire_gen.go` | 由 wire 生成；手工补的生命周期钩子写在 `main.go` 的 `newApp` 里，别写进 wire_gen |
| `api/openapi.yaml` | 生成 + `scripts/` 补丁（multipart 等），补丁逻辑不要绕过 |

新增接口的标准动作：改 `api/<domain>/v1/*.proto`（必须写 `json_name`）→ `make api` → `internal/service` 实现 → `make check`。

## 5. biz 模块地图

| 模块 | 职责 | 关键入口 |
| --- | --- | --- |
| `conversation` | 会话目录、消息、LLM 自主编排、多角色辩论 | `/api/conversations/**` |
| `agentfactory` | Agent 定义 / 任务 / 13 种执行图（含 auto）/ worker / cron / 优雅停机 | `/api/agentfactory/**` |
| `rag` + `rag/graphrag` | 入库、混合检索问答、图谱构建与召回 | `/api/ingest` `/api/query` `/api/graph/**` |
| `memory` | 短期 Redis + 长期 MySQL/Milvus + 记忆图谱 | `/api/memory/**` |
| `prompt` | 模板版本化、激活回滚、渲染 | `/api/prompts/**` |
| `mcp` | MCP 服务/工具生命周期、语义召回、调用管线 | `/api/mcp/**` |
| `platformmcp` | 平台只读 MCP 工具（任务/会话/链路/历史），供 `/mcp/platform` 暴露 | — |
| `llm` | 多数据源、场景路由、故障切换、调用历史 | `/api/llm/**` |
| `skill` | 技能安装启停、市场下载、落盘、按 URL 装包 | `/api/skills/**` |
| `aiplatform` | 外部 AI 平台（千问 / 百炼）登记、目录搜索、MCP 一键接入 | `/api/aiplatform/**` |
| `history` | 统一操作历史（5 类召回 + MCP 调用） | `/api/histories/**` |
| `harness` | 评测：用例集 → 异步运行 → 断言 + LLM 评审 → 报告；运行绑定快照/数据集版本，带心跳与失联回收 | `/api/harness/**` |
| `space` | 知识空间：树形组织 + 继承 + 显式共享，为 RAG 与资源授权算白名单 | `/api/v2/spaces*` |
| `resource` | 统一资源模型与资源级授权（默认拒绝、显式 deny 优先、跨租户硬隔离） | `/api/v2/resources*` |
| `usage` | 成本中心：追加式用量 + 多维聚合（task / ref / model / kind / day） | `/api/v2/usage/*` |
| `authn` | 账号与登录：用户名密码 + LDAP/AD，签发本平台令牌 | `/api/auth/**` |
| `org` | 租户与成员名册 | `/api/admin/**` |
| `audit` | 审计事件（授权判定 / 管理面变更 / 高危工具调用），异步批量写 | `/api/admin/audits` |

模块间允许「编排型调能力型」：`conversation → agentfactory / memory / rag / mcp`，`agentfactory → mcp / llm`，
`rag → prompt / llm / embedder`，`aiplatform → mcp / skill`（接入就是调 mcp 的创建→连接→同步）。
V2 新增几条窄接口依赖：`rag.SpaceScope ← space`、`resource.SpaceAuthorizer ← space`、
`llm.CostSink ← data/repo.CostSink`（一次 LLM 请求写一条 `usage_records`）。
反向调用一律不允许：`aiplatform` 只通过 `ports.go` 的 `MCPRegistry` / `SkillMarkets` 接口调 mcp / skill，
不 import 它们的内部实现。

## 6. 必须遵守的几条硬约束

1. **ctx 传身份，不改业务签名**：`history.WithTask` / `history.WithConv` 都是读改写，可任意叠加。
   worker 里执行任务时，`runGraph` 会用 `task.ConvID` 回填 `WithConv`；子任务继承 `parent.ConvID`。
   **顺序**：先 `WithTask` 再 `WithConv`（或保证 `WithTask` 读改写语义）。改这块必须跑 `history_test.go` 与 `TestConvIDInheritance`。
2. **GORM map 更新的 key 就是列名**：`Updates(map[string]any{"flow_json": ...})` 写错成 `"flow"` 运行时报
   `Error 1054 Unknown column`。新增 map 更新后跑 `TestMapUpdateKeysMatchModelColumns`。
3. **异步任务状态机只能按边迁移**：任务任一时刻只在一张表（活跃表 / 历史表），`FinishTask` / `ReviveTask` 必须单事务；
   `ClaimTask` 必须原子；其余状态变更一律走 `TransitionTask(ctx, id, from, t)`（CAS），不要裸 `SaveTask` 覆盖状态——
   并发下会把「已取消」改回「已完成」。合法迁移边见 `task_state.go`。
4. **优雅停机**：`StartWorkers` 在 `BeforeStart`，`Drain` 在 `BeforeStop`，`Stop` 在 `AfterStop`。
   停机期间 `SubmitTask` 返回 `ErrShuttingDown`；排空超时被打断的任务保持 `running`，交给 `RequeueInterrupted` 重跑。
   `Drain` 的等待只看 `workerWG`（不含 cron 调度器），否则会白等满超时。
5. **召回 best-effort**：记忆 / RAG / GraphRAG 失败只写 `meta.*_error`，不得让对话失败。
6. **写历史 / 写日志异步**：失败只 warn。
7. **查询必须带租户**：已接入租户的表一律 `(tenant_id, user_id)` 双条件；新表加 `tenant_id ... default 'public'`，
   读路径用 `identity.TenantID(ctx)`，不要只按 user_id 过滤（不同租户可以有相同 user_id）。
8. **平台 MCP 只读 + 仅本机**：不要给 `/mcp/platform` 加写工具，也不要去掉 `isLoopback` 守卫。
9. **protojson 零值省略**：新增字段默认值必须等价于旧行为，避免老前端丢字段。
10. **错误用哨兵**：`errors.Is` 判类型，biz 用 `errors.NotFound/BadRequest` + reason 枚举，service 统一映射 HTTP 状态码。
11. **LLM 调用只能走 `Manager.callOnce`**：一次真实请求 = 一条 `llm.chat` span = 一条 `usage_records`。
    新增调用通道（新方法 / 新客户端）绕过它，既没链路也没账单——P4 首次实现就把 `chat()` 漏在外面，
    评测的模型直连调用在 trace 里完全不可见。
12. **`llm.WithMeta` 是按字段合并**：零值表示「不改」。下游（如 agentfactory）设 `TaskID` / `Purpose`
    不会冲掉上游的 `ref_kind` / `ref_id`；不要改回整体覆盖，否则评测成本会变成无主账。
13. **map 更新里的时间字段先判零值**：`Updates(map[string]any{"finished_at": t})` 在 `t` 为零值时
    序列化成 `'0000-00-00'`，MySQL `NO_ZERO_DATE` 会拒掉**整条 UPDATE**（1292）。统一走 `nilIfZero`，
    并且**不要用 `_ =` 吞掉状态 / 进度写入错误**（评测进度曾因此永远停在 0）。
14. **追踪是可选依赖**：未调 `trace.Init` 时全局 noop、`Start` 返回空句柄，业务代码不要假设有 collector；
    也别用「TraceID 是否为空串」判活——noop 的 SpanContext 无效，但 `TraceID()` 会给出全零字符串。
15. **长跑实体要有心跳与回收**：任务靠租约 `lease_expires_at`，评测运行靠 `heartbeat_at`；
    新增长跑实体照此做，回收一律「收尾不重跑」，并在 error 里写清原因（如「执行体失联」）。
16. **判权只有两层，不要再引入策略表**：`pkg/authz` 的内置角色矩阵（代码）+ `biz/resource`
    的资源级授权（`resource_permissions`）。Casbin 已整体移除——它带来的「运行时可改的
    授权表」会让「谁能做什么」需要看两处快照才能回答，事故复盘时无法用一个事实说清。
    需要给某人额外放权时用 `/api/v2/resources/grants`，不要新建全局策略表或旁路判断。

## 7. 测试约定

- 测试与代码同目录（`*_test.go`）。`service` 假 usecase、`biz` 假 repo、`data` 打真实存储边界。
- `biz` 用手写 `fakeRepo`，不引 mock 框架：接口变更后 fake 编译失败，是刻意设计，用来强制更新所有实现。
- 这些回归测试**不许删**：`internal/data/db/repo/columns_test.go`、`internal/server/mcp_test.go`、
  `internal/biz/history/history_test.go`、`agentfactory_test.go` 里的 `TestDrain*` / `TestConvIDInheritance`、
  `internal/pkg/trace/trace_test.go`、`internal/biz/harness/{version,recovery,ref}_test.go`、
  `internal/data/db/repo/harness_recovery_test.go`。
- 单测不允许连真实 MySQL / Redis / Milvus；需要时钟、随机、外部调用一律注入。

## 8. 文档索引

| 文档 | 内容 |
| --- | --- |
| `README.md` | 能力、快速开始、配置、API、项目结构 |
| `docs/architecture-v2.md` | **V2 现状与边界**：身份/资源/空间/成本/租约/快照/委派/可观测/评测，含上线 SQL 与排查路径 |
| `internal/biz/HANDOVER.md` | biz 层交接：模块明细、时序图、跨模块协作、已知问题、排障 |
| `internal/biz/agentfactory/DESIGN.md` | Agent 工厂设计讨论稿（非实现说明，现状以 HANDOVER 2.2 为准） |
| `internal/server/MCP网关方案.md` | 平台 MCP 端点方案与落地现状 |
| `internal/biz/rag/DESIGN.md`、`rag/graphrag/DESIGN.md` | RAG / GraphRAG 设计 |
| `internal/biz/harness/设计方案.md` | 评测 harness 设计 |
| `docs/*.md` | llm / memory / prompt / rag / skill 五个子系统的实现笔记 |
| `api/README.md` | proto 契约约定与各域操作清单 |

改行为时同步改文档：**接口变化** → `api/README.md`；**模块行为/状态机变化** → `HANDOVER.md` 对应小节；
**部署/配置/命令变化** → `README.md`；**V2 能力、配置开关、已知边界变化** → `docs/architecture-v2.md`。

## 9. 提交约定

- Conventional Commits：`feat:` / `fix:` / `refactor:` / `chore(deps):` / `docs:` / `test:`。
- 生成物与源文件同一个提交（改 proto 就带上生成结果）。
- 提交前：`go build ./... && go test ./...`；动过 proto 再 `make check`。
- 永不提交真实凭据（`configs/config.yaml` 里的 key 用 `${ENV:default}` 注入）。
