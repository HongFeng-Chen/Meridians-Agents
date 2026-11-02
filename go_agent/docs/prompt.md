# 提示词工程（prompt biz）

RAG / GraphRAG 的提示词不再是代码里的常量，而是**可版本化管理的模板**：
改词、调温度、回滚、灰度前的对比，全部走接口，**不需要改代码、不需要重启**。

```
管理端 ──POST /api/prompts/{key}/versions──► MySQL prompt_versions（新版本，只增不改）
                                          └─► 激活指针 prompt_templates.active_version
                                          └─► DEL prompt:ver:{key}（Redis 缓存失效）

线上问答 ──Render(key, vars)──► Redis 缓存（60s） ──miss──► MySQL active_version
                                                └─失败──► 内置常量（兜底，服务不中断）
```

## 分层位置（遵循 AGENTS.md）

| 层 | 文件 | 职责 |
|---|---|---|
| biz | `internal/biz/prompt/{types,template,builtin,ports,prompt}.go` | DO、版本规则、模板渲染、内置兜底 |
| data | `internal/data/db`（MySQL：models + repo/prompt.go）、`internal/data/redis`（Redis：cache_prompt.go） | PO 与仓储实现 |
| service | `internal/service/prompt_service.go` | `/api/prompts/**` 路由，仅做协议转换 |

`biz/prompt` 不导入 gorm/redis；`service` 不碰存储。

## 数据模型

**`prompt_templates`**（模板元信息 + 生效指针）

| 列 | 说明 |
|---|---|
| `prompt_key` | 唯一标识，如 `rag.qa.system`（字母开头，可含 `_ . -`，≤128） |
| `scene` | `rag` / `graph` / `extract` / `general`，列表筛选用 |
| `name` / `description` / `tags` | 管理端展示信息（tags 存 JSON 数组） |
| `active_version` | 当前生效版本号；0 表示未激活 |
| `builtin` | 内置模板标记，禁止删除（可 reset） |

**`prompt_versions`**（版本快照，写后不改）

| 列 | 说明 |
|---|---|
| `(prompt_key, version)` | 联合唯一索引，版本号由事务内 `MAX(version)+1` 分配 |
| `content` | 模板原文，含 `{{变量}}` |
| `variables` | 发布时按内容自动解析，供前端渲染表单 |
| `temperature` / `max_tokens` | 该版本建议的模型参数（记录用，RAG 侧按需读取） |
| `note` / `author` | 变更说明与操作人 |

版本只增不改：改词 = 发布新版本。回滚 = 把 `active_version` 指回旧版本，历史可追溯。

## 模板语法

- 占位符：`{{name}}`，允许花括号内空格（`{{ name }}`），变量名限 `[A-Za-z_][A-Za-z0-9_.-]*`
- 未提供的变量：正式渲染报错（`MissingVarsError`），预览接口只列出不报错
- 提供空字符串视为合法取值（例如空上下文）
- 不支持条件/循环——提示词保持"扁平"，复杂逻辑放代码里，避免模板变成脚本

## 内置模板（首次启动自动写入，之后可自由改）

| key | 场景 | 变量 |
|---|---|---|
| `rag.qa.system` | RAG 系统提示词 | — |
| `rag.qa.user` | RAG 用户提示词 | `context`、`question` |
| `graph.answer.system` | 图谱问答系统提示词 | — |
| `graph.answer.user` | 图谱问答用户提示词 | `context`、`question` |
| `graph.extract.system` | 实体关系抽取系统提示词 | — |
| `graph.extract.user` | 实体关系抽取用户提示词 | `text` |

内置原文同时是**运行兜底**：MySQL/Redis 不可用、模板被误删时，渲染回落到内置常量，
问答链路不中断（`internal/biz/prompt/builtin.go`）。

## 接口

静态路径（`stats`、`builtins`）注册在 `{key}` 之前，避免被通配路由吞掉。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/prompts?scene=rag&q=问答&limit=50` | 列表（`scene` / `q` / `limit`） |
| POST | `/api/prompts` | 新建模板（v1 直接生效） |
| GET | `/api/prompts/{key}` | 详情（含生效版本 `active`） |
| PATCH | `/api/prompts/{key}` | 改元信息（name/scene/description/tags） |
| DELETE | `/api/prompts/{key}` | 删除模板及全部版本（内置禁止） |
| GET | `/api/prompts/{key}/versions` | 版本列表（倒序） |
| POST | `/api/prompts/{key}/versions` | 发布新版本（`activate` 缺省 true） |
| GET | `/api/prompts/{key}/versions/{version}` | 指定版本 |
| POST | `/api/prompts/{key}/activate` | `{version}` 切换生效版本 |
| POST | `/api/prompts/{key}/rollback` | 回滚到上一个版本 |
| POST | `/api/prompts/{key}/render` | `{vars, version?}` 预览渲染（缺变量只列出） |
| POST | `/api/prompts/{key}/reset` | 用内置原文发布新版本并生效 |
| GET | `/api/prompts/stats` | 模板数 / 版本数 |
| GET | `/api/prompts/builtins` | 代码内置模板清单（前端「恢复默认」用） |

错误码映射：不存在 → 404，key 重复 → 409，key/内容非法、内置受保护、缺变量 → 400。

### 示例

```bash
# 看内置模板与当前版本
curl -s localhost:8000/api/prompts/rag.qa.system | jq

# 改词并立即生效
curl -s -XPOST localhost:8000/api/prompts/rag.qa.system/versions \
  -H 'Content-Type: application/json' \
  -d '{"content":"你是知识库助手。只依据资料回答，资料没有就说不知道。引用标注 [1]。","note":"收紧编造","author":"alice"}'

# 预览（改词前先看渲染结果）
curl -s -XPOST localhost:8000/api/prompts/rag.qa.user/render \
  -H 'Content-Type: application/json' \
  -d '{"vars":{"context":"[1] Milvus 支持混合检索","question":"支持什么检索？"}}'

# 改坏了回滚
curl -s -XPOST localhost:8000/api/prompts/rag.qa.system/rollback | jq '.active.version'

# 彻底恢复内置原文（作为新版本，历史仍可查）
curl -s -XPOST localhost:8000/api/prompts/rag.qa.system/reset -d '{"author":"alice"}'
```

## 配置

`configs/config.yaml`：

```yaml
data:
  prompt:
    cacheTTL: 60s      # 生效版本缓存；发布/激活会立即失效，TTL 只是兜底
    seedBuiltin: true  # 启动补齐内置模板（已存在则跳过，不覆盖线上改动）
    listLimit: 200     # 列表单次上限
```

## 运维要点

- **改词流程**：`render` 预览 → `POST /versions`（默认即生效）→ 观察 `/api/query` 效果 → 不满意 `rollback`
- **只存草稿**：`POST /versions` 带 `"activate": false`，稍后 `activate` 指定版本
- **缓存一致性**：发布/激活/删除/重置都会 `DEL prompt:ver:{key}`；多实例部署下所有实例共用 Redis，一次失效全局生效
- **数据库不可用**：渲染自动回落内置常量并打 warn 日志，问答不 500；管理接口会正常报错
- **不要删内置 key**：`rag.qa.*` / `graph.*` 由代码直接引用，删除会被拒绝（`ErrBuiltinProtected`）

## 测试

```bash
go test ./internal/biz/prompt/ -v
```

覆盖：变量解析与渲染边界（多字节、非法占位符、空值）、内置模板自检、种子幂等、
发布/草稿/激活/回滚、缓存命中与失效、缺变量报错、存储故障回落、
RAG 侧确实使用管理端模板（`internal/biz/rag/rag_test.go`）。
