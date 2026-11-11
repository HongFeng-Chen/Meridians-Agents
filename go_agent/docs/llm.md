# 大模型多数据源（llm biz）

大模型配置从 `config.yaml` 迁到 **MySQL（`llm_providers`）**，支持同时接入多个
数据源（DeepSeek / OpenAI / Ollama / 通义 / SiliconFlow / 任意 OpenAI 兼容网关），
在线切换默认源、按场景路由、失败自动降级，**改配置不需要重启**。

```
管理端 ──POST /api/llm/providers──► MySQL llm_providers
                                  └─► Manager.Invalidate()（本实例快照立即失效）

线上问答 ──rag/memory Chat()──► Manager.candidates(scene)
        ├─ 场景命中（scenes 含 rag/graph/extract/memory）
        ├─ 默认源（is_default）
        ├─ 优先级（priority 降序）
        └─ 全部失败 ──► config.yaml data.llm（兜底，服务不中断）
```

## 分层位置

| 层 | 文件 | 职责 |
|---|---|---|
| biz | `internal/biz/llm/{types,ports,llm,manager}.go` | DO、解析路由、多源故障切换、管理用例、HTTP 客户端 |
| data | `internal/data/db/models`（`LLMProviderPO`）、`internal/data/db/repo/llm.go` | PO 与仓储实现（默认标记事务内唯一） |
| service | `internal/service/llm_service.go` | `/api/llm/**` 路由，仅做协议转换 |

业务方（`biz/rag`、`biz/memory`、`biz/rag/graphrag`）只依赖 `rag.Chatter`
（`Chat` / `ChatTemp` / `Model`），由 `*llm.Manager` 实现，因此换数据源对业务透明：

```go
wire.Bind(new(rag.Chatter), new(*llm.Manager))
```

## 数据模型 `llm_providers`

| 列 | 说明 |
|---|---|
| `name` | 唯一标识（字母数字开头，可含 `. _ -`，≤64），接口按 name 操作 |
| `kind` | 展示用类型：`openai` / `deepseek` / `ollama` / `qwen` / `siliconflow` / `moonshot` / `custom` |
| `base_url` | OpenAI 兼容根地址，调用 `{base_url}/chat/completions`、`{base_url}/models` |
| `api_key` | 明文存储；接口输出一律脱敏（`****后4位`），更新时传空表示保持原值 |
| `models` | JSON 数组，候选模型清单（前端下拉用） |
| `default_model` | 实际调用模型；缺省取 `models[0]` |
| `temperature` / `max_tokens` / `timeout_seconds` | 默认推理参数（图谱抽取仍强制温度 0） |
| `enabled` | 停用后不参与路由 |
| `is_default` | 全局默认源；切换时事务内先清空其它默认 |
| `priority` | 同场景/同为默认时的排序权重（大者优先） |
| `scenes` | JSON 数组：`general` / `rag` / `graph` / `extract` / `memory`；空视为通用 |
| `description` | 备注 |

**播种**：启动时若表为空且 `data.llm.baseURL` 非空，自动写入名为 `default` 的数据源
（内容来自原配置），所以升级不需要手工插数据。

## 路由规则

1. 只考虑 `enabled = true` 且 `base_url` / 模型齐全的数据源；
2. 候选排序：`is_default` → `priority` 降序 → `name`；
3. 指定 `scene` 时，声明该场景的源排在前面，其余源仍作为后备；
4. 逐个尝试，任一成功即返回；全部失败落到 `data.llm` 兜底配置；
5. 快照缓存 `reloadTTL`（默认 30s）：管理接口变更立即失效本实例，多实例靠 TTL 收敛。

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/llm/providers?all=1` | 数据源列表（默认只返回启用的；密钥脱敏） |
| POST | `/api/llm/providers` | 新增（`name` / `base_url` / `default_model` 必填） |
| GET | `/api/llm/providers/{name}` | 详情 |
| PATCH | `/api/llm/providers/{name}` | 修改（`api_key` 传空串表示不改） |
| DELETE | `/api/llm/providers/{name}` | 删除（禁止删唯一启用的源） |
| POST | `/api/llm/providers/{name}/enable` · `/disable` | 启停 |
| POST | `/api/llm/providers/{name}/default` | 设为默认 |
| POST | `/api/llm/providers/{name}/test` | 连通性测试（返回延迟与回复） |
| GET | `/api/llm/providers/{name}/models` | 拉取远端 `/models`（不落库） |
| GET | `/api/llm/current` | 当前生效模型与默认源 |
| GET | `/api/llm/scenes` | 内置场景清单 |
| GET | `/api/llm/stats` | 数据源数 / 启用数 / 默认源 |
| POST | `/api/llm/chat` | 调试对话：`{provider?, scene?, system?, user, temperature?}` |

错误码：不存在 → 404，重名 → 409，配置非法 / 无可用源 / 删唯一启用源 → 400。

### 示例

```bash
# 接入本地 Ollama（无需 key）
curl -s -XPOST localhost:8000/api/llm/providers -H 'Content-Type: application/json' -d '{
  "name":"local-ollama","kind":"ollama","base_url":"http://localhost:11434/v1",
  "models":["qwen2.5:7b"],"default_model":"qwen2.5:7b","timeout_seconds":180,
  "scenes":["general","extract"]
}'

# 接入 DeepSeek 并设为默认
curl -s -XPOST localhost:8000/api/llm/providers -H 'Content-Type: application/json' -d '{
  "name":"deepseek","kind":"deepseek","base_url":"https://api.deepseek.com",
  "api_key":"sk-xxx","models":["deepseek-chat","deepseek-reasoner"],
  "default_model":"deepseek-chat","is_default":true,"scenes":["rag","graph","memory"]
}'

# 测连通性
curl -s -XPOST localhost:8000/api/llm/providers/deepseek/test | jq

# 用指定数据源调试
curl -s -XPOST localhost:8000/api/llm/chat -H 'Content-Type: application/json' \
  -d '{"provider":"deepseek","user":"用一句话解释 BM25"}'

# 拉远端模型清单，回填 models
curl -s localhost:8000/api/llm/providers/deepseek/models | jq '.models[:5]'
```

## 配置

```yaml
data:
  llm:                      # 仅作兜底：MySQL 为空时播种 default，全部失败时兜底
    baseURL: ${LLM_BASE_URL:https://api.deepseek.com}
    apiKey: ${LLM_API_KEY:}
    model: ${LLM_MODEL:deepseek-flash}
    temperature: 0.3
    httpTimeout: 120s
    reloadTTL: 30s          # 数据源快照缓存时长（多实例最终一致）
```

## 运维要点

- **密钥**：`api_key` 明文存 MySQL——收紧库权限、不要给只读账号 SELECT 该列；
  接口侧只返回 `****1234` 与 `has_key`，日志不打印密钥
- **切默认源**：`POST /api/llm/providers/{name}/default`（立即生效，无需重启）
- **灰度**：新源先 `enabled=false` + `test` 验证，再 `enable` 并调高 `priority`
- **故障演练**：把默认源的 `base_url` 改错 → `test` 返回 `ok=false`，线上仍会按
  优先级/兜底继续回答（只打 warn 日志）
- **多实例**：变更只在当前实例立即生效，其余实例最多 `reloadTTL` 后收敛

## 调用记账与链路（V2）

所有调用通道（主链路 `Chat` / 指定数据源 `ChatSource` / conf 兜底 / `ChatWith` 带 options）最终都走
`Manager.callOnce`：**一次真实请求 = 一条 `llm.chat` span = 一条 `usage_records`**（重试 / 降级各算一条）。

| 环节 | 做什么 |
| --- | --- |
| 埋点 | `trace.Start(ctx, "llm.chat")`，写 `llm.model / llm.source / llm.purpose / prompt_tokens / completion_tokens / duration_ms` |
| 记账 | `tokens × 单价 = 微元`（整数域），`price_version` 由单价自动生成；异步写 `usage_records`，失败只 warn |
| 归属 | `llm.WithMeta` 把 `TaskID`（任务链路）或 `RefKind/RefID`（如 `harness_run:14`）带进记录；`WithMeta` 按字段合并，零值不改 |
| 落点 | `llm.CostSink` → `data/db/repo.CostSink` → `usage.Repo.Add`；绕过 `usage.UseCase.Record`（那是需要 principal 的用户自助上报） |

```bash
# 单任务成本 / 单次评测成本
curl -s 'localhost:8000/api/v2/usage/summary?task_id=112&group_by=kind'
curl -s 'localhost:8000/api/v2/usage/summary?ref_kind=harness_run&ref_id=14'

# 该任务的链路（span 树里 llm.chat 的条数应与上面 calls 相等）
curl -s 'localhost:8000/api/v2/traces?task_id=112'
```

新增调用通道时**必须**经过 `callOnce`，否则既没有链路也没有账单——
P4 首次实现就把 `chat()` 漏在外面，评测的模型直连调用在 trace 里完全不可见。

## 测试

```bash
go test ./internal/biz/llm/ -v
```

覆盖：conf 播种、场景路由、多源失败自动切换（httptest 模拟上游 500/200）、
密钥脱敏、创建校验（名称/base_url/模型）、删除唯一启用源被拒、连通性测试、
成本换算与 `price_version`、`WithMeta` 字段合并（ref 不被下游覆盖）。
