# go_agent 管理控制台 · 前端

基于 **Vite 6 + React 19** 的管理控制台，左右布局（左侧导航 + 右侧工作区），
对接同仓库上一级的 Go 后端 `../go_agent`（HTTP `:8000`，含 `/api/**` 与 `/mcp/platform`）。

无 UI 框架依赖（原生 CSS，设计令牌在 `src/styles.css` 的 `:root`），路由用 hash，刷新不丢页面。

## 页面（18 个）

| 分组 | 导航 | 说明 |
|------|------|------|
| 总览 | 仪表盘 | 文档 / 分块 / 实体 / 关系规模、LLM 用量趋势（按模型分色折线，输入实线 / 输出虚线）、最近任务、运行环境 |
| RAG 知识库 | 文档导入 | 粘贴文本或拖拽文件（PDF / Word / Excel / Markdown / 文本），解析后入库（分块 + BM25 稀疏索引） |
| | 混合检索问答 | 提问 → 混合检索 → 带引用片段的答案，展示相关度与原文 |
| GraphRAG | 图谱构建 | LLM 抽取实体 / 关系，展示新增实体 / 关系数量 |
| | 图谱问答 | 答案 + 子图可视化（SVG 环形布局）+ 实体 / 关系 / 原文证据 |
| 平台能力 | 提示词工程 | 模板版本列表、发布、激活 / 回滚、渲染预览 |
| | 记忆系统 | 短期会话、长期记忆、向量 / 图谱召回、记忆问答 |
| | 模型数据源 | 多数据源 CRUD、默认切换、连通性测试、调用历史 |
| | 技能管理 | 安装 / 启停 / 卸载、文件浏览、技能市场搜索下载、从 URL 安装技能包（zip / SKILL.md / JSON） |
| | MCP 工具 | 服务配置、探活、工具同步、语义召回、工具调用；平台内置 `platform` 服务与 7 个只读工具在此可见 |
| | AI 平台 | 千问 / 百炼 MCP 广场：平台 CRUD（千问预设填好官方路径模板）、连通性测试、目录搜索 → 一键接入成本地 MCP 服务、按 mcpCode 直接接入 |
| | 评测中心 | 评测 Harness：用例集管理（含七类断言编辑）、启动 / 停止异步运行（进行中 3s 自动轮询）、报告抽屉（逐例得分 + 断言明细 + 原始输出） |
| | Agent 工厂 | Agent 定义（能力白名单、执行图、DAG 编排）、LLM 造 Agent、能力目录 |
| | 任务中心 | 任务列表 / 详情抽屉 / 事件链路 / 子任务树 / 取消重试 / 审批（批准、驳回） |
| 会话 | 会话 | 会话目录 + 多轮对话 + 消息链路弹层（召回明细、工具调用）+ 多角色辩论（暂停 / 恢复 / 叫停、增删角色） |
| 系统管理 | 租户管理 | 多租户隔离域：新建 / 改名 / 启停 / 删除；列表带成员数、任务数与内置标记（`public` 不可删，租户键创建后不可改） |
| | 用户管理 | 成员名册：按租户 / 角色 / 状态筛选，登记与编辑成员、改角色、停用、移除；名册只做管理，不参与登录认证 |
| | 文档与系统 | 文档列表与删除（含图谱引用摘除）、连接与模型信息、系统配置 |

## 启动

```bash
# 1. 后端（新终端）
cd ../go_agent
go run ./cmd/go_agent -conf ./configs       # HTTP :8000（-conf 传目录）

# 2. 前端
cd ../frontend
npm install
npm run dev                                  # http://localhost:5173
```

开发服务器代理：`/api` → `http://localhost:8000`，无需处理 CORS。
如需指向其它后端，复制 `.env.example` 为 `.env` 并改 `VITE_PROXY_TARGET` / `VITE_API_BASE`。

## 构建与部署

```bash
npm run build      # 产物 dist/（base 为相对路径）
npm run preview    # 本地预览产物
```

生产把 `dist/` 交给 Nginx 等静态服务器，并把 `/api` 反代到 Go 后端；
`/mcp/platform` **不要**反代到公网（后端只允许回环访问，且平台数据敏感）。

## 脚本

| 命令 | 作用 |
| --- | --- |
| `npm run gen:sdk` | 由 `../go_agent/api/openapi.yaml` 生成 `src/api/generated/`（`make sdk` 同一入口） |
| `npm run test:sdk` | SDK 生成结果自检（操作数 / 命名空间） |
| `npm run smoke:pages` | 用 puppeteer-core 逐页冒烟（需先起后端与 dev server） |

## 代码结构

```text
src/
├── pages/            18 个页面（Dashboard / RagIngest / RagQuery / GraphIngest / GraphQuery /
│                     Prompts / Memory / LLM / Skills / Mcp / AIPlatforms / Eval / Agents /
│                     Tasks / Chats / Tenants / Users / Admin）
├── components/       Sidebar、UI、Markdown、MermaidBlock、Subgraph、AgentFlow、
│                     TaskDetailDrawer、ConvTraceDrawer、HistoryPanel、LLMCalls、DebateSetup…
├── api/generated/    proto → SDK（不要手改）
├── api/http.js       统一 fetch、错误处理、OPERATIONS 清单
└── styles.css        设计令牌 + 全局样式
```

## 约定

- **接口只走 SDK**：页面 import `{ rag, prompt, memory, llm, skill, mcp, agentfactory, conversation, history, aiplatform, harness, admin }`
  十二个命名空间；`src/api.js` 只保留旧签名兼容层。
- 改了 `api/*.proto` 后先 `make api && make sdk`，再改页面；不要在前端手写 URL。
- 后端错误已统一映射（`ApiError`），页面只处理提示文案，不做状态码分支。
- 图表为纯 SVG / 手写 Canvas 实现（子图、趋势图），不引图表库。
- 深色 / 浅色跟随主题令牌（`:root` 里的 CSS 变量），新增组件不要硬编码颜色。
