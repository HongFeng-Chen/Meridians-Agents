# Meridians · 经纬

**Go 实现的一体化智能体平台** —— 给知识定坐标，给智能体定分寸。

经线是 RAG 混合检索，纬线是 GraphRAG 图谱推理，交叉处就是每个 Agent 的坐标：Agent 以数据而非代码定义，提示词版本化后可被打分、优化，并由评测 Harness 把真实得分回写进版本历史。知识库支持**三层作用域**——平台通用（system）/ 租户共享（tenant）/ 个人私有（user），`scope + user_id + tenant_id` 三元组决定谁看得见、谁能写，检索白名单与 GraphRAG 召回边界自动跟随。

V2 把「能跑」补成「能放心跑」：多副本 worker 租约与崩溃恢复、工具调用幂等、Agent 配置快照与运行时二次授权、多级委派收紧；每次执行一条 OpenTelemetry Trace、每次 LLM 调用自动记账（`task_id` / `ref` 双归属），评测运行绑定快照与数据集版本并可自愈。现状、上线 SQL 与已知边界见 [`go_agent/docs/architecture-v2.md`](./go_agent/docs/architecture-v2.md)。

**在线文档：<https://ai.meridians.com.cn>** —— 从 RAG、GraphRAG、Agent 工厂到提示词工程、评测 Harness、部署运维的完整说明。

## 仓库结构

| 目录 | 说明 |
|---|---|
| [`go_agent/`](./go_agent) | 后端单体（Go 1.26 · Kratos v3）：RAG / GraphRAG 双知识线（三层知识作用域）、Agent 工厂（13 种执行图，含自动编排）、提示词工程与评测闭环、MCP 工具接入。完整文档见 [go_agent/README.md](./go_agent/README.md) |
| [`frontend/`](./frontend) | 管理控制台（React 19 · Vite 6）：会话、知识库（按作用域筛选）、Agent 工厂、提示词工程、评测中心、LLM 观测、MCP 服务与工具 |
| [`../k8s-mcp`](../k8s-mcp) | 配套服务：Kubernetes MCP Server，让 LLM 安全地看集群、有分寸地改集群（13 个工具，只读默认 + 变更审计），通过「MCP 服务与工具」页接入 |

## 快速开始

```bash
# 后端（依赖 MySQL / Redis / Milvus，见 go_agent/deploy/compose.yaml）
cd go_agent && make api && go run ./cmd/go_agent

# 前端
cd frontend && npm install && npm run dev
```

配置与详细说明：[go_agent/README.md](./go_agent/README.md)

## 配套服务：k8s-mcp

[k8s-mcp](../k8s-mcp) 是独立的 Kubernetes MCP Server（与本仓库同级目录）：MCP 只是命令翻译 + 安全校验层，查询走 Informer 缓存，变更受 RBAC / 命名空间黑名单 / 镜像白名单 / 副本上限多重约束并全量审计。

在控制台「MCP 服务与工具」页接入 `http://127.0.0.1:8080/mcp`（Bearer 鉴权）后，13 个工具自动向量化入库，Agent 即可语义召回调用。本机接入需设 `EGRESS_ALLOW_PRIVATE_HOSTS`（见 go_agent/README.md「出站访问策略（防 SSRF）」）。本地 kind 集群连通性、kubeconfig 修复步骤见 [k8s-mcp/README.md](../k8s-mcp/README.md)。

## 许可证

MIT
