# 技能管理与技能市场（skill biz）

技能（Skill）从"代码里写死的提示词片段/脚本"变成 **MySQL 里可动态管理的能力包**：
安装、启停、升级、卸载全部走接口；支持从**技能市场（skillhub）**搜索并一键下载安装；
启用后可选择落盘到 `sync_dir`，供运行时/Agent 直接读取 `SKILL.md` 与脚本。

```
管理端 ──POST /api/skills──────────► MySQL skills + skill_files（权威存储）
       ──POST /api/skills/from-hub──► skillhub HTTP ──► 下载 zip/JSON 技能包 ──┘
                                          │
启用技能 ──► sync_dir/<name>/{SKILL.md, 文件…, manifest.json}（可选，落盘供运行时读取）
```

## 分层位置

| 层 | 文件 | 职责 |
|---|---|---|
| biz | `internal/biz/skill/{types,ports,skill}.go` | DO、安装/启停/卸载规则、路径安全、落盘、市场编排 |
| data | `internal/data/db/models`（`SkillPO`/`SkillFilePO`/`SkillHubPO`）、`internal/data/db/repo/skill.go` | PO 与仓储实现 |
| data（第三方） | `internal/data/third_service/skillhub/skillhub.go` | skillhub HTTP 客户端，实现 `skill.HubClient` |
| service | `internal/service/skill_service.go` | `/api/skills/**`、`/api/skillhub/**` 路由 |

## 数据模型

**`skills`**（技能主表）

| 列 | 说明 |
|---|---|
| `name` | 唯一标识（小写字母/数字开头，可含 `. _ -`，≤64）；同名安装视为升级 |
| `version` | 语义化版本字符串，缺省 `1.0.0` |
| `title` / `description` / `category` / `tags` | 展示与筛选（tags 存 JSON 数组） |
| `source` | `hub` / `upload` / `builtin` |
| `source_url` | 市场安装时记录来源地址 |
| `enabled` | 是否启用（启用才落盘） |
| `checksum` | SKILL.md + 全部文件（按路径排序）的 SHA-256，用于判断内容是否变化 |
| `content` | SKILL.md 正文（mediumtext） |
| `file_count` / `size` | 冗余统计，列表页不用扫文件表 |

**`skill_files`**：`(skill_name, path)` 唯一，存技能包里的文本文件；`Save` 时整体替换。

**`skill_hubs`**：技能市场数据源，`name` 唯一，`base_url` + 可选 `token`，
`is_default` 保证唯一（事务内切换）。

## 落盘规则（`data.skill.syncDir`）

- 启用/安装 → 写入 `sync_dir/<name>/`：`SKILL.md`、各文件（保留子目录）、`manifest.json`
- 停用/卸载 → 删除整个技能目录
- `POST /api/skills/sync` → 全量重建启用技能，并清理 `sync_dir` 下不再启用的目录（幂等，可定时跑）
- 路径安全：拒绝绝对路径、`..`、反斜杠；写盘后再次校验 `dest` 在技能目录内
- `syncDir` 为空 → 只存 MySQL，不写磁盘

## skillhub 协议约定

`base_url` 指向任意实现下列接口的 registry，`token` 以 `Authorization: Bearer` 发送：

| 方法 | 路径 | 返回 |
|---|---|---|
| GET | `{base}/api/v1/skills?q=<kw>&limit=<n>` | `{"skills":[{name,version,title,description,category,tags,downloads}]}` 或直接数组 |
| GET | `{base}/api/v1/skills/{name}` | 最新版技能包 |
| GET | `{base}/api/v1/skills/{name}/versions/{version}/download` | 指定版本技能包 |

技能包支持两种形态，按 `Content-Type`（含 `zip`）或 ZIP magic 自动识别：

1. **ZIP**：`SKILL.md` + 任意文本文件；若所有条目共享同一顶层目录会自动去前缀；
   `manifest.json` 里的 `name/version/title/...` 会覆盖元信息；二进制文件（图片/可执行）跳过
2. **JSON**：`{name,version,title,description,category,tags,content,files:[{path,content}]}`
   （也接受 `{"skill":{...}}` 包装）

下载后统一走本地校验（名称、路径、单文件 ≤ `maxFileBytes`），再落库/落盘——
市场内容不可信，校验必须在本地做。

### 解压限额（防 zip 炸弹 / 目录穿越）

只看「压缩包 ≤ 20MB」不够：高压缩比的小包能解出上百 GB，条目路径也可能指向技能目录之外。

| 限制 | 值 | 失败行为 |
| --- | --- | --- |
| 条目数 | `maxZipEntries = 512` | 报错拒绝整个包 |
| 单文件解压后 | `maxZipEntrySize = 2MB` | 报错（不是截断：截断会把残缺内容当正常文件收下） |
| 解压总量 | `maxZipTotalSize = 32MB` | 报错 |
| 压缩比 | `maxZipRatio = 200:1`（>1MB 的条目才检查） | 判定为可疑条目 |

路径统一走 `safeSkillPath`：拒绝 `..`、绝对路径、Windows 盘符；ZIP 条目与市场 JSON 的
`files[].path` **两条入口都要过**（只 `path.Clean` 挡不住 `C://…` 与 `/etc/passwd`）。
读条目时仍用 `io.LimitReader(size+1)` 兜底——zip 头里的声明大小可以伪造。

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/skills?q=&category=&source=&enabled=1&limit=` | 技能列表（不含文件正文） |
| POST | `/api/skills` | 手动安装/上传（`name` + `content` 或 `files`） |
| GET | `/api/skills/{name}` | 详情（含 SKILL.md 与文件正文） |
| DELETE | `/api/skills/{name}` | 卸载（删记录 + 删落盘目录） |
| POST | `/api/skills/{name}/enable` · `/disable` | 启停（联动落盘） |
| GET | `/api/skills/{name}/files` | 文件清单（路径 + 大小） |
| GET | `/api/skills/{name}/file?path=scripts/run.py` | 读单个文件正文（`SKILL.md` 也可） |
| POST | `/api/skills/sync` | 全量同步落盘 |
| GET | `/api/skills/stats` | 技能数 / 启用数 / 市场数 |
| GET | `/api/skills/from-hub?q=&hub=&limit=` | 搜索技能市场（标记 `installed`） |
| POST | `/api/skills/from-hub` | 从市场安装 `{name, version?, hub?, enable?}` |
| GET | `/api/skills/hubs` | 市场列表（token 脱敏） |
| POST | `/api/skills/hubs` | 新增市场 `{name, base_url, token?, is_default?}` |
| PATCH | `/api/skills/hubs/{name}` | 修改（token 传空表示不改） |
| DELETE | `/api/skills/hubs/{name}` | 删除（删默认市场会自动提升下一个启用市场） |
| POST | `/api/skills/hubs/{name}/default` | 设为默认市场 |
| GET | `/api/skillhub/search` · `/api/skillhub/hubs` | 市场的语义化别名 |

错误码：技能/市场不存在 → 404；名称、内容、路径、市场响应非法或未配置市场 → 400。

### 示例

```bash
# 上传一个技能（SKILL.md + 一个脚本）
curl -s -XPOST localhost:8000/api/skills -H 'Content-Type: application/json' -d '{
  "name":"pdf-tool","version":"1.0.0","title":"PDF 工具","category":"document",
  "tags":["pdf","extract"],
  "content":"# PDF Tool\n读取 PDF 文本与表格。",
  "files":[{"path":"scripts/run.py","content":"print(\"hi\")"}]
}'

# 配置官方市场（也可用 env SKILLHUB_BASE_URL 播种）
curl -s -XPOST localhost:8000/api/skills/hubs -H 'Content-Type: application/json' \
  -d '{"name":"official","base_url":"https://skillhub.example.com","is_default":true}'

# 搜索 → 安装 → 看文件 → 落盘
curl -s 'localhost:8000/api/skills/from-hub?q=pdf' | jq
curl -s -XPOST localhost:8000/api/skills/from-hub -H 'Content-Type: application/json' \
  -d '{"name":"pdf-tool","version":"1.2.0"}'
curl -s 'localhost:8000/api/skills/pdf-tool/file?path=SKILL.md' | jq -r '.content'
curl -s -XPOST localhost:8000/api/skills/sync | jq

# 停用后目录即消失
curl -s -XPOST localhost:8000/api/skills/pdf-tool/disable | jq '.enabled'
```

## 配置

```yaml
data:
  skill:
    syncDir: ${SKILL_SYNC_DIR:./data/skills}   # 为空则只存 MySQL
    hubBaseURL: ${SKILLHUB_BASE_URL:}          # skill_hubs 为空时播种默认市场
    hubToken: ${SKILLHUB_TOKEN:}
    maxFileBytes: 1048576                      # 单文件上限（1MB）
    httpTimeout: 30s                           # 市场请求超时
```

## 运维要点

- **升级**：同名再装一次即可（`version` 变化）；未显式传 `enabled` 时沿用原启停状态
- **不可信来源**：技能包只做"文本入库 + 落盘"，不会被执行；运行侧应自行沙箱执行脚本
- **原子写盘**：先删后写，写失败会返回错误——重试 `POST /api/skills/sync` 可自愈
- **市场不可用**：搜索/安装报 502/400，已安装技能不受影响（本地 MySQL 为准）
- **token 安全**：与 LLM 同理，明文存库、接口脱敏

## 测试

```bash
go test ./internal/biz/skill/ -v
```

覆盖：名称/内容/路径校验（含 `..`、绝对路径、超限文件）、安装落盘与 manifest、
启停联动删除/重建目录、sync 清理孤儿目录、市场安装（source/source_url/文件）、
搜索结果 installed 标记、未配置市场返回 `ErrNoHub`；`skillhub_test.go` 另覆盖
zip 路径穿越、条目数/单文件/压缩比超限、JSON 形态非法路径。
