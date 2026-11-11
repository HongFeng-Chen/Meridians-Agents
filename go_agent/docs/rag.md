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
| POST | `/api/ingest` | `{doc_id,source,text,mode,scope}` 导入文本（mode=rag/graph，scope 见下节） |
| POST | `/api/query` | `{question,filter}` 混合检索问答 |
| POST | `/api/graph/ingest` | 图谱构建 |
| POST | `/api/graph/query` | 图谱问答 |
| GET | `/api/admin/stats` | 分块/文档/实体/关系统计 |
| GET | `/api/admin/docs?scope=system` | 文档列表（来源 MySQL；只返回当前身份可见的三层知识，`scope` 可再收窄） |
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
- **租户隔离**：`rag_documents` 加 `tenant_id`（默认 `public` 回填历史行）；
  `Ingest` 会拒绝覆盖别的租户的 `doc_id`。检索时把租户 + 用户白名单下推到 Milvus 过滤表达式。
- **三层知识（作用域）**：`rag_documents.scope`，取值 `system` / `tenant` / `user`（默认）。
  规则与实现见下节「三层知识」。
- **删除顺序**：先删分块 → `DetachDocRefs` 摘图谱引用 → 删 MySQL 元数据 → 清 Redis 缓存；
  图谱这一步失败只写 `graph_warning`，不阻断删除。
- **测试**：`rag_test.go`（分流/去重/删除/统计/重复入库清旧）、`tenant_test.go`（跨租户）、
  `graphrag/ingest_test.go`（旧引用先摘后写、他人引用不误删）。

## 三层知识：平台通用 / 租户共享 / 个人私有

一条文档的可见范围由 `scope` + `user_id` + `tenant_id` 三者共同决定，
判定只有一处实现（`internal/biz/rag/scope.go` 的 `Visible` / `CanWriteDoc`），
仓储 SQL（`DocumentRepo.visibilityCond`）是它的等价改写。

| scope | 谁能看见 | 谁能写 / 删 | 典型内容 |
| --- | --- | --- | --- |
| `system` | 所有租户、所有用户 | 平台管理员（`platform:config`） | 产品手册、协议条款、通用规范 |
| `tenant` | 同租户的所有用户 | 租户管理员（`tenant:manage`）或贡献者本人 | 部门制度、客户名单、项目背景 |
| `user`（默认） | 仅本人 | 本人 | 个人笔记、草稿、一次性资料 |

可见性判定（任一成立即可见）：

```
scope = 'system'
OR (scope = 'tenant' AND tenant_id = 当前租户)
OR (scope = 'user'   AND tenant_id = 当前租户 AND user_id = 当前用户)
```

几个刻意的取舍：

- **作用域落在 MySQL，不进 Milvus**。检索隔离本来就是「MySQL 算 doc_id 白名单 →
  Milvus 过滤 `doc_id in [...]`」，白名单先于向量检索生效，三层知识只是把这条规则
  从「按人」推广成「按层」，不需要动向量库 schema（标量字段不支持在线加列，
  改 schema 等于重建集合 + 重灌全部向量）。
- **GraphRAG 自动跟随**。图谱节点的归属是 `chunk_refs` 里的 `doc#idx`，
  可见文档集合一变，`RecallScoped` / `QueryScoped` 的召回边界就跟着变，
  不需要在实体上再存一份 tenant/user。
- **默认最保守**。不传 `scope` 一律当 `user`；未知取值直接 400 而不是静默降级——
  「写错了但看起来成功了」会让本该私有的内容以更宽的可见性落库。
- **不能靠换作用域抢文档**。`doc_id` 全局唯一，重灌时按「文档既有作用域」判定
  （`CanWriteDoc`），租户管理员也不能把别人的私有文档改成租户共享。
- **作者可以升/降自己的文档**：`scope` 参与 `ON DUPLICATE KEY UPDATE`，
  个人笔记想共享给团队时直接重导入并改 `scope` 即可。

### V2：空间白名单优先，三层作用域兜底

V2 引入知识空间后，可见性判定多了一层前置：文档可以挂到某个空间（`rag_documents.space_id`），
`rag.visibleDocIDs` 先问 `space.SpaceScope.AllowedSpaceIDs(ctx, …)` 拿到可读空间集合，再换算成 `doc_id` 白名单。

| 情况 | 判定 |
| --- | --- |
| 文档有 `space_id` | 按空间树 + 继承（home / container / isolated）+ 显式共享判定；个人空间 `isolated` 不继承父级 |
| 文档无 `space_id`（旧数据） | 回落到上面的三层作用域表达式，行为与 V2 之前完全一致 |

- 白名单仍在 MySQL 算、仍然下推成 Milvus 的 `doc_id in [...]` 过滤——**没有改向量库 schema**，
  所以升级不需要重建集合。
- GraphRAG 共用同一份白名单（`visibleDocIDs`），因此图谱召回边界与检索一致。
- 已知边界：白名单是表达式，文档量大到超过 Milvus 表达式长度上限时要改「按空间分集合」或标量过滤
  （`docs/architecture-v2.md` §13 第 1 条）。

已知边界（后续要收的话按这个方向）：

- 图谱实体是**跨作用域合并**的（同名实体累加 `chunk_refs`，描述取最长的一段）。
  因此一条私有文档可能让公共实体的描述里含一句只有作者写过的表述。
  严格隔离需要把私有知识放到独立 collection，或给实体加 `scope` 列（要重建集合）。
- 记忆（`memories`）还没有 `tenant_id`，与知识库的三层模型尚未对齐。

## 与 k8s 部署的关系

Milvus 侧仍可用 `rag-system/k8s/rag-system.yaml`；MySQL/Redis 用 `deploy/compose.yaml`，
后续如整体上 k8s，只需把 compose 里的 mysql/redis 换成对应 Deployment + PVC。
