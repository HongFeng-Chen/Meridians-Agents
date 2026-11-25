# Skill Runtime 设计方案（V2）

> 目标：让「技能」从**提示词片段**升级为**可注册、可发现、可分布式执行的能力单元**，
> 同时保持 V1 的 local 语义不破坏（存量技能一个都不用改）。

## 1. 问题

V1 的技能只有一种用法：调用 `skill:<name>` → 把 `SKILL.md` 塞进上下文，模型按说明自己干活。
这带来三个天花板：

| 问题 | 表现 |
| --- | --- |
| 能力边界 | 只能在「模型能自己做的事」范围内，解析 PDF、查内部库这类必须调代码 |
| 无法复用 | 同一个能力被多个 Agent 需要时，只能复制提示词 |
| 无法治理 | 谁在什么任务里调了什么、花了多久、失败原因，只有一行「技能使用」历史 |

## 2. 分层

```
Agent / API / 异步派发
        │  RuntimeRequest{skill, input, task, tenant, idem_key}
        ▼
┌─────────────────────────────────────────────┐
│ Skill Runtime（biz/skill/runtime.go）        │  唯一入口
│  · 读技能定义 → 解析 execution spec          │
│  · 幂等回放 · 重试 · 记账                    │
└───────┬──────────────────┬──────────────────┘
        │ local            │ remote / async
        ▼                  ▼
   SKILL.md 进上下文   Scheduler（scheduler.go）
                           │ 健康过滤 · 加权最小负载 · 熔断
                           ▼
                      WorkerClient（data/third_service/skillworker）
                           │ POST {endpoint}/execute
                           ▼
                      Skill Worker（任意语言）
```

职责边界（对应「定义 / 调度 / 执行」三者解耦）：

| 模块 | 职责 | 代码 |
| --- | --- | --- |
| 技能定义 | 描述、版本、参数 Schema、执行配置 | `skills.execution_spec` + `skill.go` |
| 注册中心 | Worker 实例元数据、心跳、健康度 | `skill_instances` + `repo/skill_runtime.go` |
| 调度 | 选实例、背压、熔断、换实例重试 | `scheduler.go` |
| 执行 | 真正跑代码 | 外部 Worker（`examples/skill-worker-python`） |
| 账本 / 队列 | 每次执行的记录；异步任务即队列 | `skill_executions` |
| 派发 | 领取 queued、租约、回收 | `async.go` |

## 3. 数据模型

**skills.execution_spec**（JSON，`ExecutionSpec`）

```json
{
  "mode": "remote",              // local | remote | async
  "protocol": "http",
  "endpoint": "http://py-worker:9101",
  "handler": "pdf_analysis",     // Worker 实现名，缺省 = 技能名
  "input_schema": "{...}",       // 进 Agent 能力描述，供模型生成参数
  "timeout_sec": 30,
  "retry": 1,
  "concurrency": 4,
  "idempotent": true,
  "required_scopes": ["file:read"]
}
```

**skill_instances**：`skill_name + instance_id` 唯一，存 endpoint / 权重 / 并发上限 /
在途数 / 状态 / 心跳 / EWMA 延迟 / 连续失败数。

**skill_executions**：字符串主键 `exec_<hex>`，存任务、租户、用户、Agent、会话、幂等键、
input/output 摘要、mode、status、worker、attempts、latency、trace_id、租约与三个时间点。

## 4. 关键流程

### 4.1 同步（remote）

1. 读技能 → `spec.Normalized()`（补默认值）→ 未启用直接拒。
2. 幂等：`spec.Idempotent && key != ""` → `FindExecutionByKey(tenant, skill, key)` 命中则回放。
   查询带租户：幂等键由调用方给，租户间撞键不能互通结果。
3. 落 `running` 执行记录（attempts=0）。
4. 循环 `1+retry` 次：`pickInstance` → `WorkerClient.Execute`。
   - 成功：写 output/worker/latency，`RecordResult(ok)`，返回文本。
   - 失败：`RecordResult(fail)`（连续失败 +1，达阈值熔断）、加入 exclude、下一次换实例。
5. 全部失败：状态落 `failed` 或 `timeout`，**仍然返回结果对象**（让 Agent 看到原因，而不是整轮崩掉）。

`pickInstance` 的兜底顺序：实例表为空 → 用 `spec.Endpoint`（虚拟实例 `static:<技能名>`）；
调度器选不出健康实例（全部过期/熔断/满载）→ 同样退到 `spec.Endpoint`。
第二条是必要的：Worker 换机器后旧实例会一直留在表里（心跳过期 → offline），
只看「表为空」会让配了 endpoint 的技能因为一条死记录永久不可用。

### 4.2 异步（async）

1. 落 `queued` 记录并立即返回 `execution_id`。
2. 派发协程（`SKILL_RUNTIME_WORKERS` 个）每 `SKILL_POLL_INTERVAL` 轮询：
   `RequeueStale`（租约过期回收）→ `ClaimQueued`（`FOR UPDATE SKIP LOCKED` + 租约）→ `runRemote`。
3. 调用方：`GET /api/skills/executions/{id}` 或 Agent 能力 `skill:status`。

### 4.3 调度打分

```
score = weight × (1 - inflight/max_concurrency) ÷ (1 + latency_sec)
排序：score ↓ → latency ↑ → instance_id ↑（确定性，可复现）
过滤：心跳过期 / offline / draining / failures ≥ 3 / 在途满
退化：全部不健康时仍返回「在线但熔断」的实例（宁可慢，别直接失败）
```

## 5. 为什么这么选

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 异步传输 | MySQL 队列 + 租约 | 执行记录本身就是账本，状态与账本同源；不引入 Kafka 也能多副本；换 MQ 只需替换 `ClaimQueued` |
| 远程协议 | HTTP `/execute` | 语言无关、可 curl 调试；gRPC 版本可作为 `WorkerClient` 的另一实现接入 |
| 注册中心 | MySQL + TTL | 部署形态不变（无 Consul/etcd 依赖）；K8s 下可由 `Service` + 心跳共存 |
| 幂等 | 执行记录 + 幂等键 | 与账本复用同一张表，不需要额外的去重存储 |
| local 也走 Runtime | 是 | 三种模式对 Agent 只有一个入口；差异在 spec，不在调用方 |

## 6. 已知边界（下一版可做）

- **并发配额**：目前只在注册时限制 `max_concurrency`，运行时用 `inflight` 做软背压，没有硬性信号量。
- **租户级路由**：实例有 `labels` / `region`，但调度未按租户亲和过滤（留给策略扩展点）。
- **流式输出**：`WorkerResponse` 是一次性 JSON，SSE/gRPC stream 未接入。
- **沙箱**：Worker 侧执行环境由部署方负责（Docker / gVisor），平台只做超时与资源契约。
- **审计联动**：高危技能（如写操作）可复用 `agentfactory` 的审批闸门，目前只在能力入口做授权。

## 7. 测试

- `scheduler_test.go`：选实例的 7 种场景（负载、权重、过期、排空、熔断、exclude、退化）。
- `runtime_test.go`：local/remote/async、幂等回放、**幂等按租户隔离**、换实例重试、无实例、
  直连兜底、**实例全过期时仍走直连兜底**、停用拒绝。
- `third_service/skillworker/client_test.go`：信封字段、鉴权头、HTTP 错误、裸 JSON 兼容、出站拦截。
- `agentfactory_test.go`：能力目录表述、参数拆分（purpose vs input）、`skill:status`。
