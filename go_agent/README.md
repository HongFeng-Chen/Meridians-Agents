# Meridians · 经纬

**Go 实现的一体化智能体平台**：把「知识怎么进来、智能体怎么造、提示词怎么变好」这三件通常散落在四五个系统里的事，做成一条闭环。

文档经 RAG 混合检索与 GraphRAG 多跳推理两条线并行入库；Agent 以数据而非代码定义，由 12 种执行图驱动，可用 LLM 元编程批量生成；提示词版本化后可被 LLM 打分、优化，并在评测 Harness 中用真实用例拿回得分、回写进版本历史。配套完整 React 管理控制台。

> **给知识定坐标，给智能体定分寸。**
>
> 经线是检索，纬线是图谱，交叉处就是每个 Agent 的坐标。代码模块名沿用 `go_agent`。

`Go 1.26` · `Kratos v3` · `gRPC / HTTP` · `MySQL` · `Redis` · `Milvus` · `mcp-go` · `React 19` · `Vite 6` · `MIT License`

**在线文档：<https://ai.meridians.com.cn>**

---

## 目录

- [为什么是它](#为什么是它)
- [核心能力](#核心能力)
- [架构](#架构)
- [技术栈](#技术栈)
- [快速开始](#快速开始)
- [配置](#配置)
- [API 一览](#api-一览)
- [项目结构](#项目结构)
- [开发](#开发)
- [测试](#测试)
- [Roadmap](#roadmap)
- [许可证](#许可证)

---

## 为什么是它

- **协议即实现**：所有 REST 路由由 `api/*/v1/*.proto` 的 `google.api.http` 注解生成，新增接口只改 proto 再 `make api`；前端 SDK 从 `openapi.yaml` 自动生成，契约单一来源。
- **Agent 是数据不是代码**：角色、能力、工具绑定全部是数据定义，支持 LLM 元编程"造 Agent"，配配额护栏，不写一行业务代码即可扩充智能体。
- **提示词即配置**：模板版本化，发布即生效、可回滚、可预览渲染；调提示词不用改代码、不用发版。
- **两条知识线并行**：RAG 分块向量检索 与 GraphRAG 图谱推理 对同一批文档可并行摄取、独立检索，会话内自动组合召回。
- **工程纪律**：严格单向分层（`service → biz → data`，biz 只依赖接口）、异步落盘、best-effort 召回不阻断对话、每次调用全链路 JSON 可回溯。

## 核心能力

### RAG 知识库

- 文档解析：`pdf / docx / xlsx / md / txt`（`internal/pkg/loader`）
  - PDF 走**双引擎**：优先本机 `python + pdfplumber`（中文年报/财报这类 CID 字体 + 表格混排的 PDF，
    纯 Go 库会把行序读反、产生 ◆ 占位符），结果乱码率 > 2% 或调用失败时自动退回纯 Go 抽取；
    `RAG_PDF_PYTHON`（指定解释器）/ `RAG_PDF_ENGINE=pymupdf` / `RAG_PDF_DISABLE_PYTHON=1`（强制纯 Go）可覆盖
- 分块入库：600 字 / 100 字重叠，checksum 幂等去重 + Redis 入库锁
- 混合检索：Dense（HNSW 稠密索引）+ BM25（稀疏索引）双路召回，RRF 融合
- 问答结果缓存、命中引用溯源

### GraphRAG 知识图谱

- LLM 抽取实体 / 关系，块内合并去重 → 跨文档合并（Milvus 图集合，并发抽取信号量 4）
- 查询：实体混合检索得种子 → 沿关系边多跳扩展（默认 2 跳 / 200 边）→ 子图 + 回查原文证据 → 组装上下文
- 与主 RAG 线路互不影响，可并行摄取

### Agent 工厂

- Agent 定义即数据：`agent_definitions` 表 + 向量化语义召回
- **12 种执行图**：ReAct / Plan-Execute / Reflexion / Orchestrator / 多 Agent 协作 / Debate 等，DAG Flow 工作流编排
- LLM 元编程造 Agent + 配额护栏（上限 200，防自举失控）
- **默认模型数据源**：Agent 可指定 `llm_source`（llm 数据源名），所有轮次优先走该源；不可用或调用失败自动回退 default 链路（`llm.Manager.ChatSource`），任务不中断
- **技能即能力**：能力目录含四类——`factory`（元编程）/ `agent:`（委派）/ `mcp:`（工具）/ `skill:`（技能）；
  调用技能 = 把该技能的 SKILL.md 工作流说明加载进上下文；编辑 Agent 的白名单支持按类型 Tab + 检索 + 翻页选择
- **Token 治理与生成质量**：JSON 模式（`response_format json_object`，老网关自动降级）消灭「输出不合规
  自纠轮」；工具循环滚动摘要（早期轮次压缩成一行）+ 同任务内相同调用去重 + 上下文预算熔断；系统提示词
  「稳定前缀在前」利于前缀缓存；温度按用途分级（规划 0.2 / 执行 0.3 / 抽取评审 0 / 生成 0.7）；RAG 召回
  后可选精排（`data.rerank`，Cohere/百炼双协议，失败静默降级）；LLM 页 token 下钻排行（按 Agent/场景/
  用途/模型）
- **提示词评测闭环**：harness 运行可标注 `prompt_key/prompt_version`，终态自动把加权得分回写
  `prompt_version_scores`；提示词工程页版本历史直接展示每版得分并可一键发起评测
- **评测 Harness**：用例集（被测对象 `agent:<key>` 走执行图 / `model:<name>` 直连）→ 异步运行（并发池、单例超时、
  边界取消）→ 七类断言（contains / regex / json_valid / 长度 / LLM 评审）→ 加权得分报告；前端「评测中心」全程可视化
- **提示词工程（Prompt Studio）**：Agent 管理界面的角色提示词支持 LLM 打分（五维评分 + 问题清单 + 建议）、
  按职责描述生成、一键优化（改后稿 + 改动说明 + 前后分）；走 `llm.Manager` 可指定数据源，失败回退 default
- 异步任务体系：Redis 队列 + 协程 worker 池 + Redlock 崩溃恢复 + cron 定时调度
- **优雅停机**：停止服务时先停止领取新任务，等执行中的任务跑完（`shutdownTimeout`，默认 20s）再退出；超时任务保留 `running`，下次启动自动重跑，不丢任务、不误标取消
- 完整任务链路：父/子任务树、事件流实时追踪、轮次预算（默认 12）
- 会话内可调用：Agent 目录非空时作为首选工具委派（会话 id = 任务 id）

### MCP 工具接入

- 服务生命周期管理：stdlib（stdio）/ HTTP 双传输、心跳探活、工具同步
- 工具语义召回（Milvus `_tools` 集合）+ 热点缓存（Redis）
- 平台内置 MCP 端点 `/mcp/platform`：7 只读工具（任务中心 / 会话 / 链路 / 操作历史），基于 `mark3labs/mcp-go` 的 Streamable HTTP 无状态模式，**仅允许本机回环访问**
- 统一操作历史：每次 MCP 调用留痕

### AI 平台接入（千问 / 阿里云百炼）

- 平台登记成数据源：`kind`（qwen / bailian / generic）+ 网关地址 + API Key，配置落 MySQL（`ai_platforms`），**API Key 只进不出**
- 从平台目录搜索 MCP 服务并一键接入本地：复用 `biz/mcp` 的「创建服务 → 连接 → 同步工具（向量化）」，失败回滚删除
- 目录接口不可用时可**按 mcpCode 直接接入**：只依赖路径模板（千问 `https://dashscope.aliyuncs.com/api/v1/mcps/{code}/mcp`）
- 传输降级：平台给 SSE 时自动落成 Streamable HTTP 并在结果里说明（本地 MCP 客户端只支持 Streamable HTTP）
- 技能侧兼容：填了「技能源地址」会自动登记同名技能市场；只给下载链接的技能包走 `POST /api/skills/import-url`（zip / 单个 SKILL.md / JSON）

### 记忆系统

- 短期：Redis TTL 会话记忆（自动续期）
- 长期：MySQL 存储 + Milvus 向量召回
- 记忆图谱：复用 GraphRAG 集合，支持图谱召回

### 提示词工程

- 模板版本化：发布即生效、可回滚、可预览渲染
- MySQL 存储 + Redis 生效版本缓存；内置模板种子（已存在则跳过，不覆盖线上改动）

### LLM 数据源

- 多数据源并存（OpenAI 兼容协议），按场景路由，失败自动切换
- 调用历史与 token 用量统计：按模型聚合、任务关联、时间范围筛选

### 会话与辩论

- 多轮对话：记忆 / 文档 / 图谱 三路上下文召回 + Agent / MCP 工具委派，最多 6 轮自主编排，每轮链路 JSON 落库
- 多角色辩论：后台 goroutine 自主推演，暂停 / 恢复 / 叫停、动态增删角色、结束后中立汇总

### 技能管理

- 技能安装 / 启停 / 卸载，技能市场搜索与下载，启用后可落盘供运行时读取
- 从 URL 安装技能包：zip 按包内结构入库、单文件当 SKILL.md 正文、JSON 包按字段解析；私有链接可带 `Authorization: Bearer`

### 统一操作历史

- 5 类"用到了什么"留痕：MCP 调用 / 技能使用 / 记忆召回 / RAG 召回 / GraphRAG 召回

### 管理控制台（`frontend/`）

15 个页面：仪表盘、RAG 文档导入、RAG 混合检索问答、GraphRAG 图谱构建、图谱问答、提示词工程、记忆系统、模型数据源、技能管理、MCP 服务与工具、AI 平台（千问 / 百炼）、Agent 工厂、任务中心、会话（含辩论）、文档与系统。

<!-- 截图待补充：仪表盘 / RAG 问答 / GraphRAG 图谱 / Agent 任务中心 / 会话链路 -->

## 架构

### 分层与依赖方向

```text
api/*/v1 (proto) ──► internal/service ──► internal/biz ──► internal/data
   HTTP/gRPC           参数翻译/映射        业务用例/接口       MySQL·Redis·Milvus·MCP·技能
```

`biz` 层只依赖接口（`ports.go`），禁止 import `internal/data`；具体实现由 wire ProviderSet 拼装。

### 模块地图

| 模块 | 一句话职责 | 对外入口（HTTP） | 主要存储 |
| --- | --- | --- | --- |
| `conversation` | 会话目录 + 消息 + LLM 自主编排 + 多角色辩论 | `/api/conversations/**` | MySQL |
| `agentfactory` | Agent 低代码工厂：定义 / 任务 / 12 种执行图 / worker / cron | `/api/agentfactory/**` | MySQL · Milvus `_agents` · Redis |
| `rag` | 文档入库 + 混合检索问答 + GraphRAG 图谱 | `/api/ingest` `/api/query` `/api/graph/**` | MySQL · Milvus · Redis |
| `memory` | 短期（Redis）+ 长期（MySQL+Milvus）+ 记忆图谱 | `/api/memory/**` | MySQL · Milvus `_memories` · Redis |
| `prompt` | 提示词模板版本化 + 在线生效 + 渲染 | `/api/prompts/**` | MySQL · Redis |
| `mcp` | MCP 服务 / 工具生命周期、语义召回、调用 | `/api/mcp/**` | MySQL · Milvus `_tools` · Redis |
| `llm` | 多数据源管理 + 场景路由 + 故障切换 + 调用历史 | `/api/llm/**` `/api/llm/calls` | MySQL |
| `skill` | 技能安装 / 启停 / 卸载 + 技能市场 + 落盘 + URL 安装 | `/api/skills/**` `/api/skillhub/**` | MySQL · 磁盘 |
| `aiplatform` | 外部 AI 平台登记 + 目录搜索 + MCP 一键接入 + 技能市场联动 | `/api/aiplatform/**` | MySQL |
| `history` | 统一操作历史（5 类"用到了什么"） | `/api/histories` | MySQL |
| `embedder` | OpenAI 兼容 Embedding 客户端 | —（内部能力） | — |

### 设计原则

- **上下文传身份，不改业务签名**：任务 / 会话身份通过 `history.WithTask` / `history.WithConv` 放进 ctx；
  两者都是「读改写」，可任意叠加，worker 里派生出的 ctx 依然能带上来源会话（`conv_id` 不再为空）。
- **召回 best-effort**：记忆 / RAG / GraphRAG 任一失败只写 `meta.*_error`，不阻断对话。
- **写历史 / 写日志一律异步**：失败只 warn，一次存储抖动不拖慢主链路。
- **优雅停机先排空再退出**：`BeforeStop` 调 `agentfactory.Drain`（停止领新任务 + 等执行中任务），
  `AfterStop` 兜底 `Stop`；排空等待只看 worker，不等 cron 调度器。
- **protojson 零值省略**：新 proto 字段默认值即旧行为，保证老前端不丢字段。
- **哨兵错误**：业务错误用 `errors.Is` 映射 HTTP 状态码，不做字符串比较。

## 技术栈

| 层 | 选型 |
| --- | --- |
| 语言 / 框架 | Go 1.26 · Kratos v3（HTTP + gRPC 双传输） |
| API | protobuf-first（buf 生成）· OpenAPI · 前端 SDK 自动生成 |
| 依赖注入 | google/wire |
| 元数据 | MySQL 8.4（GORM） |
| 缓存 / 队列 / 锁 | Redis 8.x（队列 LPUSH/BRPOP · Redlock · TTL 缓存） |
| 向量库 | Milvus 3.x（Dense HNSW + BM25 稀疏双索引，派生集合自动建） |
| Embedding | OpenAI 兼容（默认 Ollama `bge-m3`，1024 维） |
| LLM | OpenAI 兼容多数据源（默认 DeepSeek），场景路由 + 故障切换 |
| MCP | 客户端自研（stdio / HTTP JSON-RPC）· 平台端点用 `mark3labs/mcp-go`（Streamable HTTP，无状态） |
| 前端 | React 19 · Vite 6 · 纯静态（Nginx 托管，hash 路由） |

## 快速开始

前置依赖：Docker（推荐）、Go 1.26+、Node.js 18+。

### 方式一：容器跑依赖 + 本地开发（推荐）

仓库根目录的 `docker-compose.yml` 负责基础设施（etcd / MinIO / Milvus / MySQL / Redis）：

```bash
# 启动依赖（全部 healthy 才算就绪：docker compose ps）
docker compose up -d

# 可选：本地 embedding（Ollama bge-m3）
docker compose --profile embed up -d ollama
docker compose exec ollama ollama pull bge-m3
```

后端（新终端）：

```bash
cd go_agent
go run ./cmd/go_agent -conf ./configs    # HTTP :8000 · gRPC :9000 · 健康检查 /api/health
```

前端（新终端）：

```bash
cd frontend
npm install
npm run dev                                # http://localhost:5173，/api 代理到 :8000
```

### 方式二：全容器化

取消根目录 `docker-compose.yml` 中 `go_agent` / `web` 两个服务的注释后：

```bash
docker compose up -d --build
# 入口 http://localhost:8080 · 健康检查 http://localhost:8000/api/health
```

> 注意：后端启动硬依赖 MySQL / Redis / Milvus 三者可达，任一不可达会直接 panic 并给出排查提示。LLM / Embedding 不配置也能启动，只是相关功能不可用（启动时打 warning）。

### 停机行为（优雅停机）

`Ctrl+C` / `SIGTERM` 后按顺序执行：

1. `BeforeStop` → `agentfactory.Drain(ctx)`：`draining` 置位，**不再领取新任务**，唤醒阻塞在队列上的 worker；
   此时 HTTP 仍在服务，任务状态照常可查。
2. 等待执行中的任务跑完，上限取 `agentfactory.shutdownTimeout`（默认 20s）与 ctx 剩余时间的较小值；
   日志 `优雅停机完成：执行中的任务已全部结束`。
3. 超时则 `AfterStop` 兜底 `Stop()` 强制取消：被打断的任务**保持 running 不写终态**，下次启动由
   `RequeueInterrupted` 重新入队执行（日志 `优雅停机等待超时…下次启动会自动重跑`）。

因此部署时把 `shutdownTimeout` 设得比编排层 kill 期限小（k8s 默认 30s），给排空留余量。

## 配置

配置文件：`configs/config.yaml`，支持 `${ENV:default}` 环境变量展开，密钥一律用环境变量注入、不落明文。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DATABASE_SOURCE` | `root:root@tcp(127.0.0.1:3306)/go_agent?...` | MySQL DSN |
| `REDIS_ADDR` / `REDIS_PASSWORD` | `127.0.0.1:6379` / `redis_pass` | Redis 地址 / 密码 |
| `MILVUS_URI` / `MILVUS_TOKEN` / `MILVUS_COLLECTION` | `http://127.0.0.1:19530` / `root:Milvus` / `rag_docs` | 向量库连接与主集合名 |
| `EMBED_BASE_URL` / `EMBED_API_KEY` / `EMBED_MODEL` | `http://localhost:11434/v1` / `ollama` / `bge-m3` | Embedding（OpenAI 兼容） |
| `LLM_BASE_URL` / `LLM_API_KEY` / `LLM_MODEL` | `https://api.deepseek.com` / 空 / `deepseek-flash` | LLM 默认数据源 |
| `SKILL_SYNC_DIR` | `./data/skills` | 技能落盘目录 |
| `SKILLHUB_BASE_URL` / `SKILLHUB_TOKEN` | 空 | 技能市场地址 |

检索 / 图谱 / 记忆 / Agent 工厂参数（分块大小、topK、图跳数、worker 并发、Agent 配额等）均可在 `configs/config.yaml` 的 `rag` / `memory` / `agentfactory` 段调整。

几个和线上行为强相关的参数：

| 配置项 | 默认 | 说明 |
| --- | --- | --- |
| `server.http.timeout` / `server.grpc.timeout` | `300s` | Kratos 全局请求超时。入库要跑 embedding + LLM、图谱抽取更慢，所以放宽到 5min |
| `data.llm.httpTimeout` | `120s` | 单次 LLM 调用超时（不跟随全局 300s） |
| `agentfactory.workerCount` | `2` | 任务 worker 并发数 |
| `agentfactory.shutdownTimeout` | `20s` | 优雅停机等待上限，**要小于编排层 kill 期限**（k8s `terminationGracePeriodSeconds` 默认 30s） |

### 安全与配额（`security` 段）

网关层有三道闸门，都在 `configs/config.yaml` 的 `security` 段，可用环境变量覆盖：

| 项 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| 认证 | `AUTH_ENABLED` / `AUTH_JWT_SECRET` | `false` / 空 | 校验 `Authorization: Bearer <JWT HS256 或静态令牌>`，把用户/租户写进请求上下文；`/healthz`、`/metrics`、`/api/files/` 等白名单路径放行 |
| 限流 | `RATELIMIT_ENABLED` | `false` | 按「租户 × 路由」令牌桶，读写分开配额；超限返回 `429` + `Retry-After` |
| 预算 | `BUDGET_MAX_TOKENS` / `BUDGET_MAX_CALLS` / `BUDGET_MAX_PARALLEL` | `400000` / `80` / `4` | 单任务树（含派生子任务）的 token 与调用次数上限，超限走「预算耗尽」收敛分支；并行波次并发上限防止宽 DAG 同时烧钱 |
| 计价 | `PRICE_INPUT_PER_M` / `PRICE_OUTPUT_PER_M` | `1` / `2` | 元 / 百万 token，用于 `/metrics` 的成本估算 |

**默认关闭认证是为了本地开箱即跑**。启动日志会显式告警：

```
level=WARN msg="认证未启用：任何能访问本端口的人都可以读写全部会话/记忆/Agent 定义并消耗你的 token…"
```

对外暴露端口前至少做这一步：

```bash
export AUTH_ENABLED=true
export AUTH_JWT_SECRET="$(openssl rand -hex 32)"
./bin/go_agent -conf configs
```

签发 JWT（HS256，`sub` 为用户、`tenant_id` 为租户）：

```bash
python - <<'EOF'
import base64, hashlib, hmac, json, time
b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=")
head = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
body = b64(json.dumps({"sub": "alice", "tenant_id": "acme", "exp": int(time.time()) + 86400}).encode())
sig = b64(hmac.new(b"你的密钥", head + b"." + body, hashlib.sha256).digest())
print((head + b"." + body + b"." + sig).decode())
EOF

curl -H "Authorization: Bearer <token>" http://localhost:8000/api/conversations
```

内网脚本不想签 JWT，可以配静态令牌（`security.auth.staticTokens`，值格式 `user:tenant`）：

```yaml
security:
  auth:
    staticTokens:
      svc-token: "svc:ops"
```

控制台侧：浏览器 `localStorage.meridians_token` 写入令牌后，前端所有请求自动带 `Authorization`（未启用认证时后端忽略该头）。

### 数据隔离（多用户）

认证只解决「你是谁」，隔离解决「你能看到谁的东西」。四类数据都带归属键，读写两端都校验：

| 数据 | 归属列 | 写入 | 读取 / 操作 |
| --- | --- | --- | --- |
| 会话 `conversations` | `user_id`（默认 `'default'`，带索引） | `Create` 从请求上下文取 `identity.UserID` | `List` 按用户过滤；详情 / 改 / 删 / 发消息 / 辩论全部接口做归属校验，跨用户 `403` |
| 任务 `agent_tasks` / `agent_task_history` | `user_id` | `SubmitTask` 取上下文；子任务与编排成员继承父任务归属；cron 走 `baseCtx` → 落 `default` | 列表按用户过滤；详情 / 事件流 / 取消 / 重试做归属校验 |
| 知识库 `rag_documents` + Milvus 分块 | `user_id` | `Ingest` 固化归属，`doc_id` 被他人占用时直接拒绝覆盖 | 列表按用户过滤；检索用 MySQL 反查的 `doc_id` 白名单拼成 Milvus 表达式 `doc_id in [...]` 下推；图谱召回按 `chunk_refs` 前缀裁剪（无 refs 保守丢弃）；删除做归属校验 |
| 长期记忆 `memories` / `recall_logs` | `user_id` | 召回命名空间 = 当前用户 | 记忆 API 在认证态强制用令牌身份，忽略请求体 `user_id`（防伪造） |

两个要点：

- **客户端传的 filter 一律不认**。`POST /api/query` 的 `filter` 字段由服务端按当前用户重新生成，既防越权检索，也顺带堵掉 Milvus 表达式注入。
- **历史数据兼容**。列默认 `'default'`，未启用认证时 `devUser=default`，老库老数据照常可见；一旦开会话/任务/文档都由令牌身份写入，天然分到各自作用域。

验证方式（起一个开了认证 + 双静态令牌的独立实例，22 条断言覆盖越权矩阵）：

```bash
python scripts/make_isolation_config.py          # 生成 configs-isolation/config.yaml（含明文测试令牌，已 gitignore）
./bin/go_agent.exe -conf ./configs-isolation &   # 8011 端口，令牌 tok-alice→alice / tok-bob→bob
python scripts/e2e_isolation.py                  # 会话 / 任务 / 知识库三层越权验证
```

**运维端点**

| 端点 | 说明 |
| --- | --- |
| `GET /healthz` | 并发探测 MySQL / Redis / Milvus，全通过 `200`，任一失败 `503`（可直接做 LB readiness） |
| `GET /metrics` | Prometheus 文本格式：请求量/时延、`llm_tokens_total{model}`、`llm_cost_cny_total{model}`、`llm_provider_available`、限流拒绝数等 |

## API 一览

| 前缀 | 说明 |
| --- | --- |
| `/api/health` | 健康检查（无 I/O，恒 200） |
| `/api/ingest` `/api/query` `/api/graph/**` | RAG 入库 / 混合检索问答 / GraphRAG 构建与问答 |
| `/api/prompts/**` | 提示词模板、版本发布、激活回滚、渲染预览 |
| `/api/memory/**` | 短期会话、长期记忆、向量 / 图谱召回、记忆问答 |
| `/api/llm/**` `/api/llm/calls/**` | 数据源 CRUD、默认切换、连通性测试、调用历史 |
| `/api/skills/**` `/api/skillhub/**` | 技能安装启停、文件、市场搜索与下载 |
| `/api/mcp/**` | MCP 服务配置、探活、工具同步、语义召回、工具调用 |
| `/api/aiplatform/**` | 外部 AI 平台（千问 / 百炼）：平台 CRUD、连通性测试、目录搜索、MCP 一键接入 |
| `/api/agentfactory/**` | Agent 定义、LLM 造 Agent、任务提交与链路追踪 |
| `/api/conversations/**` | 会话目录 + 消息链路 + 多角色辩论 |
| `/api/histories/**` | 统一操作历史（MCP / 技能 / 记忆 / RAG / GraphRAG 召回） |
| `/mcp/platform` | 平台内置 MCP 端点（Streamable HTTP，只读，仅本机回环） |

完整契约见 `api/openapi.yaml`（含文件上传等手工补齐的 multipart 定义）。

### 平台 MCP 端点

`POST /mcp/platform` 是一个标准 MCP Server（JSON-RPC 2.0，无状态，不签发 `Mcp-Session-Id`），
供本机 Agent 与本机 MCP 管理器接入平台数据。服务启动后由 `AfterStart` 自注册为 MCP 服务并同步工具，
可在「MCP 工具」页直接看到。

| 工具 | 作用 |
| --- | --- |
| `list_tasks` / `get_task` / `get_task_trace` | 任务中心：列表、详情、全链路（事件 + 轮次 + 子任务） |
| `list_conversations` / `get_conversation` / `get_conversation_trace` | 会话：目录、详情、消息链路 |
| `list_operation_histories` | 统一操作历史（MCP / 技能 / 记忆 / RAG / GraphRAG 召回） |

```bash
curl --noproxy '*' -s http://127.0.0.1:8000/mcp/platform \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

> 非本机来源一律 403（平台数据敏感，不通过公网地址暴露）；端点只读，不提供任何写操作。
> 方案与实现说明见 `internal/server/MCP网关方案.md`。

## 项目结构

```text
go_agent/
├── api/                    # protobuf 定义与生成绑定（pb.go / grpc / http / openapi.yaml）
├── cmd/go_agent/           # 入口：装配 + worker 生命周期（BeforeStart/Drain）+ 平台 MCP 自注册
├── configs/                # 配置（${ENV:default} 展开）
├── internal/
│   ├── biz/                # 业务用例（conversation / agentfactory / rag / graphrag /
│   │                       #   memory / prompt / mcp / platformmcp / llm / skill / history /
│   │                       #   aiplatform / harness）
│   ├── data/               # 存储实现（MySQL / Redis / Milvus / MCP 客户端 / 技能 / AI 平台目录客户端）
│   ├── service/            # 传输层：proto 映射、错误映射、平台 MCP 服务注册
│   ├── server/             # HTTP / gRPC 装配 + 平台 MCP 端点（mcp.go）
│   └── pkg/                # 文档解析 loader、httpx 等
├── scripts/                # openapi 补丁脚本等
├── third_party/            # proto 依赖
└── Makefile

frontend/                   # React 管理控制台（Vite，/api 代理）
```

## 开发

```bash
make init      # 安装 buf / wire / protoc 等生成器
make api       # buf 生成 pb.go / grpc / http / openapi.yaml + 补 multipart 契约
make config    # 生成配置 proto
make all       # api + config + go generate
make check     # 校验生成路由与 openapi.yaml 一致（TestProtoRoutesAreRegistered）
make sdk       # 由 openapi.yaml 重新生成前端 SDK
make build     # 构建到 bin/
```

代码生成链路：`proto → buf → pb.go/grpc/http + openapi.yaml → 前端 SDK`，所有 REST 路由由注解生成，不手写路由表。

## 测试

```bash
go test ./...                 # 全部单元测试
go test ./internal/service/ -run 'TestProtoRoutesAreRegistered|TestPathParamRoutesLast' -v   # 路由契约校验
go test ./internal/biz/agentfactory/ -run 'TestDrain|TestConvIDInheritance' -v              # 停机排空 + conv_id 继承
go test ./internal/server/ -run 'TestInitialize|TestToolsListAndCall|TestLoopbackGuard' -v   # 平台 MCP 端点协议
go test ./internal/biz/platformmcp/ -v                                                        # 平台 MCP 工具定义与参数裁剪
go test ./internal/data/db/repo/ -run 'TestMapUpdateKeysMatchModelColumns' -v                 # map 更新列名静态校验
go test ./internal/biz/aiplatform/ -v                                                         # 千问/百炼：默认值、接入、回滚、SSE 降级、技能市场联动
go test ./internal/data/third_service/aiplatform/ -v                                          # 目录接口多形态 JSON 解码与错误退化
```

`biz` 层测试使用手写 `fakeRepo`（不引 mock 框架）：修改接口后必须同步 fake，否则编译失败——这是刻意的，用来强制更新所有实现。

几个「编译期发现不了、只能靠测试兜住」的回归点：

| 测试 | 防的是什么 |
| --- | --- |
| `repo/columns_test.go` | GORM `Updates(map[string]any{...})` 的 key 直接当列名，写错（如 `flow` 应为 `flow_json`）只在运行时炸 `Error 1054` |
| `server/mcp_test.go` | 平台 MCP 的协议层行为：initialize 协商、tools/list、工具级错误走 `isError`、非回环 403 |
| `agentfactory_test.go` 的 Drain 用例 | 优雅停机三种路径：正常排空 / 排空期间拒绝新任务 / 超时后任务保持 running |
| `history_test.go` | `WithTask` / `WithConv` 叠加顺序，避免再出现 `conv_id` 被覆盖为空 |

## Roadmap

- [ ] 评测 harness 接线：黑盒执行 + 规则断言 + LLM Judge + 报告（`internal/biz/harness` 已就绪）
- [ ] 平台 MCP 二期：对外工具聚合网关（`/mcp` + `api_key` 鉴权），复用 `biz/mcp` 调用管线
- [ ] 长耗时操作全面异步化：入库 / 图谱抽取改「提交任务 + 按 task_id 查进度」，HTTP 超时回归秒级
- [ ] CI / Release 流程与测试徽章
- [ ] 在线演示环境
- [ ] 英文文档

## 许可证

[MIT](LICENSE)
