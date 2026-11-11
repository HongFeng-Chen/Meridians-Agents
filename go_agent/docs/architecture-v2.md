# V2 架构升级：现状、落地与路线图

对应输入文档：`待优化项/06-v2.md`（Meridians-Agents V1 → V2）。

本文不复述原文档，只回答三件事：**哪些已经做了、代码在哪、下一步做什么**。
所有判断都以当前仓库代码为准，不是目标态的描述。

---

## 1. 结论先行

V2 的十二个改造方向里，本次落地了「地基」五件事：

| 能力 | 状态 | 说明 |
|---|---|---|
| External Identity / Principal | ✅ 已落地 | 多 Provider（HS256 / JWKS / OIDC discovery / Introspection / API Key），与 V1 令牌兼容 |
| 统一资源模型 + 资源授权 | ✅ 已落地 | `resource_metadata` / `resource_permissions` / `resource_versions`，默认拒绝 |
| Knowledge Space | ✅ 已落地 | 树形空间、三种继承模式、显式共享、统一 RetrievalScope，RAG/GraphRAG 共用 |
| Cost Center | ✅ 基础版 | `usage_records` 追加式用量 + 多维聚合 |
| Worker 分布式执行 | ✅ 已落地 | `worker_instances` 注册/心跳 + 任务租约 + `retry_wait`/`timeout` 状态 + 死信 |
| 工具调用幂等 | ✅ 已落地 | `mcp_call_records` + `WithIdemKey`：任务重试链路命中即回放，失败不缓存 |
| Agent 配置快照 | ✅ 已落地 | `agent_snapshots`（checksum 去重、只增不改）；任务钉 `snapshot_id`，执行按快照加载配置 |
| 运行时二次授权 | ✅ 已落地 | `agent`/`tool`/`skill` 登记资源执行前重新判权（快照 ≠ 永久权限） |
| 委派上下文 | ✅ 已落地 | 子任务 `delegated_caps`：多级委派单调收紧、深度闸门、归属继承 |
| Agent 不可变快照 | ⏳ 待做 | 现有 `agent_definitions.version` 只是递增号，未冻结依赖 |
| Evaluation / Observability | ⏳ 待做 | 已有 harness 与 metrics，缺统一 Trace 与评估快照 |
| API v2 / SDK | 🟡 部分 | `/api/v2` 已可用（空间/资源/成本/me），未走 proto/OpenAPI 生成 |

为什么先做这五件：它们是其余模块的前置依赖。没有统一资源与空间，
Worker 拿到的任务无法判断「这个 Agent 还能不能检索这个知识库」；
没有 Principal，第三方系统接入只能继续复用平台内部用户体系；
没有租约，多副本部署时「进程被 kill 掉的任务」会永远停在 running。

---

## 2. 差距矩阵（逐章对照）

| 原文章节 | V1 已有 | 本次改动 | 仍缺 |
|---|---|---|---|
| 3. External Identity | JWT(HS256) + 静态令牌，单租户 claim | Principal 模型；JWKS/RS256、OIDC discovery、RFC 7662、API Key 适配器 | OAuth2 授权码流程（属于第三方系统职责）、Token 缓存分层 |
| 4. 统一资源 | 各业务表各自管归属 | `resource` 包：注册/版本/显式授权/判权；9 种动作 | 资源创建尚未强制经过统一注册（渐进接入中） |
| 5. Knowledge Space | 三层 scope（system/tenant/user） | 空间树 + 继承 + 共享 + 迁移映射；文档双写 `space_id` | 空间配额、跨空间引用计数、控制台页面 |
| 6. RAG | MySQL 算 doc_id 白名单 → Milvus 过滤 | 白名单改由空间授权计算；旧数据自动回落三层作用域 | 空间级 embedding 配置、按库路由 |
| 7. GraphRAG | 图谱靠 `chunk_refs` 跟随文档可见性 | 与 RAG 共用同一份白名单（`visibleDocIDs`） | 实体级 ACL、跨空间实体映射 |
| 8. Agent 快照 | 定义版本号、能力白名单 | 资源版本表可承载快照 | 发布时冻结 Prompt/Skill/MCP/RAG 版本 |
| 9. Skill / MCP 安全 | 审批、egress、redact、凭证脱敏 | — | Skill 沙箱（容器/WASM）、Tool 级 ACL 接入 |
| 10. Worker | Redis 队列 + 进程内 worker 池 + CAS 状态机 | `worker_instances` 注册/心跳；任务租约续期与回收；`retry_wait`/`timeout`、死信；启动恢复按 attempt 限制 | 外部副作用幂等键、跨节点任务分片（当前靠 Redis 队列竞争） |
| 11. Evaluation | harness 四张表 + 评分 | — | 评估与 Agent 快照/数据集版本绑定 |
| 12. Cost Center | `llm_calls` 记录 token | `usage_records` + 聚合 API | 计价规则版本管理、账单结算 |
| 13. Observability | access log + metrics | — | OpenTelemetry Trace（跨 LLM/RAG/MCP 子调用） |

---

## 3. 代码位置

| 层 | 路径 | 职责 |
|---|---|---|
| 身份 | `internal/pkg/principal/` | Principal、Provider 接口、Resolver、四种外部适配器 |
| 身份（兼容） | `internal/pkg/identity/`、`internal/pkg/auth/` | V1 Info 扩展 issuer/scopes/departments；JWT claim 解析 |
| 资源 | `internal/biz/resource/` | 资源类型/动作、注册、版本、显式授权、`Check` |
| 空间 | `internal/biz/space/` | 空间树、继承、共享、`AllowedSpaceIDs`、scope→space 映射 |
| 成本 | `internal/biz/usage/` | 追加式用量、聚合过滤 |
| 执行链路 | `internal/biz/agentfactory/lease.go`、`task_state.go` | 执行体注册/心跳、任务租约、清扫回收、重试退避、状态机 |
| 执行链路（表） | `internal/data/db/models/agentfactory.go`、`internal/data/db/repo/worker.go` | `worker_instances` + 任务租约列 + 执行体仓储 |
| 工具调用幂等 | `internal/biz/mcp/idem.go`、`internal/data/db/repo/mcp.go` | 幂等键生成/注入、`mcp_call_records` 读写、命中回放 |
| 配置快照 | `internal/biz/agentfactory/snapshot.go`、`internal/data/db/repo/agentfactory.go` | checksum、依赖抽取、发布去重、按快照加载 |
| 二次授权 | `internal/biz/agentfactory/authz.go`、`internal/biz/resource/resource.go` | 运行前/调用前判权、`Registered` 区分未登记 |
| 委派上下文 | `internal/biz/agentfactory/delegation.go` | 能力交集、通配覆盖、深度闸门、归属继承 |
| 模型 | `internal/data/db/models/{resource,space,usage}.go` | 五张表 |
| 仓储 | `internal/data/db/repo/{resource,space,usage}.go` | gorm 实现（含 sqlite 可跑的测试） |
| 接入 | `internal/server/v2.go`、`internal/server/middleware.go` | /api/v2 路由；网关 Principal 注入与外部 Provider |
| RAG 集成 | `internal/biz/rag/rag.go`（`visibleDocIDs`）、`internal/data/db/repo/document.go` | 空间白名单 + 旧数据回退 |

测试：

```bash
go test ./internal/biz/space/ ./internal/biz/resource/ \
        ./internal/biz/usage/ ./internal/pkg/principal/ \
        ./internal/biz/agentfactory/ \
        ./internal/data/db/repo/ ./internal/biz/rag/
```

覆盖的核心语义：默认拒绝、显式 Deny 优先、跨租户硬隔离、
restricted 不继承、个人空间 isolated、子树成环拦截、版本不可覆盖、
空间白名单与旧作用域回退；租约回收后进 retry_wait、超上限进死信、
超截止时间落 timeout 并归档、执行体心跳过期判离线。

---

## 4. External Identity

### 4.1 Principal

```go
type Principal struct {
    Issuer, ClientID, SubjectID, TenantID string
    Roles, Scopes, DepartmentIDs, Audience []string
    AuthType string
    Admin    bool
}
```

唯一身份 = `issuer + client_id + tenant_id + subject_id`。
不能只用 subject：不同第三方系统的 `admin` 是两个不同的人。

### 4.2 认证与授权分离

- Provider 只做「凭证 → Principal」，不查库、不判权限；
- 第三方令牌里的 `roles` **不会**自动变成平台 RBAC 角色；
- 平台权限仍走 `pkg/authz`（Casbin + 内置矩阵），资源权限走 `biz/resource`。

### 4.3 启用方式（环境变量）

```bash
# 外部 IdP 的公钥（RS256/384/512），适合 Keycloak / Auth0 / 自建 OIDC
IDENTITY_JWKS_URL=https://idp.example.com/realms/corp/protocol/openid-connect/certs
IDENTITY_JWKS_ISSUER=https://idp.example.com/realms/corp
IDENTITY_JWKS_AUDIENCE=meridians

# 或者只给 issuer，启动时自动读 /.well-known/openid-configuration
# IDENTITY_OIDC_ISSUER=https://idp.example.com/realms/corp

# 不透明令牌（opaque token）：交给 IdP 校验
# IDENTITY_INTROSPECTION_URL=https://idp.example.com/introspect
# IDENTITY_CLIENT_ID=meridians
# IDENTITY_CLIENT_SECRET=******

# 受控服务接入：token=用户:租户[:角色]，逗号分隔
# IDENTITY_API_KEYS="abc123...=svc-report:public,def456...=svc-etl:acme"
```

全部留空时网关行为与 V1 完全一致（只认本地 HS256 与静态令牌），
不会因为升级而改变现有部署的鉴权口径。

判定顺序：本地 HS256 验签 → JWKS/RS256 → Introspection → API Key。
便宜的在前，避免每个请求都打一次 IdP。

---

## 5. 统一资源与资源授权

### 5.1 数据模型

| 表 | 作用 |
|---|---|
| `resource_metadata` | 归属（tenant/space/owner）、类型、可见性、版本、状态 |
| `resource_permissions` | 一个「主体 + 动作」一行，带 `allow/deny` |
| `resource_versions` | 不可变快照（追加，不覆盖）；`checksum` 用于比对两次发布是否等价 |

资源类型：`agent / prompt / skill / mcp_server / mcp_tool / knowledge_base / graph_space / evaluation_dataset`。
动作：`create / read / update / delete / share / bind / execute / publish / manage_permission`。

### 5.2 判权顺序（改动前先读）

1. 资源不存在 → 拒绝
2. 跨租户 → 拒绝（`visibility=platform` 且 `read` 例外）
3. 显式 **Deny** → 拒绝（永远优先于 Allow）
4. 显式 **Allow** → 允许
5. 归属人 → 允许
5′. 资源所属空间对调用方可见 → 允许
6. 平台公共资源 + `read` → 允许；租户共享 + `read` → 允许
7. 其余 → 拒绝

显式授权不能突破租户隔离：给其他租户授权直接 `ErrForbidden`。

---

## 6. Knowledge Space

### 6.1 模型

```text
platform                     平台公共知识（所有租户可见）
└── <自定义子空间>

tenant:<租户>                 租户根空间（restricted：只认显式授权）
├── dept:<租户>:<部门>        部门空间（inherit 才继承父级）
│   └── …                    最多 8 层
└── personal:<租户>:<用户>    个人空间（isolated：不继承、不共享）
```

- 邻接表是权威关系，`path` / `depth` 是查询加速；
- 移动子树时整棵子树路径一起平移，成环直接拒绝；
- 删除有子空间的空间直接拒绝（避免孤儿）。

### 6.2 继承

| 模式 | 行为 |
|---|---|
| `inherit` | 父空间可见 → 子空间可见（逐层传递） |
| `restricted` | 只认显式共享，不继承父级 |
| `isolated` | 完全独立（个人空间默认） |

**父空间成员身份 ≠ 子空间资源权限。** 研发部成员能看到研发部共享知识，
但看不到部门成员的个人空间。

### 6.3 与检索的关系

```text
请求 → Principal → AllowedSpaceIDs（平台 ∪ 租户 ∪ 部门 ∪ 个人 ∪ 显式共享，逐层展开 inherit）
     → MySQL: space_id IN (...) OR (space_id='' AND 旧三层作用域)
     → doc_id 白名单 → Milvus doc_id in [...] → GraphRAG 同一边界
```

- 作用域仍不进 Milvus：白名单机制不变，避免重建集合；
- 历史数据（`space_id=''`）自动回落到 system/tenant/user 三层判定，
  迁移未完成时不会出现「文档突然消失」；
- 空间服务异常时**退回**三层作用域而不是放开（宁可少召回，不可越权）。

### 6.4 迁移映射

| V1 scope | V2 space |
|---|---|
| `system` | `platform` |
| `tenant` | `tenant:<tenant_id>` |
| `user` | `personal:<tenant_id>:<user_id>` |

入库时已双写（`scope` + `space_id`）。存量回填建议：

```sql
-- 幂等：只补未迁移行
UPDATE rag_documents
SET space_id = CASE scope
    WHEN 'system' THEN 'platform'
    WHEN 'tenant' THEN CONCAT('tenant:', tenant_id)
    ELSE CONCAT('personal:', tenant_id, ':', user_id)
  END
WHERE space_id = '';

-- 校验：不应再有空值
SELECT COUNT(*) FROM rag_documents WHERE space_id = '';
```

回填前先确保对应空间存在（`EnsureDefaults` 或控制台创建），
否则白名单会算出空集。建议先在只读副本上跑一遍，确认命中行数符合预期。

---

## 7. Cost Center

- `usage_records` 只追加，不 UPDATE；
- `cost_micros` 用整数微元（1 元 = 1,000,000），避免浮点累加误差；
- `price_version` 记录计价规则版本：模型调价后**不能**用新价格重算历史账单；
- 租户/主体由 Principal 决定，请求体里的 `tenant_id` 会被拒绝（防越权记账）。

记录来源 `kind`：`llm / embedding / rerank / mcp / skill / worker`。

**自动记账（P4）**：每次 LLM 调用结束，`llm.Manager` 通过 `llm.CostSink` 端口
（实现：`data/db/repo.CostSink`）往 `usage_records` 追加一条，字段取自调用时的
`identity`（租户/用户）与 `CallMeta`（task_id / agent_key / purpose）。

- 与 `llm_calls` 的分工：`llm_calls` 是**调用历史**（含 prompt 原文，可按天清理）；
  `usage_records` 是**账单**（只有数字，长期保留）。两者用途不同，不能互相替代。
- 为什么绕过 `usage.UseCase.Record`：那是「用户自助上报」，要 principal；
  worker 后台协程没有请求身份，记的是「任务跑了多少 token」——平台代记。
- `price_version` 由单价自动生成（`in=2.0000/out=8.0000`），改配置即换版本，
  避免「忘了升版本号 → 新价格重算历史账单」。
- 成本折算在整数域完成：`tokens × 单价 = 微元`（因为 tokens/1e6×单价 元 ×1e6 = tokens×单价），
  不经过浮点除法。
- 记账异步、失败只告警：账单少一条是会计问题，不能让已付费成功的调用变业务失败。

单任务成本：`GET /api/v2/usage/summary?task_id=123&group_by=kind`。
`task_id` 与 trace 的 `task.id` 属性同构，**成本与链路用同一个键串联**。

**归属维度 `ref`（P4 收尾）**：`task_id` 只覆盖「有任务行」的调用，
评测的 `model:` 目标、LLM Judge 这类调用根本没有任务，成本会变成无主的 tokens。
所以 `usage_records` 另开一对通用列 `ref_kind` / `ref_id`：

| 字段 | 取值示例 | 说明 |
|---|---|---|
| `ref_kind` | `harness_run` / `conv` | 归属对象类型（`usage.Ref*` 常量） |
| `ref_id` | `14` | 该对象的标识 |

- 与 `task_id` **并存**而不是替代：任务链路两者都写（`task_id` 供详情页，
  `ref` 供「一次评测 / 一个会话」这类跨任务聚合）。
- 单次评测成本：`GET /api/v2/usage/summary?ref_kind=harness_run&ref_id=14`；
  横向对比：`group_by=ref`（桶键为 `kind:id`，无归属的记录落空串）。
- 归属键由 `harness.WithRunRef` 在运行 context 上挂一次，顺着 ctx 传到
  `llm.Manager.charge`。它**同时**写进 `llm.CallMeta`——只挂在 harness 私有
  context key 上会被 `agentfactory.runGraph` 重建元数据时丢掉（见 §13 第 21 条）。
- `llm.WithMeta` 因此改为**按字段合并**：零值表示「不改这一项」。
  否则下游设 `TaskID`/`Purpose` 时会把上游挂的 `ref` 一起冲掉。


---

## 8. /api/v2 接口

所有接口都经过网关中间件（认证 + 限流 + Principal 注入），
**不接受**请求体里的身份字段。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v2/me` | 当前 Principal（issuer/client/subject/tenant/scopes/departments） |
| GET | `/api/v2/spaces` | 可见空间列表 |
| POST | `/api/v2/spaces` | 创建空间（`space_type`、`parent_space_id`、`inheritance_mode`） |
| GET | `/api/v2/spaces/detail?space_id=` | 空间详情 |
| PATCH | `/api/v2/spaces?space_id=` | 改名 / 改继承 / 移动（`parent_space_id`） |
| DELETE | `/api/v2/spaces?space_id=` | 删除（有子空间时拒绝） |
| GET | `/api/v2/spaces/children?space_id=` | 直接子空间 |
| GET/POST | `/api/v2/spaces/shares?space_id=` | 列出 / 新增显式共享 |
| DELETE | `/api/v2/spaces/shares?share_id=` | 撤销共享 |
| GET | `/api/v2/resources?type=&space_id=` | 资源列表（逐条判权过滤） |
| POST | `/api/v2/resources` | 登记资源（版本 1 + 首份快照） |
| GET | `/api/v2/resources/detail?resource_id=` | 资源详情 |
| PATCH | `/api/v2/resources?resource_id=` | 更新（版本 +1 + 追加快照） |
| DELETE | `/api/v2/resources?resource_id=` | 删除（保留版本历史） |
| POST | `/api/v2/resources/check` | 判权 `{"resource_id","action"}` |
| GET/POST | `/api/v2/resources/grants?resource_id=` | 列出 / 新增授权（`effect=allow|deny`） |
| DELETE | `/api/v2/resources/grants?resource_id=&principal_type=&principal_id=&action=` | 撤销授权 |
| GET | `/api/v2/resources/versions?resource_id=` | 版本历史 |
| POST | `/api/v2/usage/records` | 记录用量 |
| GET | `/api/v2/usage/summary?task_id=&ref_kind=&ref_id=&group_by=agent\|task\|ref\|model\|kind\|day` | 成本聚合（`task_id` = 单任务；`ref_kind+ref_id` = 单次评测运行） |
| GET | `/api/v2/workers` | 执行体列表（在线/排空/离线，含在跑任务数）；仅管理员 |
| GET | `/api/v2/tasks/leases?limit=` | 运行中任务的租约视图（owner / 到期 / 截止）；仅管理员 |
| GET | `/api/v2/tasks/dead-letters?limit=` | 死信任务（重试用尽）；仅管理员，按租户隔离 |
| GET | `/api/v2/tasks/detail?id=` | 任务执行视图（snapshot_id / delegated_caps / depth / attempt / trace_id），租户隔离 |
| GET | `/api/v2/traces?task_id=&name=&limit=` | 链路列表（span 树）；带 `task_id` 校验任务归属，不带则仅管理员 |
| GET | `/api/v2/traces/detail?trace_id=` | 单条 trace（完整 span 列表） |
| GET | `/api/v2/agents/snapshots?agent_id=&limit=` | Agent 配置快照列表（新→旧） |
| POST | `/api/v2/agents/snapshots` | 发布快照（内容未变复用最新一条）；仅管理员 |

错误码：`404` 不存在、`403` 无权、`400` 参数错误、`409` 冲突。
无权读与不存在同口径，避免枚举资源 ID。

---

## 9. 执行链路：注册 / 租约 / 重试

多副本部署暴露出的真实问题：任务被某个进程认领后，如果该进程被 OOM kill、
被 k8s 驱逐、机器直接断电，`agent_tasks` 里那条记录会**永远停在 running**。
进程没有机会写终态，重启扫描又只能救自己进程里的内存态。

### 9.1 三个机制

| 机制 | 表/字段 | 作用 |
|---|---|---|
| 执行体注册 | `worker_instances`（`worker_key` 唯一） | 心跳区分「活着但空闲」与「进程已经没了」；`alive` 由 `expires_at` 计算 |
| 任务租约 | `agent_tasks.lease_owner` / `lease_expires_at` | 认领即写租约，执行期间每 TTL/3 续租；进程一死续租即停 |
| 清扫协程 | `sweep`（默认 20s 一轮） | 过期租约 → `retry_wait`；超截止时间 → `timeout`；退避到期 → 回 `pending` 重新入队 |

`ClaimTask` 把「状态 CAS + 租约写入」压成一条 UPDATE：分两步写会出现
「状态已是 running、租约还是空」的窗口，清扫协程在窗口里扫到就会抢走任务。

### 9.2 状态机

```
pending ──claim──▶ running ──成功──▶ succeeded
   ▲                  │ ├──失败(允许重试)──▶ retry_wait ──退避到期──▶ pending
   │                  │ ├──失败(不重试)────▶ failed
   ├──批准续跑────────┤ ├──超时────────────▶ timeout
   │                  │ ├──审批闸门────────▶ paused
   │                  │ └──用户取消────────▶ canceled
   └──retry_wait 到期─┘
```

- `pending` 就是原文档的 `QUEUED`：沿用旧名是为了不动历史数据、前端枚举与外部调用方；
- `retry_wait` 是**唯一**的重新入队入口，不允许 `retry_wait → running`：
  直接回 running 不会累加 `attempt`，死循环任务永远撞不到上限；
- `timeout` 与 `canceled` 分开：超时率与取消率混在一起就失去观测意义。

### 9.3 重试策略（两个来源，不要混）

| 场景 | 上限来源 | 默认 | 说明 |
|---|---|---|---|
| 执行体失联 / 进程崩溃 | `Options.MaxAttempts`（`AGENT_TASK_MAX_ATTEMPTS`） | 3 | 基础设施故障，重排是唯一不丢任务的办法 |
| 业务失败（LLM 报错等） | `TaskInput.MaxAttempts`（`/tasks` 的 `max_attempts`） | 0（不重试） | 大模型失败多为确定性（提示词错、能力不存在），无脑重试只烧 token |

退避：`30s × 2^(attempt-1)`，10 分钟封顶（`RetryBackoff`）。
次数用尽 → 终态 `failed` + `dead_letter_reason=worker_lost`，任务进历史表，
`/api/v2/tasks/dead-letters` 可查；人工确认后走 v1 的重试接口复活（`attempt` 归零）。

### 9.4 运维开关

```bash
AGENT_WORKER_KEY=host-a            # 实例标识（默认 hostname-pid-随机后缀）
AGENT_LEASE_TTL=60s                # 租约有效期，执行期间自动续租
AGENT_HEARTBEAT_INTERVAL=15s       # 心跳间隔
AGENT_HEARTBEAT_TTL=45s            # 心跳有效期（>2×间隔，容忍漏一次）
AGENT_TASK_TIMEOUT=30m             # 单任务执行截止时间 → timeout
AGENT_TASK_MAX_ATTEMPTS=3          # 失联重试上限（硬顶 10）
AGENT_SWEEP_INTERVAL=20s           # 清扫间隔
```

### 9.5 工具调用幂等（防重复副作用）

重试救回任务有一个隐藏代价：如果进程死在「工具已执行、checkpoint 还没落」的
窗口里，恢复后 LLM 重发同一调用，副作用就发生两次。任务执行链路的每次工具
调用都带幂等键 `task:<task_id>:turn:<n>:<tool>:<args 摘要>`：

- **命中即回放**：`mcp_call_records` 里有同键成功记录就直接返回上次结果
  （`CallResult.Replayed=true`），不再真正调用；
- **turn 恢复自 checkpoint**（`RunState.Turn`），同一逻辑轮次在续跑后编号不变，
  这是「重发同一调用」能被拦住的前提；
- **attempt 不参与 key**：重试是同一次逻辑调用的再执行；
- **失败与 IsError 不缓存**：副作用状态未知时，重试必须有翻身的可能；
- **只拦任务链路**：管理界面手动调用、会话编排直连不带 key，不去重——
  人手动点两次就是要执行两次。

已知边界：恢复后 LLM 若生成**不同参数**（改了主意），args 摘要不同即视为新调用、
照常执行——这是语义正确的：只有「同一意图的重复」才该拦。

### 9.6 上线注意

- 老库需加列（`AutoMigrate` 开了会自动建；关掉时手工执行）：

  ```sql
  ALTER TABLE agent_tasks
    ADD COLUMN attempt INT NOT NULL DEFAULT 0,
    ADD COLUMN max_attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN lease_owner VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN lease_expires_at DATETIME NULL,
    ADD COLUMN deadline_at DATETIME NULL,
    ADD COLUMN next_attempt_at DATETIME NULL,
    ADD COLUMN dead_letter_reason VARCHAR(64) NOT NULL DEFAULT '',
    ADD INDEX idx_agent_tasks_lease (lease_owner),
    ADD INDEX idx_agent_tasks_lease_exp (lease_expires_at),
    ADD INDEX idx_agent_tasks_next_attempt (next_attempt_at);

  -- P3：配置快照与委派上下文
  ALTER TABLE agent_tasks
    ADD COLUMN snapshot_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN delegated_caps TEXT NULL;   -- NULL = 未受限，'[]' = 受限且全禁

  ALTER TABLE agent_task_history
    ADD COLUMN snapshot_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
    -- 归档必须保留委派能力：权限复盘时要回答「这个子任务当时被下放了什么」
    ADD COLUMN delegated_caps TEXT NULL;

  CREATE TABLE IF NOT EXISTS agent_snapshots (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    agent_def_id BIGINT UNSIGNED NOT NULL,
    agent_key VARCHAR(64) NOT NULL,
    version INT NOT NULL DEFAULT 0,
    checksum VARCHAR(64) NOT NULL,
    config MEDIUMTEXT NULL,
    deps TEXT NULL,
    created_at DATETIME NOT NULL,
    KEY idx_agent_snapshots_def (agent_def_id),
    KEY idx_agent_snapshots_key (agent_key),
    KEY idx_agent_snapshots_sum (checksum)
  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

  ALTER TABLE agent_task_history
    ADD COLUMN attempt INT NOT NULL DEFAULT 0,
    ADD COLUMN max_attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN dead_letter_reason VARCHAR(64) NOT NULL DEFAULT '',
    ADD INDEX idx_agent_task_history_dlq (dead_letter_reason);

  CREATE TABLE IF NOT EXISTS mcp_call_records (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    idem_key VARCHAR(190) NOT NULL,
    task_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
    tool_name VARCHAR(256) NOT NULL,
    args_hash VARCHAR(64) NOT NULL,
    ok TINYINT(1) NOT NULL,
    result_json MEDIUMTEXT NULL,
    tenant_id VARCHAR(128) NOT NULL DEFAULT 'public',
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_mcp_call_records_key (idem_key),
    KEY idx_mcp_call_records_task (task_id)
  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

  CREATE TABLE IF NOT EXISTS worker_instances (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    worker_key VARCHAR(160) NOT NULL,
    host VARCHAR(128) NOT NULL DEFAULT '',
    capacity INT NOT NULL DEFAULT 0,
    running INT NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL DEFAULT 'online',
    started_at DATETIME NOT NULL,
    heartbeat_at DATETIME NOT NULL,
    expires_at DATETIME NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    UNIQUE KEY uk_worker_instances_key (worker_key),
    KEY idx_worker_instances_status (status),
    KEY idx_worker_instances_expires (expires_at)
  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
  ```

- 多副本部署必须让各副本的 `AGENT_WORKER_KEY` 唯一（默认值已含 pid + 随机后缀）；
- 时钟要同步（NTP）：租约与心跳判活全部基于 `now()` 比较，节点间漂移超过 TTL 会误判失联。

---

## 10. Agent 快照与委派授权

### 10.1 为什么需要快照

任务提交后，执行时读的是「当前」定义。改一次提示词、换一个模型、收紧一个能力，
所有在跑的任务立刻跟着变；事后审计也答不出「这条任务当时到底用了什么配置」。

现在：创建 / 更新定义时生成一条不可变快照（`agent_snapshots`），任务提交时钉住
`snapshot_id`，执行时按快照加载 Prompt / 能力 / 模型 / 执行图。

| 设计点 | 取舍 |
|---|---|
| checksum 去重 | 抹掉 `Version`/`Enabled`/`Vectorized`/时间戳再算 sha256：启停、调度不会刷出内容相同的快照 |
| 只增不改 | 没有 Update/Delete 方法；回滚 = 用旧快照再发一次，不改写历史 |
| 快照缺失回退 | 快照读不到/解析失败 → 用当前定义执行并 warn + 落事件：快照是加固项，不能让脏数据阻断任务 |
| 主键取当前定义 | `ID`/`Enabled` 用 live 值：停用仍然阻断新执行，日志与回写不串定义 |

执行时落一条 `snapshot` 事件（`snapshot_id` / `version` / `checksum`），
trace 里直接可查「这一跑用的是哪一版」。

### 10.2 运行时二次授权

快照冻结的是**配置**，不是**权限**。任务恢复、重试、长时间运行期间，资源归属 /
授权 / 空间共享都可能已经变了，沿用提交那一刻的结论就会出现「权限已被收回，
任务还在替他调工具」。

执行边界上重新判权（`authz.go`）：

| 时机 | 资源 ID | 动作 |
|---|---|---|
| 任务开始执行 | `agent:<agent_key>` | execute |
| 工具调用 | `tool:<unique_name>` | execute |
| 技能加载 | `skill:<name>` | execute |
| 委派子 Agent | `agent:<agent_key>` | execute |

- **未登记资源放行**：`Registered` 先区分「没接入资源体系」与「明确拒绝」，
  否则全量收口前的旧资源会被一刀切拒绝（已知边界）；
- **判权存储故障 → 拒绝**：MySQL 挂了任务本来也写不进状态，不如明确失败交给租约重排；
- 知识库不走这里：检索侧白名单（Knowledge Space / doc_id 过滤）本来就是按请求实时算的。

### 10.3 委派上下文

```text
子任务有效能力
= 子 Agent 声明能力                      （父级未受限时）
= 子 Agent 声明能力 ∩ 父任务委派清单     （父级已受限时，多级链单调收紧）
再 ∩ 运行时资源授权（每次调用）
```

- 父定义里写 `agent:<child>` 的语义就是「按该 Agent 的配置使用它」；若把它解释成
  「父的白名单就是子能力上限」，所有多 Agent 编排都得让父再抄一遍成员的工具清单，
  最终只会退化成一份大而全的白名单；
- 真正的收紧发生在**链路上**：一旦父任务自己处于受限上下文（`delegated_caps` 非 nil），
  子任务必须与之求交，A ⊇ B ⊇ C 单调递减，任何一级都不能把上游收回的权限重新发放；
- `nil` = 未受限，`[]` = 受限且全禁——两者语义不同，数据库列用 NULL / `'[]'` 区分；
- 深度闸门：默认 5 层（`AGENT_MAX_DELEGATION_DEPTH`），防 A→B→A 成环烧预算；
- 归属继承：`user_id` / `tenant_id` / `conv_id` 一律取父任务，跨租户委派只能平台侧显式配置。

---

## 11. 可观测与评估（P4）

### 11.1 链路追踪：一次执行一条 Trace

埋点位置与语义（实现见 `internal/pkg/trace`）：

| Span | 起点 | 关键属性 |
|---|---|---|
| `task.run` | `executeTaskNow` | `task.id` / `task.agent_key` / `task.snapshot_id` / `task.attempt` / `task.depth` / `task.tenant_id` |
| `llm.chat` | `llm.Manager.chatWithOptOnce`（每次数据源尝试一条，含 fallback） | `llm.model`（实际使用）/ `llm.source` / `llm.purpose` / `llm.prompt_tokens` / `llm.completion_tokens` / `llm.duration_ms` |
| `mcp.call` | `mcp.UseCase.CallTool` | `tool.name` / `mcp.replayed` |
| `skill.run` | `agentfactory.skillCapability` | `tool.name` |
| `rag.retrieve` / `rag.query` | `rag.Retrieve` / `rag.Query` | `rag.hits` / `rag.cached` |
| `graphrag.query` | `rag.GraphQuery` | `graphrag.nodes` |
| `harness.run` / `harness.case` | `harness.StartRun` / `runCase` | `harness.target` / `harness.cases` / `harness.passed` / `harness.score` |

两个落点，分工明确：

- **进程内环形存储**（默认开启，容量 `OTEL_TRACE_BUFFER`）：`/api/v2/traces` 直接查。
  单机部署、内网隔离环境没有 collector，但「这条任务慢在 LLM 还是 MCP」照样要能回答。
- **OTLP/gRPC 导出**（配了 `OTEL_EXPORTER_OTLP_ENDPOINT` 才有）：送 Jaeger / Tempo，长期留存。

任务行上的 `trace_id` 由根 span 回写：内存缓冲被淘汰后，仍能拿它去 collector 查历史链路。

为什么不落 MySQL：一次任务几十条 span、带属性，写事件表会让存储膨胀一个数量级，
而价值集中在「最近一段时间」——长期留存是 collector 的职责，不是业务库的。

埋点是零成本的：未初始化追踪时 otel 全局 provider 为 noop，`trace.Start` 返回空句柄，
业务代码不需要判断「追踪开没开」。分类靠 `span.kind` 属性，漏打时按名称前缀兜底。

### 11.2 评测可复现：Run 绑定版本

`harness_runs` 新增绑定列，回答「这个分数是哪一套东西跑出来的」：

| 列 | 含义 |
|---|---|
| `snapshot_id` / `snapshot_version` | 被测 Agent 的配置快照（`model:` 目标为 0） |
| `dataset_version` / `dataset_checksum` | 数据集版本与内容哈希 |
| `model` | 实际被测模型（`agent:` 目标留空，由快照里的 LLM 配置决定） |
| `trace_id` | 本次评测运行的链路 |

数据集版本维护规则：

- `checksum` = 启用用例的 `input/context/expect/checks` 序列化后 sha256 前 16 位；
- **不含**名称、标签、ID、时间戳，也**不含用例顺序**——改名或拖排序不该让版本跳号；
- 首次运行只记录 checksum（v1 = 最初那套用例），内容确实变了才 `version+1`；
- 编辑用例集元信息（名称 / 描述 / 默认 target）不改 version / checksum。

`Runner` 端口因此多了 `Resolve(ctx, target) TargetInfo`：target 的语义只有执行器知道
（`agent:` 要去 agentfactory 查最新快照，`model:` 直接就是模型名）。解析失败返回零值、
不阻断运行——版本绑定是记录项，不是开跑的前置条件。

### 11.3 配置

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | 空 | OTLP/gRPC 端点（`host:4317`，容忍 `http(s)://` 前缀）；空 = 只留进程内存储 |
| `OTEL_EXPORTER_OTLP_INSECURE` | true | 端点是否明文（内网 collector 一般为 true） |
| `OTEL_TRACES_SAMPLE_RATIO` | 1 | 按 TraceID 概率采样，(0,1]；父级决定子级 |
| `OTEL_TRACE_BUFFER` | 512 | 进程内保留的 trace 条数 |
| `HARNESS_HEARTBEAT_SEC` | 15 | 评测运行心跳间隔（执行期续 `heartbeat_at`） |
| `HARNESS_STALE_SEC` | 90 | 判失联阈值：心跳早于 `now - stale` 的 `running`/`pending` 行被回收 |

评测的巡检周期取 `HARNESS_STALE_SEC / 2`：任何僵尸行的可见延迟不超过 1.5 倍阈值。
阈值必须显著大于心跳间隔（≥3 倍），否则一次数据库抖动就会误杀正常运行的评测。

### 11.4 上线 SQL

```sql
ALTER TABLE agent_tasks
  ADD COLUMN trace_id VARCHAR(32) NOT NULL DEFAULT '' AFTER dead_letter_reason,
  ADD KEY idx_agent_tasks_trace (trace_id);

ALTER TABLE agent_task_history
  ADD COLUMN trace_id VARCHAR(32) NOT NULL DEFAULT '' AFTER dead_letter_reason;

ALTER TABLE harness_sets
  ADD COLUMN version INT NOT NULL DEFAULT 1 AFTER is_enabled,
  ADD COLUMN checksum VARCHAR(32) NOT NULL DEFAULT '' AFTER version;

ALTER TABLE harness_runs
  ADD COLUMN snapshot_id BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER created_at,
  ADD COLUMN snapshot_version INT NOT NULL DEFAULT 0 AFTER snapshot_id,
  ADD COLUMN dataset_version INT NOT NULL DEFAULT 0 AFTER snapshot_version,
  ADD COLUMN dataset_checksum VARCHAR(32) NOT NULL DEFAULT '' AFTER dataset_version,
  ADD COLUMN model VARCHAR(128) NOT NULL DEFAULT '' AFTER dataset_checksum,
  ADD COLUMN trace_id VARCHAR(32) NOT NULL DEFAULT '' AFTER model,
  ADD KEY idx_harness_run_trace (trace_id);

-- 成本归属维度：没有任务行的调用（评测 model: 目标 / LLM Judge）靠它归集
ALTER TABLE usage_records
  ADD COLUMN ref_kind VARCHAR(32) NOT NULL DEFAULT '' AFTER task_id,
  ADD COLUMN ref_id VARCHAR(64) NOT NULL DEFAULT '' AFTER ref_kind,
  ADD KEY idx_usage_ref (ref_kind, ref_id);

-- 评测运行的存活判定：执行体失联后由巡检回收
ALTER TABLE harness_runs
  ADD COLUMN runner_id VARCHAR(64) NOT NULL DEFAULT '' AFTER trace_id,
  ADD COLUMN heartbeat_at DATETIME NULL AFTER runner_id;
```

上线后第一次巡检会把历史遗留的僵尸行收尾（`status IN ('running','pending')` 且
`COALESCE(heartbeat_at, created_at)` 早于阈值），`error` 写「执行体失联：心跳超时」。
如果希望它们显示为「人工取消」而不是失败，先手工执行一次：
`UPDATE harness_runs SET status='canceled', finished_at=NOW() WHERE status IN ('running','pending');`

历史 `agent_tasks` 行 `trace_id` 为空是正常的：那些任务在 P4 之前执行，没有链路可查。

### 11.5 排查路径（怎么用）

1. **任务卡在 running** → `/api/v2/tasks/leases` 看 owner 与租约到期，判断是执行体失联还是任务真的在跑；
2. **任务慢** → 任务详情 → 执行链路，看 `task.run` 下面哪一段占大头（LLM / MCP / RAG）；
3. **重试后结果一样却没重复副作用** → span 上 `mcp.replayed=true`，说明命中了幂等回放；
4. **账单对不上** → `/api/v2/usage/summary?task_id=` 与 trace 里的 `llm.*_tokens` 对账，
   两边来自同一批 LLM 调用。
5. **评测进度不动 / 一直 pending** → 先看进程日志有没有 `运行行落库失败`：
   `harness_runs` 的每次状态推进都是一条整行 UPDATE，任何一个字段类型不合
   （典型是 `finished_at` 的零值时间）都会让**整条** UPDATE 被拒。
6. **评测分数相邻版本可比吗** → 比 `dataset_checksum` 与 `snapshot_version`，
   两者都相同才是同一套东西；只比分数会得出错误结论。
7. **评测一直 running** → 看 `harness_runs.heartbeat_at` 是否还在动、`runner_id` 是哪台：
   心跳停了超过 `HARNESS_STALE_SEC` 会被巡检自动收为 `failed`（`error` 写「执行体失联」）。
   心跳还在动说明用例确实在跑，看 `harness.case` span 卡在哪一层。
8. **一次评测花了多少** → `GET /api/v2/usage/summary?ref_kind=harness_run&ref_id=<id>`，
   与 trace 的 `llm.chat` 条数、token 逐个对得上。

### 11.6 评测的链路与成本归属

- **一条 Trace 贯通评测与执行**：`agent:` 目标的评测会创建真实任务，链路是
  `harness.run → harness.case → task.run → llm.chat` 四层（实测一次运行 8 个 span）；
  `model:` 目标是 `harness.run → harness.case → llm.chat` 三层。
- **`harness.case` span 带判分结果**（`harness.case` / `harness.passed` / `harness.score`）：
  打开链路就能看出「慢的那一例是不是也挂了」，不必再翻报告页。
- **成本归属**：一次运行的**全部** LLM 成本都归到 `ref_kind=harness_run` + `ref_id=<runID>`：
  被测 Agent 内部调用（`agent:` 目标）、模型直连调用（`model:` 目标）、
  LLM Judge 判分调用三路共用同一个归属键。`agent:` 目标的成本**同时**带 `task_id`，
  所以既能按任务查，也能按运行查。
  查询：`GET /api/v2/usage/summary?ref_kind=harness_run&ref_id=14&group_by=kind`；
  报告页的「成本」卡片用的就是它（`Eval.jsx`）。
  实测一次 `agent:` 评测：6 次 `llm.chat` → 一条 ref 记录集，5814 in / 2855 out / 11524 微元。
- **崩溃恢复**：执行期每 `HARNESS_HEARTBEAT_SEC` 续 `harness_runs.heartbeat_at`，
  进程被强杀即停跳；巡检（启动即扫一次，之后每 `stale/2`）把超时行收为 `failed`，
  `error` 写「执行体失联：心跳超时（进程可能被强制终止）」。
  多实例安全：活着的实例心跳新鲜，不会被别的实例误收。
  心跳是**独立 UPDATE**（`TouchRun`，条件带 `status='running'`），
  不走 `SaveRun`——否则全量写会把并发的进度覆盖回旧值。
- **踩过的坑（已修）**：`Run.FinishedAt` 是值类型 `time.Time`，未结束时是零值，
  直接写进 map 更新会序列化成 `'0000-00-00'`，被 MySQL `NO_ZERO_DATE` 拒掉（1292）。
  由于调用方用 `_ =` 吞了错误，表现是**状态永远 pending、进度条一动不动**。
  现在 `SaveRun` 统一走 `nilIfZero`，所有落库点收敛到 `persistRun` 并打日志。

---

## 12. 接下来（建议顺序）

**P2 · 执行链路（已完成）**

1. ✅ `worker_instances` + 注册/心跳/Lease；任务领取带 lease 过期时间。
2. ✅ 任务状态机扩展 `pending(QUEUED) → running → retry_wait → pending`，终态加 `timeout`。
3. ✅ 崩溃恢复：心跳超时 → lease 过期 → `retry_wait` → 退避重排 → 超上限进死信。
4. ✅ 外部副作用工具调用补幂等键：任务链路调用带 `task:<id>:turn:<n>:<tool>:<args>` 键，
   命中 `mcp_call_records` 直接回放上次结果，拦住「重试后重复下单/重复发消息」。

**P3 · Agent 快照与二次授权（已完成）**

1. ✅ 发布时生成 `agent_snapshots`：定义全文 + checksum（内容去重）+ 依赖清单
   （委派目标 / 工具 / 技能）。Prompt / Skill / MCP / RAG 目前没有独立版本号，
   快照冻结的是 Agent 侧引用它们的配置。
2. ✅ Runtime 按快照加载配置，**按当前权限**做二次授权（快照不等于永久权限）。
3. ✅ 子 Agent 委派使用受限 Delegation Context：能力交集（链路上单调收紧）+ 深度闸门。

**P4 · 评估与可观测（已完成）**

1. ✅ OpenTelemetry：一次执行一条 Trace，Span 覆盖 task / LLM / MCP / Skill / RAG / GraphRAG。
   进程内环形存储 + 可选 OTLP 导出双落点，`/api/v2/traces` 按 task_id 查 span 树。
2. ✅ Evaluation Run 绑定 Agent 快照 + 数据集版本/哈希 + 模型 + Trace，保证分数可复现。
3. ✅ 成本与 Trace 通过 `task_id` 关联：LLM 调用自动写 `usage_records`，
   `group_by=task` 支持单任务成本看板。
4. ✅ 所有 LLM 调用收敛到 `Manager.callOnce`（一次真实请求 = 一条 span = 一条用量记录）：
   主链路 / 指定数据源 / conf 兜底三条路径共用同一处埋点与记账，
   同时消掉了 `chat()` 与 `chatWithOptOnce()` 两套重复的候选源循环。

**持续项**
- 各业务资源（agent/prompt/skill/mcp/kb）在创建时调用 `resource.Register`，逐步收口。
- `/api/v2` 迁移到 proto 生成（与现有 `api/*/v1` 一致），产出 OpenAPI 与前端 SDK。
- Console 增加空间树、共享、成本三个页面（当前只有接口）。

---

## 13. 已知边界（不要误用）

1. **空间授权不落到 Milvus**：白名单是 MySQL 算出来的；文档量大到白名单表达式超限时，
   需要改为「按空间分集合」或 Milvus 标量字段过滤，届时要做一次集合迁移。
2. **GraphRAG 实体不存租户**：可见性完全依赖 `chunk_refs` 里的 doc_id。
   同一实体被两个空间引用时会合并展示，来源权限仍各自生效——但社区摘要
   可能包含用户看不到的文档内容，接入 Community Summary 前必须补来源过滤。
3. **资源注册尚未全覆盖**：新接口可登记资源，但旧业务表不会自动出现。
   在全部接入前，`Check` 对未登记资源一律返回 false（默认拒绝）。
4. **`/api/v2` 未进 OpenAPI**：前端 SDK 目前不包含这些接口。
5. **显式授权不做「权限上限」推导**：授权人只要具备 `manage_permission` 就能授出
   `execute` 等动作；如需「不能授出比自己更大的权限」，要引入权限偏序表。
6. **租约判活依赖时钟**：`lease_expires_at` 与心跳全是 `now()` 比较，
   节点间漂移超过 `AGENT_LEASE_TTL` 会误判失联（后果是任务被重排、可能重复执行一次）。
   必须开 NTP；跨机房部署时把 `AGENT_LEASE_TTL` 调大到 ≥ 3 倍正常漂移。
7. **业务重试默认关闭**：`max_attempts=0` 时确定性失败不重试。想获得「失败重跑」
   必须在提交任务时显式给 `max_attempts>1`，否则只会看到一次失败即终态。
8. **死信不会自动重放**：进历史表的死信任务只能人工确认后走 v1 重试接口复活；
   没有自动重放策略（避免同一个坏任务反复消耗额度）。
9. **运行时判权是 opt-in**：只有调用过 `resource.Register` 的资源才强制二次授权；
   未登记资源放行（兼容旧数据）。全量收口前，这条防线只覆盖新接口创建的资源。
10. **快照不含依赖资源的版本**：Prompt 工程、Skill、MCP 工具都没有独立版本号，
    快照记录的是 Agent 侧配置与依赖清单；依赖资源本身变更仍会影响执行
    （MCP 工具被停用会直接失败，这是有意的——工具可用性属于运行时事实）。
11. **委派收紧只发生在链路上**：根 Agent 的 `agent:<child>` 视为「按配置使用」的授权，
    子 Agent 用它自己的能力清单执行；如果要求「根 Agent 必须逐项授权子能力」，
    需要引入显式的 Delegation Policy 表（当前未做）。
12. **幂等记录无 TTL**：`mcp_call_records` 只增不删，量级 ≈ 任务链路的工具调用数
   （不拦管理界面/会话直连）。规模变大后可按 `created_at` 定期归档，删旧记录
   只会损失「超旧任务重试」的回放保护，不影响在线链路。
13. **进程内 trace 会被淘汰**：环形缓冲只在内存，重启即清空；超出 `OTEL_TRACE_BUFFER`
   的旧 trace 查不到（返回 404「已被环形缓冲淘汰」）。要长期留存必须配
   `OTEL_EXPORTER_OTLP_ENDPOINT`，走 collector。
14. **trace 不做跨进程传播**：worker 从队列取任务时从 `Background` 起新 trace；
   子 Agent 委派在同一进程内因此是同一棵 span 树。跨执行体（多副本/多节点）
   没有 W3C `traceparent` 透传——队列消息里目前只带 task_id，可按需补。
15. **自动成本只覆盖 LLM**：embedding / rerank / MCP / Skill 的用量仍靠手动
   `POST /usage/records` 上报。所以「单任务成本」当前等于「该任务的 LLM 成本」，
   不是全量成本；`kind` 维度已预留，补埋点即可扩展。
16. **评测快照取的是 StartRun 时刻的最新版本**：运行中再发布快照不影响本次运行
   （ID 已固化），但同一批用例隔天再跑可能落在新快照上——报告里的
   `snapshot_version` 就是用来识别这种跨版本对比的。
17. **`span.kind` 分类靠约定**：新埋点忘了打这个属性时按名称前缀兜底
   （`task.` / `llm.` / `mcp.` / `rag.` / `graphrag.`），前缀也不匹配就落到 `other`。
   面板分组会变差，但不会丢数据。
18. **`model:` 目标的评测成本归属（已解决）**：这类运行不创建任务行，
   以前只能按时间窗 + 模型聚合。现在由 `usage_records.ref_kind/ref_id` 承载，
   `harness_run:<run_id>` 覆盖被测对象与判分模型两路（§7「归属维度 ref」）。
19. **评测运行的崩溃恢复（已解决）**：执行体中途退出曾让 `harness_runs` 永久停在
   `running`，只能手工 UPDATE。现在执行期心跳 + 巡检回收（§11.6），
   回收后状态为 `failed` 且 `error` 写明「执行体失联」。
   ——注意回收是**不重跑**：用例结果已写入的保留，未写入的丢失，需要重跑就新建一次运行。
20. **`llm.chat` span 与用量记录一一对应**：一次真实请求 = 一条 span = 一条用量记录
   （重试 / 降级各算一次），所以 trace 里的 `llm.chat` 条数应与
   `usage_records.calls` 相等。不相等就说明有调用绕过了 `Manager.callOnce`——
   新增调用通道必须经过它，否则既没链路也没账单（P4 首次实现就把 `chat()` 漏在外面，
   评测的模型直连调用在 trace 里完全不可见）。
21. **`ref` 只沿同步调用链传递**：归属键挂在 context 上，队列异步取起的任务
   （`agent:` 目标若走排队执行）拿不到它，那部分成本只有 `task_id` 没有 `ref`。
   要让「一次评测」跨异步任务汇总，得把 ref 写进任务行（新增列），当前未做。
22. **回收判定依赖心跳与时钟**：与任务租约同理，节点间时钟漂移超过
   `HARNESS_STALE_SEC` 会误判失联（后果是运行被标记 failed，但用例结果已落库的仍在）。
   阈值不要低于 3 个心跳周期；跨机房部署时统一放大。
