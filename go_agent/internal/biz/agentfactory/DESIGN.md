# Agent 工厂设计讨论稿（DESIGN）

> **状态：历史讨论稿，非实现说明。** 本文是「Agent 低代码化 + LLM 元编程」方案成形前的对话整理，
> 保留是为了记录当时的取舍。**当前实现以 [`../HANDOVER.md`](../HANDOVER.md) 2.2 为准**。
>
> 讨论中的关键结论与落地情况：
>
> | 设计点 | 落地 |
> | --- | --- |
> | Agent 是数据不是代码（`agent_definitions` + 向量召回） | ✅ |
> | 可插拔能力白名单（`agent:<key>` / `mcp:<unique_name>`） | ✅ `capability.go` |
> | 把「创建 Agent」做成 tool（`create_agent`） | ✅ `factory.go`（配额上限 200、白名单、防自举） |
> | 执行图可配（13 种含 auto + DAG Flow） | ✅ `graphs.go` / `flow.go` / `graph.go` |
> | 沙箱 Agent 不得委派 | ✅ `ErrSandboxDeny`（`TestSandboxDenyDelegation`） |
> | 异步任务队列 + 崩溃恢复 + 定时调度 | ✅ `agentfactory.go` / `cron.go` |
> | 停机不腰斩任务 | ✅ `Drain`（见 HANDOVER 2.2「优雅停机」） |
> | 讨论中的「Agent 自进化 / 自动改自己的定义」 | ❌ 未做（有自举护栏，刻意不做） |

---

agent 可以类似低代码那种的进行配置，而不是固定的，另外如果创建一个 tool 让 llm 自定义决定需要创建什么类型的 agent ，自定义 agent 的能力，以及其他驾驭工程的 agent 会更灵活

你这句「让 LLM 自定义决定创建什么类型的 Agent」点到了**第二层灵活性（元编程）**，我先把它讲透再上图。

## 先给结论

之前我们聊的「可插拔能力 + 执行图 + 中间件」是**第一层灵活性**——系统层面，结构可配。你现在补的这两点是**第二层灵活性**，分两块：

1. **Agent 本身低代码化**：Agent 不是一个写死的 Go 结构体，而是一份可配置的「定义」，像低代码平台的表单一样，改配置就改 Agent，不用改代码。
2. **把「创建 Agent」做成一个 Tool**：让 LLM 把它当成普通能力调用，LLM 自己判断「当前任务需要什么 Agent、带什么能力」，动态生成 Agent 定义并注册——这就是**元编程 / 自进化**。

两层叠加，才叫真正灵活的平台。下面拆开讲。

## 关键设计 A：Agent 低代码化（DSL 配置，而非硬编码）

Agent 实例 = 一份 YAML/JSON「定义」+ 一个通用的 Agent Runtime。定义驱动一切：

```
agentId: research-agent
name: 调研助手
llm:
  provider: ollama          # 可随时换模型
  model: qwen2.5:72b
capabilities:               # 能力白名单，这是权限边界
  - mcp:github__search
  - a2a:expert-doc-agent
  - local:calc
graph: react.yaml           # 推理策略：ReAct/PlanAndSolve/Reflexion
memory: redis               # 记忆后端可选
middlewares: [auth, ratelimit, audit]
maxTurns: 20
```

运行时：Agent Runtime 读这份定义 → 挂载 LLM、注册能力、加载执行图、套中间件 → 成为一个可服务的 Agent 实例。**新增 Agent = 加一条配置，不需要写一行 Go 代码。**

## 关键设计 B：AgentFactory —— 把「造 Agent」本身做成 Tool（元编程）

这是你这句话的精髓。平台内置一个特殊 Capability：`create_agent`。

```
type CreateAgentArgs struct {
    Name         string   `json:"name"`
    Description  string   `json:"description"`          // 这个agent用来干嘛
    Capabilities []string `json:"capabilities"`          // 需要哪些工具/子agent
    GraphType    string   `json:"graph_type"`            // 用哪种推理策略
    MaxTurns     int      `json:"max_turns"`
}

func (f *AgentFactory) Invoke(ctx context.Context, args map[string]any) (CapabilityResult, error) {
    // 1. LLM 已经给出要建的 Agent 定义
    // 2. 校验：能力白名单是否越权、graph 是否存在、参数合法性
    if err := f.validate(args); err != nil { return nil, err }
    // 3. 生成 agentDef，落 MySQL + 同步 Milvus 向量 + Redis 缓存
    def := buildAgentDef(args)
    f.registry.Save(def)
    // 4. 注册到 Agent Runtime 能力池，立即可被其他 Agent 调用
    f.runtime.Register(def)
    return &CapabilityResult{AgentID: def.AgentID}, nil
}
```

于是整条链路变成：

1. 主 LLM 收到一个复杂需求 → 判断「这活需要个专门的 Agent，比如『合规审查 Agent』」
2. 主 LLM 调用 `create_agent`，传入 `{name: 合规审查, capabilities: [合同MCP, 法条检索], graph: Reflexion}`
3. `AgentFactory` 校验后生成定义、注册 → 返回新 Agent ID
4. 主 LLM 再通过 A2A 委派任务给这个新建的 Agent → 协作完成

**这就是「LLM 自己招人、自己组队干活」**——平台只提供工厂和注册表，Agent 的形态完全由 LLM 按需决定。

## 两者的关系（闭环）

```
人工(低代码)配置  ──┐
                   ├──► Agent 定义(MySQL) ──► 注册表(Milvus召回 + Redis缓存) ──► Agent Runtime ──► 调用能力(MCP/A2A)
LLM(AgentFactory)──┘                              ▲                                        │
                                             运行时新造Agent 又能被召回/委派 ◄──────────────┘
```

## 落地 Go 抽象（和之前那套完全兼容）

关键是 Agent 也实现统一的 `Capability` 接口——**一个 Agent 既能被调用，又能作为能力被更高层 Agent 调用**，这样就能无限嵌套组队：

```
// 一个 Agent 本身也实现 Capability
type AgentRuntime struct {
    Def          AgentDef
    LLM          LLM
    CapRegistry  *CapabilityRegistry   // 它自己挂载的能力
    Graph        ExecGraph
    Middlewares  []Middleware
}

// 关键：AgentRuntime 也实现 Capability，所以可以被别的 Agent 通过 A2A 调用
func (a *AgentRuntime) GetSchema() CapabilitySchema { return schemaFromDef(a.Def) }
func (a *AgentRuntime) Invoke(ctx context.Context, args map[string]any) (CapabilityResult, error) {
    return a.Run(ctx, args)  // 作为 A2A 被上层 Agent 委派
}
```

## 执行图：可插拔的架构模式

`graph_type` 决定「谁来决定下一步」，实现见 `graphs.go`，全部复用 `runToolLoop`（工具循环）
与 `runChildTask`（委派链路），因此换架构不影响能力白名单、风险风评与 trace 记录：

| graph_type | 控制流 | 适合场景 |
| --- | --- | --- |
| simple | 单轮直答 | 分类、抽取、改写 |
| react | 思考→行动→观察循环 | 通用多步任务 |
| plan_execute | 先出完整计划 → 逐步执行 → 失败重规划 | 长任务、可审计流程 |
| reflexion | 执行 → 评估打分 → 反思教训 → 重做 | 推理、写作、代码 |
| router | 意图判断后路由到技能/成员，不做自由循环 | 客服、业务问答 |
| self_ask | 拆子问题 → 逐个回答 → 汇总 | 轻量检索问答 |
| function_call | 无思考文本，直接选工具再作答 | 低延迟单步工具调用 |
| state_machine | 固定状态流转，模型只判断状态 | 强合规固定流程 |
| orchestrator | 总指挥派活给成员并汇总 | 跨专长协作 |
| hierarchical | 规划器拆子目标，成员细化执行 | 跨领域大任务 |
| debate | 正方生成 / 反方挑错 / 多轮收敛 | 方案评审、风险对抗 |
| blackboard | 成员共享一块状态板读写推进 | 方案推演、创意写作 |

`plan_execute / reflexion / self_ask / 多 Agent` 的中间产物不落 `final` 事件（`Recorder.silentFinal`），
只有真正的最终答案才记 final；计划、评估、反思、路由分别记 `plan / evaluate / reflect / route` 事件。

## ⚠️ 元编程的风险与护栏（必须做，否则失控）

LLM 能自己造 Agent，就意味着有自举失控风险，四个硬护栏：

1. **能力白名单授权**：`create_agent` 生成的 capabilities 必须落在平台已注册的合法集合内，不能凭空捏造新工具
2. **禁止无限自举**：不能生成一个「能造 Agent 的 Agent」再无限套娃（防递归失控）
3. **配额+审计**：每次 create 都要记账、限次数、可回滚（Agent 定义可版本回退，注册表里有 deleted_at 软删）
4. **新 Agent 必须先沙箱/灰度**：默认只读、低权限，验证通过才提升权限

## 一句话总结

**Agent 是「数据」（一份可配置的定义），不是「代码」；平台是「工厂 + 注册表 + 运行时」，让 LLM 既能低代码地配置 Agent，也能把「造 Agent」当工具调用，实现按需自组队。**

下图是这套「双层灵活性」的完整架构（示意）：

```
<html style="margin:0;padding:0;">
<title>LLM元编程驱动的灵活Agent平台架构</title>
<div style="font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;padding:16px;box-sizing:border-box;width:100%;color:#1f2937;background:#f7f8fa;">
  <div style="font-size:15px;font-weight:700;margin-bottom:4px;">LLM 驱动的灵活 Agent 平台（双层灵活性）</div>
  <div style="font-size:12px;color:#6b7280;margin-bottom:16px;">示意架构 · 非精确实现 · 数据流自上而下</div>

  <!-- 顶部：两个入口 -->
  <div style="display:flex;flex-wrap:wrap;gap:16px;">
    <div style="flex:1;min-width:280px;border:2px solid #2563eb;border-radius:10px;padding:12px;">
      <div style="font-size:13px;font-weight:700;color:#2563eb;margin-bottom:6px;">入口① 人工低代码配置</div>
      <div style="font-size:13px;line-height:1.7;">YAML/JSON 直接定义 Agent<br/>改配置 = 改 Agent，不写代码</div>
    </div>
    <div style="flex:1;min-width:280px;border:2px solid #0d9488;border-radius:10px;padding:12px;">
      <div style="font-size:13px;font-weight:700;color:#0d9488;margin-bottom:6px;">入口② AgentFactory 元编程</div>
      <div style="font-size:13px;line-height:1.7;">LLM 调用 create_agent Tool，<br/>按需自定义「造什么 Agent、带什么能力」</div>
    </div>
  </div>

  <!-- 下箭头 -->
  <div style="text-align:center;color:#9ca3af;margin:8px 0;">▼</div>

  <!-- Agent 定义 -->
  <div style="border:2px solid #6b7280;border-radius:10px;padding:12px;background:#ffffff;">
    <div style="font-size:13px;font-weight:700;color:#374151;margin-bottom:6px;">Agent 定义（一份可配置的「数据」）</div>
    <div style="display:flex;flex-wrap:wrap;gap:8px;font-size:12px;">
      <span style="background:#eef2ff;color:#3730a3;padding:4px 8px;border-radius:6px;">能力白名单</span>
      <span style="background:#eef2ff;color:#3730a3;padding:4px 8px;border-radius:6px;">LLM 选择</span>
      <span style="background:#eef2ff;color:#3730a3;padding:4px 8px;border-radius:6px;">执行图 Graph</span>
      <span style="background:#eef2ff;color:#3730a3;padding:4px 8px;border-radius:6px;">记忆后端</span>
      <span style="background:#eef2ff;color:#3730a3;padding:4px 8px;border-radius:6px;">中间件</span>
      <span style="background:#eef2ff;color:#3730a3;padding:4px 8px;border-radius:6px;">配额/权限</span>
    </div>
  </div>

  <!-- 下箭头 -->
  <div style="text-align:center;color:#9ca3af;margin:8px 0;">▼</div>

  <!-- 存储注册表 -->
  <div style="border:2px solid #d97706;border-radius:10px;padding:12px;background:#fff7ed;">
    <div style="font-size:13px;font-weight:700;color:#b45309;margin-bottom:6px;">注册表 &amp; 存储（定义持久化 + 语义发现）</div>
    <div style="display:flex;flex-wrap:wrap;gap:8px;font-size:12px;">
      <span style="background:#fff;color:#92400e;padding:4px 8px;border-radius:6px;border:1px solid #fcd34d;">MySQL 主数据</span>
      <span style="background:#fff;color:#92400e;padding:4px 8px;border-radius:6px;border:1px solid #fcd34d;">Milvus 能力/Agent 召回</span>
      <span style="background:#fff;color:#92400e;padding:4px 8px;border-radius:6px;border:1px solid #fcd34d;">Redis 热缓存/状态</span>
    </div>
  </div>

  <!-- 下箭头 -->
  <div style="text-align:center;color:#9ca3af;margin:8px 0;">▼</div>

  <!-- Agent Runtime -->
  <div style="border:2px solid #7c3aed;border-radius:10px;padding:12px;background:#f5f3ff;">
    <div style="font-size:13px;font-weight:700;color:#5b21b6;margin-bottom:6px;">Agent Runtime（通用执行引擎，读定义实例化）</div>
    <div style="font-size:12px;line-height:1.7;color:#4c1d95;">执行图驱动 · 中间件链 · 支持单 Agent 跑，也可被上层 Agent 委派</div>
  </div>

  <!-- 下箭头 -->
  <div style="text-align:center;color:#9ca3af;margin:8px 0;">▼</div>

  <!-- 能力层 -->
  <div style="border:2px solid #059669;border-radius:10px;padding:12px;background:#ecfdf5;">
    <div style="font-size:13px;font-weight:700;color:#065f46;margin-bottom:6px;">能力供给层（统一 Capability 接口）</div>
    <div style="display:flex;flex-wrap:wrap;gap:8px;font-size:12px;">
      <span style="background:#fff;color:#065f46;padding:4px 8px;border-radius:6px;border:1px solid #a7f3d0;">MCP Tool（stdio/http）</span>
      <span style="background:#fff;color:#065f46;padding:4px 8px;border-radius:6px;border:1px solid #a7f3d0;">A2A 子 Agent</span>
      <span style="background:#fff;color:#065f46;padding:4px 8px;border-radius:6px;border:1px solid #a7f3d0;">AgentFactory（造Agent）</span>
      <span style="background:#fff;color:#065f46;padding:4px 8px;border-radius:6px;border:1px solid #a7f3d0;">本地函数</span>
    </div>
  </div>

  <!-- 闭环箭头说明 -->
  <div style="margin-top:14px;border-left:4px solid #059669;background:#eff6ff;padding:10px 12px;font-size:12px;line-height:1.7;color:#1e40af;">
    <b>闭环：</b>新建的 Agent 注册后，又进入 Milvus/MySQL 可被召回，也能作为 A2A 能力被上层 Agent 委派 → 实现「LLM 自己招人组队」。
    <br/><b>护栏：</b>能力白名单授权、禁止无限自举、配额+审计+版本回滚、新 Agent 先沙箱灰度。
  </div>

  <!-- 图例 -->
  <div style="margin-top:14px;font-size:12px;color:#6b7280;line-height:1.8;">
    <b>图例：</b>蓝色=配置入口 · 青绿=元编程入口 · 灰色=Agent 定义 · 琥珀=存储注册表 · 紫色=运行时 · 绿色=能力层
  </div>
</div>
</html>
```

如果你要，我可以接着给你：

1. `CapabilityRegistry` + `AgentFactory` + `create_agent` Tool 的完整 Go 实现
2. 一套可校验的 Agent 定义 DSL（含能力白名单、graph 引用、配额字段的 schema）
3. 防自举失控的递归检测与沙箱机制代码。