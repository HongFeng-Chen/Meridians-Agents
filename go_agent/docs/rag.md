# RAG / GraphRAG 业务线（迁移自 rag-system）

原 `rag-system`（独立 Go 服务 + 自己的 SQLite-free 内存配置）已并入本 Kratos 工程，
作为 DDD 分层下的一条业务线，并新增 **MySQL 文档元数据** 与 **Redis 问答缓存/入库锁**。

## 分层映射

| 原 rag-system                | 现 go_agent                                    | 说明 |
|------------------------------|------------------------------------------------|------|
| `internal/config`            | `internal/conf`（conf.proto + configs/config.yaml） | 改为 protobuf 配置，支持 `${ENV:default}` |
| `internal/store`（Milvus）    | `internal/data/milvusdb/repo/chunk.go`           | 实现 `biz/rag.ChunkStore` |
| `internal/graphrag/store.go` | `internal/data/milvusdb/repo/graph.go`           | 实现 `biz/rag/graphrag.Store` |
| `internal/graphrag/service.go` | `internal/biz/rag/graphrag/service.go`        | 只依赖接口，不碰 SDK |
| `internal/chunker`           | `internal/biz/rag/chunker.go`                   | 原样迁移（含多字节边界修复） |
| `internal/loader`            | `internal/pkg/loader`                           | 纯解析工具，无业务依赖 |
| `internal/embedder` / `llm`  | `internal/biz/embedder`、`internal/biz/llm`     | 后者补了 `ChatTemp`（图谱抽取用 0 温度） |
| `internal/rag/pipeline.go`   | `internal/biz/rag/rag.go`（Usecase）            | 新增 MySQL 元数据、Redis 缓存、去重锁 |
| —（提示词常量）               | `internal/biz/prompt` + `/api/prompts`          | 提示词改由管理端版本化维护，见 [prompt.md](./prompt.md) |
| `cmd/api/main.go`（net/http）| `internal/service/rag_service.go` + `server/http.go` | 走 Kratos HTTP 路由，路径不变 |
| —（新增）                     | `internal/data/db`（MySQL：models + repo/document.go）、`internal/data/redis`（Redis：cache_answer.go） | 本次新增 |

依赖方向严格遵守 AGENTS.md：`service → biz ← data`，`biz` 不导入 Milvus/gorm/redis。

## 三个存储的分工

```
导入：文本 ──分块──► Embedding ──► Milvus（向量 + BM25 稀疏）
                                  └─► MySQL rag_documents（doc_id/来源/块数/状态/校验和）
                                  └─► Redis DEL rag:ans:*（旧答案失效）

问答：问题 ──► Redis 命中？──是──► 直接返回（cached=true）
                 │否
                 └─► Milvus 混合检索（Dense+BM25, RRF） ──► LLM 生成 ──► Redis 缓存 TTL
```

**MySQL**（表 `rag_documents`，gorm AutoMigrate 自动建表）
- `doc_id` 唯一索引：文档管理接口的权威数据源（不再靠扫描 Milvus 统计）
- `checksum`：同内容重复导入直接跳过向量化（`deduped=true`）
- `status`：ready / failed，便于前端展示失败原因定位

**Redis**
- `rag:ans:<sha1>`：问答缓存，键含问题 + 过滤条件；文档增删时用 SCAN 批量失效
- `rag:lock:ingest:<doc_id>`：SET NX 分布式锁，阻止同一文档并发入库；Redis 不可用时降级放行

## 接口（与旧版一致，默认监听 :8000）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/health` | 健康检查 + 运行参数 |
| POST | `/api/ingest` | `{doc_id,source,text,mode}` 导入文本（mode=rag/graph） |
| POST | `/api/query` | `{question,filter}` 混合检索问答 |
| POST | `/api/graph/ingest` | 图谱构建 |
| POST | `/api/graph/query` | 图谱问答 |
| GET | `/api/admin/stats` | 分块/文档/实体/关系统计 |
| GET | `/api/admin/docs` | 文档列表（来源 MySQL） |
| DELETE | `/api/admin/docs?doc_id=x` | 删除文档（向量+图谱+元数据+缓存） |
| GET | `/api/formats` | 支持的文件格式 |
| POST | `/api/extract` | 上传文件仅解析，不入库 |
| POST | `/api/upload?mode=rag\|graph` | 上传并入库 |
| * | `/api/prompts/**` | 提示词模板与版本管理，见 [prompt.md](./prompt.md) |

## 运行

```bash
# 1) 依赖：MySQL + Redis
docker compose -f deploy/compose.yaml up -d

# 2) Milvus（复用 rag-system 的 compose，需先启动）
cd ../rag-system && docker compose up -d

# 3) 启动服务（默认读 configs/config.yaml）
cd ../go_agent && go run ./cmd/go_agent -conf ./configs

# 4) 冒烟
curl -s localhost:8000/api/health | jq
curl -s -XPOST localhost:8000/api/ingest -d '{"doc_id":"d1","source":"a.txt","text":"Milvus 支持 BM25 与稠密向量混合检索。"}'
curl -s -XPOST localhost:8000/api/query  -d '{"question":"Milvus 支持什么检索？"}'
```

## 环境变量（均有默认值）

| 变量 | 默认 | 说明 |
|---|---|---|
| `DATABASE_SOURCE` | `root:root@tcp(127.0.0.1:3306)/go_agent?...` | MySQL DSN |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis 地址 |
| `MILVUS_URI` / `MILVUS_COLLECTION` | `http://127.0.0.1:19530` / `rag_docs` | 向量库 |
| `EMBED_BASE_URL` / `EMBED_MODEL` / `EMBED_API_KEY` | 本地 Ollama `bge-m3` / 1024 维 | 嵌入模型 |
| `LLM_BASE_URL` / `LLM_MODEL` / `LLM_API_KEY` | DeepSeek `deepseek-flash` | 生成模型 |

配置文件里的 `${NAME:default}` 由 `internal/conf/loader.go` 在加载前展开
（Kratos 自带的 file source 不做这一步）。

## 后续变更（2026-11，迁移之后）

这篇是迁移记录，下面这些是后来改的，读代码时以本节为准：

- **文档更新是「版本替换」**：同一个 `doc_id` 重复 `Ingest` 时，先记下旧分块主键
  （`ChunkStore.IDsOfDoc`）→ 写新版本 → 按旧主键删（`DeleteByIDs`）。写新失败则旧版本原样保留；
  删旧失败只留重复，下次入库会自愈。返回里的 `purged` 是被顶掉的行数。
- **图谱摘引用而不是删孤儿节点**：`DetachDocRefs(docID+"#")` 摘掉本文档的 `doc#idx` 引用，
  引用清空才删行；还有别的文档/记忆引用就改写 `refs`/`freq`。先写改写行再删旧行，避免中途失败留下空窗。
- **`mode` 切换自动清对面产物**：`rag → graph` 删旧分块，`graph → rag` 调 `DeleteDocGraph`，
  不需要先手工 `DeleteDoc`。
- **租户隔离**：`rag_documents` 加 `tenant_id`（默认 `public` 回填历史行），
  `DocumentRepo.List/ListDocIDs` 是 `(tenantID, userID)` 四参；`Ingest` 会拒绝覆盖别的租户的 `doc_id`。
  检索时把租户 + 用户白名单下推到 Milvus 过滤表达式。
- **删除顺序**：先删分块 → `DetachDocRefs` 摘图谱引用 → 删 MySQL 元数据 → 清 Redis 缓存；
  图谱这一步失败只写 `graph_warning`，不阻断删除。
- **测试**：`rag_test.go`（分流/去重/删除/统计/重复入库清旧）、`tenant_test.go`（跨租户）、
  `graphrag/ingest_test.go`（旧引用先摘后写、他人引用不误删）。

## 与 k8s 部署的关系

Milvus 侧仍可用 `rag-system/k8s/rag-system.yaml`；MySQL/Redis 用 `deploy/compose.yaml`，
后续如整体上 k8s，只需把 compose 里的 mysql/redis 换成对应 Deployment + PVC。
