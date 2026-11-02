# Meridians · 经纬

**Go 实现的一体化智能体平台** —— 给知识定坐标，给智能体定分寸。

经线是 RAG 混合检索，纬线是 GraphRAG 图谱推理，交叉处就是每个 Agent 的坐标：Agent 以数据而非代码定义，提示词版本化后可被打分、优化，并由评测 Harness 把真实得分回写进版本历史。

## 仓库结构

| 目录 | 说明 |
|---|---|
| [`go_agent/`](./go_agent) | 后端单体（Go 1.26 · Kratos v3）：RAG / GraphRAG 双知识线、Agent 工厂（12 种执行图）、提示词工程与评测闭环、MCP 工具接入。完整文档见 [go_agent/README.md](./go_agent/README.md) |
| [`frontend/`](./frontend) | 管理控制台（React 19 · Vite 6）：会话、知识库、Agent 工厂、提示词工程、评测中心、LLM 观测 |

## 快速开始

```bash
# 后端（依赖 MySQL / Redis / Milvus，见 go_agent/deploy/compose.yaml）
cd go_agent && make api && go run ./cmd/go_agent

# 前端
cd frontend && npm install && npm run dev
```

配置与详细说明：[go_agent/README.md](./go_agent/README.md)

## 许可证

MIT
