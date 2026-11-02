# 记忆系统（Memory）

长期记忆走 **MySQL 权威存储 + Milvus 向量召回 + GraphRAG 图谱**，短期记忆走 **Redis（TTL 续期）**。分层与 RAG 一致：`service → biz/memory ← data`。

## 双层记忆模型

| 层 | 存储 | 生命周期 | 用途 |
|---|---|---|---|
| 短期 | Redis List（`mem:short:{session_id}`） | TTL 续期（默认 24h），会话静默自动过期 | 多轮对话上下文 |
| 长期 | MySQL `memories` + Milvus `{collection}_memories` + GraphRAG 图集合 | 显式过期时间（`expires_at`，nil = 永久） | 事实/偏好/事件的沉淀与召回 |

## 长期记忆的三份存储

1. **MySQL（权威）**：`memories` 表——内容、种类（fact/preference/event/insight/task）、重要度、标签、来源、`(user_id, checksum)` 唯一去重、`expires_at` 过期时间。
2. **Milvus（召回）**：`{collection}_memories` 集合——`content` 中文分词 + BM25 Function 生成稀疏向量，dense 存 Embedding（bge-m3），检索双路召回 RRF 融合，可按 `user_id` 过滤。集合残缺（有库无索引）启动时自动补建。
3. **GraphRAG（关联）**：记忆内容经 LLM 抽取实体/关系写入 **与 RAG 文档共享的图集合**——同一个人的记忆和文档会合并到同一批实体节点上，跨记忆、跨文档推理。单条记忆的引用前缀为 `{memory_id}#0`，删除/过期清理时只回收孤儿节点。

## 写入流程（POST /api/memory/long）

```
校验/补默认值
  -> (user_id, content) 查 checksum：命中且未过期 => 直接返回 deduped:true（不重复向量化/抽取）
  -> EmbedOne(content) => Milvus 记忆集合            （失败则整体失败）
  -> MySQL 落库（含 expires_at / checksum）           （失败则整体失败）
  -> GraphRAG 抽取实体关系 -> 与库中已有实体合并 -> 向量化入图集合
       （可关：graph:false 或配置 graphEnabled:false；失败降级为 graph_warning，不阻断）
```

## 召回流程（POST /api/memory/recall）

```
EmbedOne(query)
  -> Milvus 混合检索（BM25 + Dense，RRF）按 user_id 过滤
  -> MySQL GetByIDs 补全权威记录 + 过滤已过期（向量残留防御：MySQL 没有的 ID 跳过）
  -> [可选] 图谱召回：实体混合检索 -> 关系两跳扩展（去重） -> 子图
```

## 问答流程（POST /api/memory/answer）

短期历史（Redis）+ 长期召回（向量+图谱）组装上下文 → 提示词模板 `memory.answer.system` / `memory.answer.user`（走 /api/prompts 管理，回落内置）→ LLM 生成 → 本轮问答写回短期记忆（续期）。

## 接口一览

```
POST   /api/memory/short                {session_id, role, content}  追加消息（续期）
GET    /api/memory/short?session_id&limit                            最近消息
DELETE /api/memory/short?session_id                                  清空会话

POST   /api/memory/long                 写入（expire_at RFC3339 或 ttl_days 二选一；graph 可关）
GET    /api/memory/long?user_id&include_expired&limit                列表（默认滤掉已过期）
GET    /api/memory/long/stats           统计（总数/过期数/向量数/图谱规模）
POST   /api/memory/long/purge?limit     清理全部已过期记忆（MySQL+向量+图谱）
GET    /api/memory/long/{id}            详情
DELETE /api/memory/long/{id}            删除（三处同时清理）

POST   /api/memory/recall               {query, user_id, top_k, with_graph}
POST   /api/memory/answer               {session_id, user_id, question}
GET    /api/memory/info                 运行参数
```

## 配置（configs/config.yaml -> data.memory）

```yaml
memory:
  shortTTL: 86400s     # 短期会话 TTL（protobuf Duration 只认秒/毫秒，不认 24h）
  recallTopK: 8
  graphEnabled: true   # 记忆入库是否抽取图谱
  graphMaxEntities: 8
  graphMaxRelations: 6
  historyLimit: 20     # answer 携带的短期历史条数
```

## 运维要点

- **过期是查询时过滤 + 手动清理**：MySQL 侧 List/Recall 自动滤掉 `expires_at <= now`；向量与图谱里的残留靠 `POST /api/memory/long/purge` 回收（也可挂定时任务）。
- **图谱与 RAG 共享**：记忆实体与文档实体按 name 归一化合并、频率累加；删除记忆只删孤儿节点，不影响文档图谱。
- **去重键**：`(user_id, checksum)` 唯一索引，checksum = sha256(user_id + "\0" + content)；同内容重复写入零成本返回已有记忆。
- **Redis 挂掉**：短期历史按空处理（不阻断 answer），长期记忆与召回不受影响。
