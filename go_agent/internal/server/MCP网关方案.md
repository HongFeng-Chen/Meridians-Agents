# 平台 MCP 端点方案与实现（`/mcp/platform`）

> 状态：**已实现**（2026-11-01）。本文以**当前代码**为准；最初「对外工具聚合网关」的想法保留在第 7 章作为二期。
> 代码位置：`internal/server/mcp.go`（协议层）、`internal/service/platform_mcp_service.go`（自注册）、
> `internal/biz/platformmcp`（工具语义）。
>
> 一句话：把平台自己的**任务中心 / 会话 / 链路 / 操作历史**做成 7 个**只读** MCP 工具，
> 挂在本进程的 `/mcp/platform`，让 Agent 工厂里的 Agent 直接查自己的执行链路，不用写 SQL、不用拼 HTTP。

---

## 1. 为什么和最初方案不一样

最初设想是「把已接入的全部第三方 MCP 工具再包一层，对外暴露成标准 MCP Server，用 `api_key` 鉴权」。
落地时改成了**平台只读数据端点**，原因：

| 维度 | 对外工具聚合网关 | 平台只读端点（实际采用） |
| --- | --- | --- |
| 谁用 | 外部客户端（Cursor / Dify / 第三方系统） | 平台内的 Agent、本机 MCP 管理器 |
| 数据敏感度 | 第三方工具，风险在「谁能调」 | 任务/会话/链路，**平台内部数据**，不能出本机 |
| 鉴权成本 | 必须做 key / 多租户 / 审计 | 回环地址即可（`isLoopback` 守卫） |
| 当前痛点 | 暂无外部接入需求 | Agent 复盘链路要靠人查库，评测 Agent 拿不到执行轨迹 |

结论：先把「Agent 能查平台数据」这件事做成，鉴权面收敛为「仅本机」，
既满足 `project-task-chain-reader` 这类 Agent 的需求，又不引入 key 管理。
工具聚合网关留二期（第 7 章）。

## 2. 分层与职责

```text
MCP 客户端（Agent / 本机 MCP 管理器）
        │  POST /mcp/platform   JSON-RPC 2.0（无状态）
        ▼
internal/server/mcp.go            ← 协议层：mcp-go 的 MCPServer + Streamable HTTP + isLoopback 守卫
        │  uc.Tools() / uc.Call(name, args)
        ▼
internal/biz/platformmcp          ← 工具语义：参数校验、条数/字符裁剪、JSON 组装；只读复用业务用例
        │
        ├─► agentfactory.UseCase（ListTasks / GetTask / TaskEvents）
        ├──► conversation.UseCase（List / Get / ListMessagesPage）
        └─► history.UseCase（List）
```

**biz 与底层方法完全共用**：平台 MCP 不写一条 SQL，全部走既有用例，因此权限、过滤、分页语义与
`/api/**` 完全一致；协议层只负责「MCP 长什么样」，业务规则仍在 biz。

`internal/service/platform_mcp_service.go` 负责第三件事：**把端点注册成平台自己的 MCP 服务**，
让工具出现在能力目录（`mcp:platform__list_tasks` 这种），Agent 才能通过能力白名单调用。

## 3. 端点与协议行为

| 项 | 值 |
| --- | --- |
| 路径 | `POST /mcp/platform`（同进程，复用 8000 端口，不新开端口） |
| 实现 | `mark3labs/mcp-go v1.5.0`：`mcpserver.NewMCPServer` + `NewStreamableHTTPServer(WithStateLess(true))` |
| 会话 | **无状态**：不签发 `Mcp-Session-Id`，一次 POST 一次应答（`application/json`） |
| 工具注册 | `mcp.NewToolWithRawSchema(name, desc, schema)`，schema 直接用 `platformmcp` 里的 JSON 原文，零二次加工 |
| `GET /mcp/platform` | 无状态模式下由库回 405（仍挂路由，保持语义完整） |
| `DELETE /mcp/platform` | 回 204（客户端关闭会话；无状态实现没有会话可关） |
| 方法支持 | `initialize` / `ping` / `tools/list` / `tools/call` / `notifications/*` 由库处理；未知方法 `-32601` |
| 错误语义 | **工具执行失败 → `result.isError = true`**（MCP 规范），不是 JSON-RPC error；未知工具由库回 `-32602` |
| 访问控制 | `isLoopback(r)`：非回环一律 `403 platform MCP 仅允许本机访问`；空 `RemoteAddr`（unix socket / 测试）放行 |

> 为什么不用 SSE：客户端与 Agent 都是「问一次答一次」，无服务端主动推送需求；无状态模式还省掉了会话清理。

## 4. 工具清单

| 工具 | 作用 | 关键参数 |
| --- | --- | --- |
| `list_tasks` | 任务列表（默认只返回根任务） | `status` / `agent_key` / `keyword` / `conv_id` / `include_children` / `limit(≤100,默认20)` |
| `get_task` | 任务详情 + 子任务列表 | `task_id`（必填）/ `include_children(默认true)` |
| `get_task_trace` | 完整执行链路：思考、工具调用与结果、委派、计划、自评、错误、终答 | `task_id` / `types[]` / `limit(≤1000,默认200)` / `max_chars(≤4000,默认600)` |
| `list_conversations` | 会话目录（标题、消息数、最近预览） | `keyword` / `limit(≤200)` / `offset` |
| `get_conversation` | 会话信息 + 消息（含 `meta` 工具链、关联 task_id） | `conv_id` / `limit(≤100)` / `before_id`（向上翻页） |
| `get_conversation_trace` | 会话内操作历史：MCP 调用 / 技能 / 记忆 / RAG / GraphRAG 召回 | `conv_id` / `kind` / `ok` / `limit(≤200)` |
| `list_operation_histories` | 统一操作历史（可按 task_id / conv_id / 类型 / 对象过滤） | `task_id` / `conv_id` / `kind` / `ref` / `ok` / `limit` |

**输出裁剪**：列表条数有硬上限；payload 统一返回**对象**，超长字段逐个截断到 `max_chars`（默认 600），
并置 `truncated: true`（保持结构可解析，不让 Agent 拿到半截 JSON）；
`get_task_trace` 额外返回 `stats{events, tool_calls, tool_errors, delegates, evaluations, errors}`，
评测 Agent 可以直接引用，不必自己数事件。

## 5. 自注册（启动即可用）

`cmd/go_agent/main.go` 的 `AfterStart` 起后台协程调 `PlatformMCPService.EnsureServer`：

1. endpoint 由 `server.http.addr` 推导：`0.0.0.0` / `::` → `127.0.0.1`，拼上 `/mcp/platform`；
2. 按服务名 `platform` 查 `mcp_servers`：不存在则创建，端点/传输/启用状态变化则更新（端口改了也能自愈）；
3. `SyncTools` 拉取 `tools/list` 落库并向量化（失败只 warn，语义召回降级但不影响按名调用）；
4. 失败重试 10 次（间隔 2s，总预算 3min）——必须等 HTTP 端口起来，因为这是**自调用**。

成功后日志：`平台 MCP 服务已就绪 endpoint=http://127.0.0.1:8000/mcp/platform`。
之后在「MCP 工具」页能看到 `platform` 服务与 7 个工具，Agent 能力白名单里可勾选 `mcp:platform__*`。

## 6. 验证

```bash
# 1) 列出工具
curl --noproxy '*' -s http://127.0.0.1:8000/mcp/platform \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'

# 2) 调一个工具
curl --noproxy '*' -s http://127.0.0.1:8000/mcp/platform \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
       "params":{"name":"list_tasks","arguments":{"limit":3}}}'

# 3) 链路（评测 Agent 主要用这个）
curl --noproxy '*' -s http://127.0.0.1:8000/mcp/platform \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call",
       "params":{"name":"get_task_trace","arguments":{"task_id":52,"max_chars":800}}}'
```

自动化测试：

```bash
go test ./internal/server/ -run 'TestInitialize|TestToolsListAndCall|TestUnknownMethod|TestLoopbackGuard' -v
go test ./internal/biz/platformmcp/ -v
```

覆盖点：initialize 版本协商、tools/list 数量、tools/call 成功与业务错误（`isError`）、未知方法 `-32601`、
非回环 403、参数裁剪与截断。

## 7. 二期：对外工具聚合网关（未实施）

最初方案里仍然有效的部分，留作扩展设计（**当前代码没有 `/mcp`，也没有 `api_key`**）：

- 端点：`POST /mcp`（工具聚合），与 `/mcp/platform` 并存；同样走 mcp-go，传输仍用无状态 Streamable HTTP。
- 工具范围：`biz/mcp` 里 `enabled = true` 的全部工具，`tools/list` 直接透传库里的 `inputSchema`。
- 调用管线：`uc.CallTool(ctx, 0, uniqueName, args)`，复用启用校验、能力白名单、`op_histories` 留痕。
- 鉴权：`Authorization: Bearer <api_key>` / `X-Api-Key`，配置放 `server.mcp_gateway.api_key`（空 = 全拒），
  401 返回 JSON-RPC error；后续再考虑按 key 隔离 namespace（多租户）。
- 仍未解决：外部调用的 `conv_id` 为空（无会话上下文），需要在 `Record.Ref` 里补 `gateway:<client>` 以区分来源。

真要做时，只需在 `internal/server/mcp.go` 里再加一个 `newGatewayHandler`（复用现成的工具注册循环），
不需要动 biz——这正是当初「协议皮 + 共用 biz」的设计目的。
