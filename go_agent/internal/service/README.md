# service · 传输适配层

一个文件对应一个 proto service，职责固定：**协议 ↔ 领域模型转换 + 参数校验**，
不做业务（业务在 `internal/biz`），不碰存储（存储接口在 `internal/data`）。

## 与服务注册的关系

方法签名由 `api/<domain>/v1/*.proto` 生成，本包只负责实现：

```go
type RagService struct {
	v1.UnimplementedRagServiceServer   // gRPC 前向兼容要求
	uc  *rag.Rag
}

var (
	_ v1.RagServiceHTTPServer = (*RagService)(nil)   // REST
	_ v1.RagServiceServer     = (*RagService)(nil)   // gRPC
)
```

- 路由不由本包声明，`internal/server/http.go` 用 `RegisterXxxServiceHTTPServer` 注册，
  `internal/server/grpc.go` 用 `RegisterXxxServiceServer` 注册；
- 同一份实现因此同时提供 REST 与 gRPC，行为、校验、错误码一致；
- proto 加方法后如果忘了实现，HTTP 接口的方法集缺项会直接编译失败。

## 约定

1. **错误用 Kratos error**：`badRequest / notFound / conflict / internalErr`
   （见 `errors.go`），不要自己写状态码。领域错误用 `mapXxxErr` 映射。
2. **时间统一 RFC3339**：`t.Format(timeLayout)`。
3. **计数用 int32**：proto 里除 `id` / `duration` 外都是 int32，转换时显式强转。
4. **字段少就是不改**：proto3 的 `repeated` 没有 presence，`len == 0` 视为「不修改」，
   指针字段（`optional`）直接透传给 biz 的 `UpdateInput`。
5. **文件上传走 `internal/pkg/httpx`**：`httpx.ResolveFile` 同时兼容
   HTTP 的 multipart 与 gRPC 的 bytes+name。
6. **管理面写操作走 `requirePerm`**：`guard.go` 把「角色 → 权限」判定收敛到
   `authz.Allowed`（矩阵在 `internal/pkg/authz`），拒绝时统一 403 + `denied` 审计。
   handler 里不要再自己比较角色名；只读的管理接口用 `audit:read`，写用
   `platform:config` / `agent:develop` / `task:approve` 等。
7. **列表/统计要带租户**：`authScope(ctx)` / `auditScope(ctx)` 决定「看自己还是看全部」，
   租户维度用 `auditTenant(ctx)`；biz 的查询方法签名把 `(tenantID, userID)` 显式传下去。

## 测试

- `contract_test.go`：把生成代码真实注册出的路由与 `api/openapi.yaml` 逐条比对，
  并校验「静态路径必须排在 `{param}` 之前」；
- 业务逻辑的测试在各 biz 包里，本包只做冒烟（起 server 打真实 HTTP）。

## 例外：平台 MCP 服务（非 proto）

`platform_mcp_service.go` **不对应任何 proto**，是 `/mcp/platform` 的适配层：

| 方法 | 作用 |
| --- | --- |
| `NewPlatformMCPService` | 由 `server.http.addr` 推导 endpoint（`0.0.0.0`/`::` → `127.0.0.1`），拼 `/mcp/platform` |
| `Endpoint()` | 自调用地址，供注册与日志使用 |
| `EnsureServer(ctx)` | 幂等注册内置 MCP 服务（`name=platform`, `transport=http`）并 `SyncTools`；端口变化会更新端点 |

- 协议实现（JSON-RPC / tools/list / tools/call / 回环守卫）在 `internal/server/mcp.go`；
- 工具语义在 `internal/biz/platformmcp`——**新增只读工具改那里**，不要在本包写业务或 SQL；
- 启动时由 `main.newApp` 的 `AfterStart` 调 `EnsureServer`（等 HTTP 端口起来，失败重试 10 次）。
