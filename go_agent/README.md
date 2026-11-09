# Meridians · 经纬

**Go 实现的一体化智能体平台**：把「知识怎么进来、智能体怎么造、提示词怎么变好」这三件通常散落在四五个系统里的事，做成一条闭环。

文档经 RAG 混合检索与 GraphRAG 多跳推理两条线并行入库；Agent 以数据而非代码定义，由 13 种执行图驱动（含 auto 自动编排），可用 LLM 元编程批量生成；提示词版本化后可被 LLM 打分、优化，并在评测 Harness 中用真实用例拿回得分、回写进版本历史。配套完整 React 管理控制台。

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

### 文档更新（版本替换）

Milvus 分块主键是 autoID，重新入库只会新增行。旧实现直接 `Insert`，同一个 `doc_id`
会同时留下多版分块，检索时新旧混着返回。现在的顺序是**先写新版本、再删旧版本**：

```
读取旧版本主键 (IDsOfDoc) → 写新版本分块 → 按旧主键删除 (DeleteByIDs)
```

- 新版本写失败 → 旧版本原样保留，只是文档状态落 `failed`，不会出现「文档在、向量没了」
- 旧版本删失败 → 只是残留重复，下次内容变化时 `IDsOfDoc` 会把它们一起列出来清掉（自愈）
- `rag` / `graph` 两种模式互切时，另一侧的旧产物一并清理（`DeleteDoc` / `DeleteDocGraph`）
- 入库结果返回 `purged`（本次顶掉的旧行数），前端可据此提示「已替换旧版本」

### GraphRAG 知识图谱

- LLM 抽取实体 / 关系，块内合并去重 → 跨文档合并（Milvus 图集合，并发抽取信号量 4）
- 查询：实体混合检索得种子 → 沿关系边多跳扩展（默认 2 跳 / 200 边）→ 子图 + 回查原文证据 → 组装上下文
- 与主 RAG 线路互不影响，可并行摄取
- **旧引用清理**：同一文档重新入库时，先摘掉它在图上的全部旧引用再写新节点。
  只被该文档引用的节点/边直接删除；与其他文档共享的节点保留，但 `refs` 改写成
  「剩余引用」——旧实现只删孤儿节点，共享实体上的旧引用会留在图里，下次入库又被
  合并回去，等于永远清不掉（旧 `chunk_idx` 还会指向新版本的另一段内容）。
  摘除放在所有 embedding 之后，前面的抽取/向量化失败都不会动到已有图谱。

### Agent 工厂

- Agent 定义即数据：`agent_definitions` 表 + 向量化语义召回
- **13 种执行图**：ReAct / Plan-Execute / Reflexion / Orchestrator / 多 Agent 协作 / Debate 等，另有 `auto` 由 LLM 执行前判定架构与成员并自动建子 Agent；DAG Flow 工作流编排
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
| `agentfactory` | Agent 低代码工厂：定义 / 任务 / 13 种执行图（含自动编排）/ worker / cron | `/api/agentfactory/**` | MySQL · Milvus `_agents` · Redis |
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
# 默认监听 127.0.0.1:8000 / 127.0.0.1:9000，认证关闭也能直接启动
kratos run
# 或等价地：go run ./cmd/go_agent -conf ./configs
# HTTP :8000 · gRPC :9000 · 健康检查 http://127.0.0.1:8000/api/health
```

需要从**别的机器 / 手机**访问（监听 0.0.0.0）时，二选一：

```bash
# A. 配好认证（推荐）
AUTH_ENABLED=true AUTH_JWT_SECRET="$(openssl rand -hex 32)" HTTP_ADDR=0.0.0.0:8000 kratos run
# B. 仍是本机/内网演示，显式声明开发模式（启动打 WARN）
AUTH_DEV_INSECURE=true HTTP_ADDR=0.0.0.0:8000 GRPC_ADDR=0.0.0.0:9000 kratos run
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
| `HTTP_ADDR` / `GRPC_ADDR` | `127.0.0.1:8000` / `127.0.0.1:9000` | 监听地址。默认只绑回环，本机开发无需认证即可启动；容器镜像内由 Dockerfile 覆盖为 `0.0.0.0` |
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
| JWT 校验 | `AUTH_JWT_ALLOW_NO_EXP` / `AUTH_JWT_ISSUER` / `AUTH_JWT_AUDIENCE` | 空（= 强制 exp） | 见下方「JWT 侧的校验规则」 |
| 开发态逃生阀 | `AUTH_DEV_INSECURE` | `false` | 认证关闭时允许对外监听（仅本机开发/内网演示，启动 WARN） |
| 限流 | `RATELIMIT_ENABLED` | `false` | 按「租户 × 路由」令牌桶，读写分开配额；超限返回 `429` + `Retry-After` |
| 预算 | `BUDGET_MAX_TOKENS` / `BUDGET_MAX_CALLS` / `BUDGET_MAX_PARALLEL` | `400000` / `80` / `4` | 单任务树（含派生子任务）的 token 与调用次数上限，超限走「预算耗尽」收敛分支；并行波次并发上限防止宽 DAG 同时烧钱 |
| 计价 | `PRICE_INPUT_PER_M` / `PRICE_OUTPUT_PER_M` | `1` / `2` | 元 / 百万 token，用于 `/metrics` 的成本估算 |

**认证关闭 + 对外监听 = 拒绝启动**（`server.CheckInsecureExposure`，在 `main` 里先于依赖装配执行）：

| 组合 | 结果 |
| --- | --- |
| `AUTH_ENABLED=true` | 正常启动 |
| `AUTH_ENABLED=false` + 只监听 `127.0.0.1` / `localhost` / `[::1]`（**仓库默认**） | 正常启动（本机开发） |
| `AUTH_ENABLED=false` + 监听 `0.0.0.0` / `:8000` / 具体网卡 / 主机名 + `AUTH_DEV_INSECURE=true` | 启动，日志 WARN |
| `AUTH_ENABLED=false` + 监听 `0.0.0.0` / `:8000` / 具体网卡 / 主机名 | **拒绝启动**，错误信息给出三条出路 |

空 host（`:8000`）等价于 `0.0.0.0`；绑定的主机名在启动期无法确认解析到哪块网卡，同样按对外处理——
误判的代价是加一个环境变量，漏判的代价是数据面裸奔。

为什么是硬失败而不是告警：告警在容器日志里滚两屏就没人看了，而这一步一旦放过，整个数据面
（会话、记忆、Agent 定义、token 额度）就对任何能连上端口的人开放。默认配置已收回到回环，
所以闸门只在「有人主动把监听改成对外」时才会响——那一下正是应该停下来想清楚的地方。

JWT 侧的校验规则（v1.8 加严）：

- **强制 `exp`**：没有有效期的签名令牌等于永久后门，默认拒绝；历史签发体系可临时用
  `AUTH_JWT_ALLOW_NO_EXP=true` 兼容（会打印 WARN）。
- **`iss` / `aud`**：配置 `AUTH_JWT_ISSUER` / `AUTH_JWT_AUDIENCE` 后逐项比对（`aud` 支持字符串与数组）。
- **`iat`**：晚于当前时间（超出 60s 时钟偏移）视为非法签发。
- **`nbf`**：早于生效时间拒绝（原有）。
- **算法**：只接受 `HS256`，显式拒绝 `none` / `RS256`（防算法混淆）。

对外暴露端口前至少做这一步：

```bash
export AUTH_ENABLED=true
export AUTH_JWT_SECRET="$(openssl rand -hex 32)"
export HTTP_ADDR=0.0.0.0:8000 GRPC_ADDR=0.0.0.0:9000   # 只绑回环就保持默认
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
| 长期记忆 `memories` / `recall_logs` | `user_id` | 召回命名空间 = 当前用户 | 记忆 API 在认证态强制用令牌身份，忽略请求体 `user_id`（防伪造）；单条读取 / 删除先查归属再动手（`GetLong` / `DeleteLong`） |
| 短期会话 `mem:short:*` | Redis 键内嵌 `tenant:user` | 键 = `mem:short:{tenant}:{user}:{session}` | 伪造 `session_id` 只会落到自己的命名空间（读到空）；MySQL 回源按 `conversation.user_id` 校验 |
| 记忆图谱（与 RAG 共用集合） | 引用前缀 `mem::{user}::{memoryID}#0` | 只与「同用户」的既有实体合并 | 召回按引用归属裁剪（实体/关系/扩展邻居），并裁掉其中的他人引用 |
| 操作历史 `op_histories` / 调用记录 `llm_calls` | `tenant_id` + `user_id` | `history.Record` / `llm.Manager` 从 ctx 身份补齐 | 列表与统计按身份过滤；`prompt_text` / `reply_text` 非管理员只返回 240 字预览 |

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
| `GET /api/admin/audits` | 审计事件查询（仅管理员）：`?action=&result=&resource=&user=&since=&until=&limit=` |
| `GET /api/mcp/tool-approvals` | 待审批的高危工具调用（仅管理员） |
| `POST /api/mcp/tool-approvals/{id}/approve` \| `/deny` | 批准 / 驳回一次高危调用（仅管理员，且不能批自己发起的） |

### 管理面授权（登录 ≠ 管理员）

网关的认证只回答「你是谁」，**授权由 `internal/pkg/authz` 的角色矩阵回答**：
平台级变更（MCP 配置、技能安装、Agent 删除）是 `platform:config`，只有平台管理员有。

| 角色 | 主要权限 |
| --- | --- |
| `platform_admin`（别名 `admin` / `superadmin`） | 全部：平台配置、租户管理、Agent 开发、任务运维与审批、审计读取 |
| `tenant_admin` | 本租户成员与配额、Agent 开发、任务运维与审批、审计读取（不含平台配置） |
| `developer` | Agent / Prompt / Skill 的开发与调试、任务运维 |
| `operator` | 任务执行、取消、重试、审批（不含平台配置） |
| `auditor` | 只读审计与调用记录（跨用户，但限定在本租户） |
| `user`（默认） | 使用被授权的 Agent，只能看自己的数据 |

判定规则（`authz.Allowed`）：

| 情况 | 判定 |
| --- | --- |
| 令牌 `role`/`roles` 里含具备该权限的角色 | 放行 |
| JWT / 静态令牌带 `role=admin`（老写法） | 等价 `platform_admin` |
| 认证关闭（开发态）+ 请求来自回环地址 | 放行（仅为了本机开发可用） |
| 认证关闭 + `AUTH_DEV_TRUST_ALL=true` | 放行（演示环境逃生阀，启动打 WARN） |
| 其余（含「已登录的普通用户」与未知角色） | `403`，并落一条 `denied` 审计事件 |

被闸门覆盖的动作：`mcp.server.*`（增删改 / 连接 / 同步 / 客户端参数）、`mcp.tool.*`（启停 / 删除）、
`mcp.proxy.preview`、`skill.install*` / `skill.uninstall` / `skill.sync` / `skill.hub.*`、
`agentfactory.def.delete` / `agentfactory.def.toggle` / `agentfactory.vectors.sync`、`mcp.tool.approval.*`、`audit.list`。

技能的**启停**（`skill.enable` / `skill.disable`）刻意不设闸门：它不写磁盘、不改能力集合，属于个人偏好。

管理员的两种给法：

```bash
# 1) 静态令牌：user:tenant:role
#    security.auth.staticTokens: { "ops-token": "ops:acme:admin", "audit-token": "auditor1:acme:auditor" }
curl -H "Authorization: Bearer ops-token" -X POST .../api/mcp/servers

# 2) JWT：payload 里加 role（单个）或 roles（数组）
#    {"sub":"ops","tenant_id":"acme","role":"admin","exp":...}
#    {"sub":"auditor1","tenant_id":"acme","roles":["auditor"],"exp":...}
```

> 单机演示时浏览器从另一台机器访问，回环判定不成立，管理按钮会全 403。
> 要么签发一个 admin 令牌，要么显式打开 `AUTH_DEV_TRUST_ALL=true`——
> 后者把管理面重新变成匿名可达，只在确定不对外暴露的环境里用。

### 租户隔离（同 userID 不同租户也互不可见）

`identity` 里的 `TenantID` 不再只用于限流与日志，已下推到数据面：

| 数据 | 隔离键 | 行为 |
| --- | --- | --- |
| 知识库 `rag_documents` + Milvus 分块 | `tenant_id` + `user_id` | 列表、检索白名单、入库归属校验都按 `(租户, 用户)`；跨租户同名 `doc_id` 直接拒绝覆盖 |
| 任务 `agent_tasks` / `agent_task_history` | `tenant_id` + `user_id` | 提交时固化，列表按租户过滤（跨租户同 userID 也列不到） |
| 操作历史 `op_histories` / 调用记录 `llm_calls` | `tenant_id` + `user_id` | 列表与统计按身份过滤；非管理员只返回 240 字预览 |

加列时统一用 `default 'public'`，MySQL 会用默认值回填历史行——历史数据归入默认租户，
不会变成「谁都能看」。**跨租户共享**（MCP 服务、技能市场、提示词模板）仍按平台级资源处理，
由 `platform:config` 权限控制写操作。

### 聊天附件（`/api/files/`）的访问控制

附件是 `<img src>` / `<video src>` 直接加载的，带不了 `Authorization` 头；这条路由又是
原始 `net/http` 注册的，根本不经过认证中间件——所以以前「知道地址就能读」。

现在把地址本身变成凭据（`internal/pkg/mediaauth`）：服务端读消息时给附件地址补
`?sig=hex(HMAC-SHA256(密钥, 路径))`，静态路由校验签名。

| 场景 | 行为 |
| --- | --- |
| 认证关闭（开发态） | 不签名、不校验，打开就能看图 |
| 认证开启 + 带有效签名 | 放行（前端无需改动，`a.url` 原样用） |
| 认证开启 + 带 `Authorization` 头或 `?token=` 的有效令牌 | 放行（给 API / 脚本用） |
| 认证开启 + 两者都没有 / 签名对不上路径 | `401`，且 `Cache-Control: no-store` |

几个实现取舍：

- **存储里始终是裸地址**，签名在读取时现算：换密钥、或用进程内随机密钥（纯静态令牌部署）重启，
  历史消息不会永久 403，也不需要数据迁移。
- 签名只覆盖路径，不覆盖用户——它是能力型 URL，挡的是「改路径读别人的文件 / 猜文件名批量扫」，
  不是「按会话授权」。真正的按用户授权需要给附件建索引（`bucket/name` → 会话/上传人）。
- `mediaDiskRel` 会先剥掉查询串再映射磁盘路径（图片回传给模型时要用），路径穿越检查保持不变。

### 出站访问策略（防 SSRF）

服务端会主动访问的地址（MCP `endpoint`、Swagger `spec_url` / `base_url`、技能包 URL、技能市场 `base_url`）
全部来自管理面配置。只校验 `http(s)://` 前缀等于把后端变成内网跳板，因此统一走
`internal/pkg/egress`，三道闸叠加：

1. **保存时静态校验**：scheme、`userinfo`、字面量 IP 段、主机白名单、端口；
2. **Dial 阶段**：`net.Dialer.Control` 校验**解析后的 IP**——挡住「域名指向内网」与 DNS rebinding；
3. **重定向**：每一跳目标重新校验——挡住 `302 Location: http://169.254.169.254/`。

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `EGRESS_ALLOWED_HOSTS` | 空 | 主机白名单（`mcp.example.com` / `.corp.internal` / `*.corp.internal`）；**配了就变白名单模式** |
| `EGRESS_DENIED_HOSTS` | 空 | 额外拉黑，优先级高于白名单 |
| `EGRESS_ALLOW_PRIVATE` | `false` | 放行内网 / 回环（内网部署、本机 MCP 服务才开） |
| `EGRESS_ALLOWED_PORTS` | 空 | 端口白名单，空 = 不限 |
| `EGRESS_MAX_REDIRECTS` | `5` | 最多跟几跳重定向，`0` = 不跟随 |

**始终拒绝**（即便 `EGRESS_ALLOW_PRIVATE=true`）：`169.254.0.0/16` 链路本地（云元数据）、组播 / 保留段、
`localhost`、`metadata.google.internal` 等元数据域名。旧的 `MCP_EGRESS_*` 变量名仍被识别。

### MCP 凭证不回显

MCP 服务的 `headers` / `env` / `proxy_config` 里是能直接用的凭证（`Authorization`、长期 token、
数据库密码）。读接口不再原样返回（`internal/pkg/redact`）：

| 接口 | 行为 |
| --- | --- |
| `GET /api/mcp/servers`、`GET /api/mcp/servers/{id}` | 值一律是 `******`，键名保留；另有 `has_credentials` 布尔 |
| `POST` / `PUT` | 收到 `******` 表示「这次不改」，用旧值回填；空 map 同样表示不改 |
| `proxy_config` | 按 JSON 结构递归打码（`auth.token`、`client_secret`、嵌套 `headers` 都覆盖），结构保留供前端编辑 |

「写回掩码 = 保持原值」让前端不用改：表单原样提交不会把凭证写成 `******`。
AI 平台接入面的网关头走同一套（`redact.MaskMap` / `redact.MergeMasked`）。

> 边界：这是「不再复制扩散」，不是静态加密。落库仍是明文（`mcp_servers.headers`），
> 真正的 KMS / Envelope Encryption 属于存储层改造，尚未做。

### 高危工具调用审批

MCP 工具是远端定义的，`delete_repository` 一次调用就能造成不可逆损失。平台按三个信号给工具分级
（`internal/biz/mcp/risk.go`）：显式标签（`tags: ["readonly"｜"write"｜"destructive"]`）→ proxy 路由方法
（`GET` 读 / `PUT`·`PATCH`·`POST` 写 / `DELETE` 高危）→ 名称与描述关键词（`delete`/`drop`/`exec`/`truncate`…）。
**未知工具按 `write` 处理**，不猜 `read`。

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `MCP_TOOL_APPROVAL_LEVEL` | `destructive` | 达到该等级即需审批：`none` / `write` / `destructive` / `all` |
| `MCP_TOOL_APPROVAL_TTL` | `300` | 审批单有效期（秒） |

两段式调用（管理面调试接口 `POST /api/mcp/tools/{id}/call`，也是脚本直调的唯一入口）：

```bash
# 1) 直接调用 → 428，响应里带审批单号 ap...
curl -s -X POST http://localhost:8000/api/mcp/tools/12/call   -H 'Content-Type: application/json'   -d '{"arguments":{"repo":"billing","force":true}}'
# → 428 APPROVAL_REQUIRED：已生成审批单 ap3f…（5m0s 内有效）

# 2) 管理员批准（四眼原则：不能批自己发起的）
curl -s -X POST http://localhost:8000/api/mcp/tool-approvals/ap3f.../approve   -H 'Authorization: Bearer ops-token' -d '{"comment":"确认删测试库"}'

# 3) 带令牌重放，参数必须完全一致
curl -s -X POST http://localhost:8000/api/mcp/tools/12/call   -H 'Content-Type: application/json' -H 'X-MCP-Approval-Token: ap3f...'   -d '{"arguments":{"repo":"billing","force":true}}'
```

令牌绑定 `(申请人, 租户, 工具, 参数指纹)`，一次性、默认 5 分钟过期；参数改一个字符就得重新申请。
审批遵循四眼原则（不能批自己发起的）；**只在认证关闭的开发态**允许自批，且在审计里标注
`self_approved=true`——单机开发时系统里只有一个身份，坚持四眼原则只会让流程不可用。

Agent 运行期同一套口径：`approvalReason` 对 `mcp:` 能力先做静态风险判定——高危工具即便没写进
`require_approval` 也会挂起等审批（LLM 生成定义时漏写是常态，漏写的代价是一条 `DELETE` 直接打到生产库）。
任务挂起后走既有的 O3 流程：`paused` → `POST /api/agentfactory/tasks/{id}/approve` → 从 checkpoint 续跑。

审批凭据的粒度（`internal/biz/agentfactory/approval.go`）：

| 维度 | 取值 | 为什么 |
| --- | --- | --- |
| `ability` | `mcp:db__delete` / `agent:xxx` … | 批准的是哪个能力 |
| `canonical_args_hash` | 参数的 JSON 规范化 sha256 | **只批准这一次参数**：同一工具换 `id=999999` 必须重新问人 |
| `policy_version` | `approval/v2` | 改了「什么需要审批」的规则，旧凭据自动失效 |

`task_id` / 用户 / 租户由凭据的载体（`task_checkpoints` 按任务存、任务自带归属）隐含，
默认 24h 过期；凭据本身带 `requester` / `approver` / `decision` / `reason` / `created_at` / `expires_at`，
写进 checkpoint 与 `approval` 事件，审计里能还原「谁批了谁的哪一次调用」。

审批人资格：

- 请求人自批 **高危** 操作 → 拒绝（`403 APPROVAL_SELF_APPROVE`，四眼原则）；非高危允许，
  否则单机 / 小团队场景没人能批；
- 跨用户审批 → 需要管理员（`AUTH_*` 里的 `role=admin`，或认证关闭 + 本机）；
- 开发态（认证关闭 + 回环地址）自批放行，凭据标 `self_approved=true`，审计单独可见。

风险评估是「增强判断」而不是「唯一控制」：`risk_assess` 服务不可用时**不再当成没风险**
（旧实现返回 `nil`，调用方按「未启用评估」处理，等于评估器一挂就全放行）——配了
`risk_pause_level=high` / `require_approval` 的 Agent 转人工，没配的直接阻止本次调用并如实告知模型。

复合执行图里的挂起：`plan_execute` 就地挂起（不重规划、不推进步骤，批准后从该步续跑）；
`delegate` / 编排成员 / DAG 节点的子任务挂起时，父任务**显式失败**并带上子任务 id，
下游节点不激活。父任务之所以不跟着挂起，是因为父任务续跑会重建子任务（DAG 无节点级断点），
拿不到子任务那张凭据，会陷入「批准父任务 → 新子任务又要审批」的循环。
审批子任务后它会自行续跑完成，需要的话再提交一次父任务。

### 编排图（DAG）的失败与跳过语义

节点只有 `succeeded` 才默认放行下游。失败节点的出边默认**全部置死**，下游整段跳过——
旧实现把失败写成「节点完成 + `[执行失败] …` 文本」并激活所有出边，下游 Agent 会把上一跳的
报错当成正常输入继续干活（甚至产生副作用），失败因此被静默吞掉。

| 出边 `when` | 源节点成功 | 源节点失败 | 源节点被跳过 |
| --- | --- | --- | --- |
| 空 / `success` | 放行 | 不放行 | 不放行 |
| `failure`（on_error 分支） | 不放行 | 放行 | 不放行 |
| `always` | 放行 | 放行 | 不放行 |
| `true` / `false` | 条件节点按分支命中（只允许条件节点使用） | 不放行 | 不放行 |

需要「失败也要继续」就在边上显式写 `when: "failure"`，而不是依赖默认行为。
跳过的分支不级联激活：条件未命中的整段下游（含再下游）都不会执行。

### 任务状态迁移（CAS）

任务状态原来是「先读、再判断、最后写库」，读写之间没有约束，于是有这样的竞态：

```
请求 A：读到 pending
Worker：ClaimTask（pending → running），开始执行并产生副作用
请求 A：按「pending 取消」分支走终态写库 → 库里的 running 被改成 canceled
```

结果就是任务显示「已取消」，副作用却已经发生，事后无从追溯。现在所有**跨状态**
写入都带源状态下推到 SQL（`UPDATE … WHERE id = ? AND status IN (…)`，
见 `internal/data/db/repo/agentfactory.go` 的 `TransitionTask`），影响行数为 0
就说明状态已变，调用方重新读一次再决策，而不是覆盖别人的结论：

| 迁移 | 触发方 | 输掉竞争时的行为 |
| --- | --- | --- |
| `pending → running` | Worker 认领（`ClaimTask`） | 直接放弃，不执行 |
| `pending → canceled` | 取消未开始的任务 | 重读重试（最多 3 次） |
| `running → paused` | 命中审批闸门 | 补一条事件，不再改回 paused |
| `running → succeeded/failed/canceled` | Worker 收尾 | 只补事件，不覆盖已有终态 |
| `paused → pending` | 人工批准续跑 | 返回 409 `TASK_STATE_CONFLICT`，提示刷新 |
| `paused → failed` | 人工驳回 / 审批超时 | 跳过，不再写终态 |
| `paused → canceled` | 挂起期取消 | 重读重试 |

允许的边集中在 `internal/biz/agentfactory/task_state.go`（`CanTransit`），
终态没有出边；重试走 `ReviveTask`（历史表 → 任务池，重置 pending）。

### 断点续跑：已完成的步骤不会被重跑

Checkpoint 原来只在「一个波次跑完」时才落一次。并行波次里 A 已完成、B 还在跑时进程挂掉，
重启后 A 会被重跑一遍——A 如果发过邮件、下过单、改过库，副作用就发生了两次。
现在把「哪一步已经完成」变成一等状态，**步骤一结束就落盘**：

| 执行模型 | 状态载体 | 续跑行为 |
| --- | --- | --- |
| plan_execute | `RunState.StepState`（下标 → 状态 / 产出 / 执行次数） | `done` 跳过（产出直接进 `Done`）、`failed` 重跑、`paused` 重放该步等审批 |
| 编排图（flow DAG） | `RunState.NodeState` + `EdgeState`（节点 → 状态/产出/分支；边下标 → active/dead） | `done`/`skipped` 复用产出与边判定、`failed` 重试并复位该节点出边 |
| 串行 / 并行编排 | `RunState.MemberOut`（agent_key → 答案） | 已有产出的成员直接复用，只补跑缺失成员 |
| 工具循环 | `RunState.LoopTurn` / `Seen` / `Approvals` | 从中断轮次继续，已批准的调用不再问人 |

几条刻意收紧的语义：

- **`Done` / `StepIdx` 只是 `StepState` 的连续前缀视图**：并行波次里步骤乱序完成，
  只有前面的步骤都完成了才把后面的产出并入 `Done`（`promoteDoneSteps`，幂等，重复续跑不会写出重复行）。
  扫到「连续前缀断了」就停，保证「步骤 N 的上下文含 1..N-1 的全部产出」。
- **节点状态与边判定同一次快照写入**：只落节点不落边，重启后节点等不到「入边已决断」，
  整张图会卡在不能推进；反过来只落边不落节点，下游会拿到空产出。
- **失败节点重试时复位它自己的出边**：否则重试成功了，下游仍因为「边已置死」不启动。
- **中断中的步骤（`running`）按未完成重跑**：外部调用是否已经发生无法从本地判断，
  这一步是 at-least-once，窗口只在「步骤执行结束 → 落盘」之间。
  真正需要 exactly-once 的副作用应该在工具侧做幂等键。
- **老 checkpoint 兼容**：历史版本把步骤产出记在 `MemberOut["step:N"]`，解码时自动迁进
  `StepState`，升级不用清库。

### 计划校验：依赖写错不能让任务跑错

模型给出的 `depends_on` 可能越界、自依赖、指向前面的步骤之外，或重复声明。
现在的处理是**校验 + 修正 + 留痕**，而不是猜：

| 情况 | 处理 |
| --- | --- |
| 依赖 `< 1` / 指向自己或更晚的步骤 / 重复声明 | 摘掉这一条依赖，其余保留；记一条 `plan` 事件（`invalid` 保留原始依赖，便于排障） |
| 全部依赖被摘掉 | 退化为串行（串行永远正确，只是慢） |
| 重规划的新计划 | `depends_on` 先按「新计划内部 1-based」重基成绝对下标，再**对拼接后的完整计划**校验（只校验新段会把段内依赖全判成前向依赖） |
| 波次调度时发现依赖仍未满足 | 直接报错 `ErrInvalidDef`，绝不挑一个依赖未满足的步骤硬跑——那正是重复副作用的来源 |

### 条件判断失败 ≠ 条件不成立

条件节点决定「哪条分支被激活」。旧实现里模型调用失败或输出无法解析时一律当成
`false`，等于猜了一个答案还继续跑，在审批 / 风控类条件上就是**放行**。现在：

- 规则模式：`contains` 缺比较值、`gt/lt` 取不到数字、未知运算符 → 返回明确错误；
- 模型模式：调用失败、没有可用模型、缺判断问题、输出里同时出现「是」和「否」 → 返回明确错误；
- 错误向上变成**节点失败**：出边按失败语义决断（只有 `when: failure/always` 放行），
  兜底答案里带 `[执行失败] 条件判断失败：<原因>`，不会静默走进任何一条分支。

### 审计事件（`audit_events`）

授权判定、管理面变更、高危工具调用都会落一条审计事件（`internal/biz/audit` + `audit_events` 表），
异步批量写入——审计不能拖慢也不能搞挂业务请求，缓冲满了宁可丢并计数告警。

| 字段 | 说明 |
| --- | --- |
| `ts` / `action` / `result` | 时间、动作（`mcp.server.create` / `skill.install` / `mcp.tool.call`）、结果（`allowed` / `denied` / `error`） |
| `user_id` / `tenant_id` / `auth_type` / `is_admin` / `is_local` | 身份快照（不靠 join，用户改名后仍还原当时事实） |
| `resource` / `resource_id` / `risk` | 作用对象与风险等级 |
| `ip` / `trace_id` / `latency_ms` / `detail` | 来源 IP（优先 `X-Forwarded-For`）、链路号、耗时、JSON 明细（截断 2000 字符） |

保留期 `AUDIT_RETENTION_DAYS`（默认 90 天），后台每 6h 清理一次。

```bash
# 上周谁被拒过
curl -s -H 'Authorization: Bearer ops-token'   'http://localhost:8000/api/admin/audits?result=denied&since=7d&limit=50'
# 某个高危工具的全部调用
curl -s -H 'Authorization: Bearer ops-token'   'http://localhost:8000/api/admin/audits?action=mcp.tool.call&resource=tool&limit=200'
```

> 边界：审批单存在进程内存里（单实例够用；多副本需要换 Redis 实现，接口已抽成 `ApprovalStore`）。
> 审计是「事后追溯」，不替代事前拦截——真正的拦截在前面的三道闸门。

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
