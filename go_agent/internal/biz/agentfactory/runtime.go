package agentfactory

// AgentRuntime：通用执行引擎，读 AgentDef 实例化执行。
//
// 执行图（架构模式，见 graphs.go）：
//   - simple         单轮直答
//   - react          思考→行动→观察 循环（LLM 文本协议，最多 maxTurns 轮）
//   - plan_execute   规划-执行分离（先计划再逐步执行，失败重规划）
//   - reflexion      执行→评估→反思→重试
//   - router         意图路由到技能/成员，不做自由循环
//   - self_ask       自问自答（拆子问题逐个回答再汇总）
//   - function_call  原生单步工具调用（无思考文本）
//   - state_machine  固定状态流转，模型只做状态判断
//   - orchestrator / hierarchical / debate / blackboard  多 Agent 协作
//
// 中间件链：audit 恒开启——每轮思考/能力调用/结果/委派/最终答案全部落 trace 事件，
// 支撑任务链路检索与实时状态。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go_agent/internal/pkg/budget"

	"go_agent/internal/biz/history"
	"go_agent/internal/biz/llm"
)

// defaultMaxParallel 执行图并行波次的默认并发上限
const defaultMaxParallel = 4

// RunResult 一次执行的结果
type RunResult struct {
	Answer string `json:"answer"`
	Turns  int    `json:"turns"`
	// BudgetExceeded 是否因 token 预算耗尽而提前收敛（有部分结论，未跑完全部步骤）
	BudgetExceeded bool `json:"budget_exceeded,omitempty"`
}

// Recorder 任务 trace 记录器：事件落 MySQL，任务快照刷 Redis（实时状态轮询）
type Recorder struct {
	u    *UseCase
	task *Task
	def  *AgentDef
	turn int
	// silentFinal 为真时丢弃中间循环的 final 事件：
	// reflexion 的草稿、plan_execute 的单步产出都不是任务的最终答案
	silentFinal bool
}

func (r *Recorder) event(typ string, turn int, payload any) {
	if typ == EventFinal && r.silentFinal {
		return
	}
	_ = r.u.repo.AddEvent(r.u.baseCtx, &TaskEvent{
		TaskID:    r.task.ID,
		Turn:      turn,
		Type:      typ,
		Payload:   marshalPayload(payload),
		CreatedAt: time.Now(),
	})
}

func (r *Recorder) status(from, to string) {
	r.event(EventStatus, r.turn, map[string]any{"from": from, "to": to})
}

// snapshot 把任务当前态刷到 Redis，供前端轮询实时状态
func (r *Recorder) snapshot() {
	r.u.cache.SetTask(r.u.baseCtx, r.task)
}

// setProgress 更新进度并落快照
func (r *Recorder) setProgress(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 99 {
		pct = 99
	}
	r.task.Progress = pct
	r.u.repo.SaveTask(r.u.baseCtx, r.task)
	r.snapshot()
}

// —— system 提示词 ——

// buildSystemPrompt 组装系统提示词。
//
// 排版刻意「稳定在前、易变在后」：输出协议对所有 Agent 完全一致，放在最前面，
// 多任务/多 Agent 场景下请求前缀尽可能长地保持一致，命中率最高的前缀缓存
// （DashScope context cache / OpenAI 自动缓存）能省下大量输入 token；
// 角色设定与能力列表随 Agent 变化，放在后面。
func (u *UseCase) buildSystemPrompt(ctx context.Context, def *AgentDef) (string, error) {
	schemas, err := u.catalog.Schemas(ctx)
	if err != nil {
		return "", err
	}
	// 只保留白名单内的能力
	allowed := make(map[string]struct{}, len(def.Capabilities))
	for _, c := range def.Capabilities {
		allowed[c] = struct{}{}
	}
	var b strings.Builder

	// —— 稳定前缀：输出协议（所有 Agent 共享，利于前缀缓存命中）——
	b.WriteString(`## 输出协议（严格遵守）
每次只输出一个 JSON 对象，不要输出其他文字：

思考并行动：
{"thought": "简短推理", "action": {"name": "能力名", "args": {}}}

给出最终答案：
{"thought": "简短推理", "final": "面向用户的完整答案"}

规则：
1. args 必须是对象，符合能力参数示例
2. 只能使用上面列出的能力，不要编造
3. 信息足够就立即输出 final，不要空转
4. final 必须是最终答案本身，不要再调用能力
`)

	// —— 易变段：角色 ——
	b.WriteString("\n## 角色\n你是 Agent「")
	b.WriteString(def.Name)
	b.WriteString("」。\n")
	if def.Description != "" {
		b.WriteString("职责：")
		b.WriteString(def.Description)
		b.WriteString("\n")
	}
	if def.System != "" {
		b.WriteString("\n## 角色设定\n")
		b.WriteString(def.System)
		b.WriteString("\n")
	}
	b.WriteString("\n## 可用能力\n")
	if len(def.Capabilities) == 0 {
		b.WriteString("（无外部能力，直接思考作答）\n")
	} else {
		for _, s := range schemas {
			if _, ok := allowed[s.Name]; !ok {
				continue
			}
			// 参数示例只截取前 200 字符：超长示例对生成质量无益还烧 token
			fmt.Fprintf(&b, "- %s（%s）：%s\n  参数示例: %s\n", s.Name, s.Type, s.Description, truncateRunes(s.ArgsExample, 200))
		}
	}
	return b.String(), nil
}

// —— LLM 输出解析 ——

type llmAction struct {
	Thought string `json:"thought"`
	Final   string `json:"final"`
	Action  struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"action"`
}

// parseLLMJSON 从模型输出中提取第一个平衡的 JSON 对象并解析
func parseLLMJSON(text string) (*llmAction, error) {
	obj, ok := extractJSONObject(text)
	if !ok {
		return nil, fmt.Errorf("输出中没有完整的 JSON 对象")
	}
	var act llmAction
	if err := json.Unmarshal([]byte(obj), &act); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	return &act, nil
}

// extractJSONObject 截取第一个括号平衡的 JSON 对象（容忍模型输出前后的解释文字）
func extractJSONObject(text string) (string, bool) {
	start := strings.Index(text, "{")
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if esc {
			esc = false
			continue
		}
		switch {
		case inStr && ch == '\\':
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return text[start : i+1], true
			}
		}
	}
	return "", false
}

// decodeJSONInto 提取 JSON 对象并解析到任意结构（各架构模式的协议输出共用）
func decodeJSONInto(text string, v any) error {
	obj, ok := extractJSONObject(text)
	if !ok {
		return fmt.Errorf("输出中没有完整的 JSON 对象")
	}
	if err := json.Unmarshal([]byte(obj), v); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	return nil
}

// —— 执行图 ——

// runGraph 执行 Agent：按 GraphType 分派执行图
func (u *UseCase) runGraph(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	// 把任务身份塞进 context：llm.Manager 记录调用历史时取出，
	// 每次 LLM 调用都能关联到 task_id / agent_key（子任务在此覆盖父任务的元数据）；
	// history.WithTask 同理：任务内的 MCP 调用 / 技能使用 / 各类召回自动关联本任务
	ctx = llm.WithMeta(ctx, llm.CallMeta{
		TaskID:   rec.task.ID,
		AgentKey: def.AgentKey,
		Purpose:  "agentfactory." + graphPurpose(def.GraphType),
	})
	ctx = history.WithTask(ctx, rec.task.ID, def.AgentKey)
	// 运行预算：根任务按配置领取一份，子任务沿用父任务的（Ensure 幂等）——
	// 于是一个任务树的全部 LLM 调用共享同一份额度，而不是每个子任务各领一份。
	ctx = budget.Ensure(ctx, u.opt.BudgetTokens, u.opt.BudgetCalls)
	// 任务来自会话编排时回填会话身份：worker ctx 派生自 Background 从头没有 conv，
	// 若不回填，任务内的 MCP / 记忆 / RAG 落 op_histories 时 conv_id 为空，
	// 会话链路时间线看不到这些调用。WithTask 已改为读改写，此处顺序不再敏感，但先 Task 后 Conv 最稳。
	if rec.task.ConvID != "" {
		ctx = history.WithConv(ctx, rec.task.ConvID)
	}
	// 自动编排：graph_type=auto、多 Agent 图缺成员、或开了 auto_compose 时，
	// 先让 LLM 判定架构与成员并落库（每任务一次，结果记 orchestrate 事件）。
	if composed := u.ensureOrchestration(ctx, def, input, rec); composed != nil && composed != def {
		def = composed
	}
	// 编排图优先：flow 里有节点时按 DAG 执行（串并行可混排 + 条件分支）
	if g := ParseFlowGraph(def.Flow); g != nil && len(g.Nodes) > 1 {
		return u.runFlowGraph(ctx, def, g, input, rec)
	}
	// 显式编排（串行/并行）：成员任务 + 汇总 + 监察
	if (def.Orchestration == OrchestrationSerial || def.Orchestration == OrchestrationParallel) && len(def.Members) > 0 {
		return u.runWorkflow(ctx, def, input, rec)
	}
	switch def.GraphType {
	case GraphSimple:
		return u.runSimple(ctx, def, input, rec)
	case GraphReact, "", GraphAuto: // auto 已在 ensureOrchestration 里解析成具体图，这里只是兜底
		return u.runReact(ctx, def, input, rec)
	case GraphPlanExecute:
		return u.runPlanExecute(ctx, def, input, rec)
	case GraphReflexion:
		return u.runReflexion(ctx, def, input, rec)
	case GraphRouter:
		return u.runRouter(ctx, def, input, rec)
	case GraphSelfAsk:
		return u.runSelfAsk(ctx, def, input, rec)
	case GraphFuncCall:
		return u.runFunctionCall(ctx, def, input, rec)
	case GraphStateMachine:
		return u.runStateMachine(ctx, def, input, rec)
	case GraphOrchestrator:
		return u.runOrchestrator(ctx, def, input, rec, false)
	case GraphHierarchical:
		return u.runOrchestrator(ctx, def, input, rec, true)
	case GraphDebate:
		return u.runDebate(ctx, def, input, rec)
	case GraphBlackboard:
		return u.runBlackboard(ctx, def, input, rec)
	default:
		return nil, fmt.Errorf("%w: 未知执行图 %q", ErrInvalidDef, def.GraphType)
	}
}

// graphPurpose LLM 调用记录的用途标记（等于执行图名，便于按架构统计模型开销）
func graphPurpose(gt string) string {
	if gt == "" {
		return GraphReact
	}
	return gt
}

// runSimple 单轮直答
func (u *UseCase) runSimple(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	system := def.System
	if system == "" {
		system = "你是 Agent「" + def.Name + "」。" + def.Description
	}
	answer, err := u.chatWith(ctx, def, system, input)
	if err != nil {
		return nil, err
	}
	rec.turn = 1
	rec.event(EventFinal, 1, map[string]any{"answer": answer})
	return &RunResult{Answer: answer, Turns: 1}, nil
}

// chatWith 按定义选数据源与模型：
// LLMSource 非空时优先用该数据源（不可用/调用失败由 Manager 回退 default 链路），
// LLMModel 非空时在提示词头部声明期望模型。
func (u *UseCase) chatWith(ctx context.Context, def *AgentDef, system, user string) (string, error) {
	return u.chatWithOpts(ctx, def, system, user, 0.3, false)
}

// chatWithOpts 带生成参数的 chatWith：temperature 显式传入（规划 0.2 / 执行 0.3 /
// 抽取 0）；jsonMode 走 response_format json_object 强制 JSON 输出，省掉
// 「输出不合规 → 喂回自纠」的整轮重试（数据源不支持时由 Manager 自动降级）。
func (u *UseCase) chatWithOpts(ctx context.Context, def *AgentDef, system, user string, temperature float64, jsonMode bool) (string, error) {
	if u.chat == nil {
		return "", ErrNoChatter
	}
	// 预算闸门：宁可提前收敛，也不要「明知超额还发一次请求」
	if err := budget.From(ctx).Check(); err != nil {
		return "", err
	}
	if def.LLMModel != "" {
		system = "[期望模型: " + def.LLMModel + "]\n" + system
	}
	return u.chat.ChatOpts(ctx, llm.ChatOptions{
		System:      system,
		User:        user,
		Source:      def.LLMSource,
		Temperature: temperature,
		JSONMode:    jsonMode,
	})
}

// runReact ReAct 循环
func (u *UseCase) runReact(ctx context.Context, def *AgentDef, input string, rec *Recorder) (*RunResult, error) {
	system, err := u.buildSystemPrompt(ctx, def)
	if err != nil {
		return nil, err
	}
	return u.runToolLoop(ctx, def, system, "## 任务\n"+input+"\n\n## 执行记录\n", u.turnBudget(def, 12), rec)
}

// turnBudget 轮数预算：未配置时用兜底值
func (u *UseCase) turnBudget(def *AgentDef, fallback int) int {
	if def.MaxTurns > 0 {
		return def.MaxTurns
	}
	return fallback
}

// runToolLoop 通用「LLM 输出协议 → 调能力 → 观察回填」循环：
// ReAct 与各架构模式的执行步骤都复用它，保证工具调用、错误自纠、轮数收敛行为一致。
// 工具循环的上下文管理参数：
//   - keepRecentTurns  最近 N 轮保留全文，更早的轮次压缩成一行摘要（滚动摘要，
//     把 O(n²) 的 transcript 重发压成近似线性）
//   - digestCap        观察摘要截断长度（摘要只保留「做了什么」而非全文）
//   - transcriptBudget 发送给 LLM 的 user 消息字符预算（rune 计），超过强制收敛
const (
	keepRecentTurns  = 3
	digestCap        = 160
	transcriptBudget = 120_000
)

func (u *UseCase) runToolLoop(ctx context.Context, def *AgentDef, system, seed string, maxTurns int, rec *Recorder) (*RunResult, error) {
	if maxTurns <= 0 {
		maxTurns = 8
	}
	var digest []string // 早期轮次的一句话摘要
	var recent []string // 最近 keepRecentTurns 轮的全文段
	seen := map[string]string{}
	promptChars := utf8.RuneCountInString(system) + utf8.RuneCountInString(seed)

	progressStep := 90 / maxTurns
	invalidCount := 0 // 模型输出不合规的连续次数，超过 2 次直接失败

	call := func(user string) (string, error) {
		promptChars += utf8.RuneCountInString(user)
		return u.chatWithOpts(ctx, def, system, user, 0.3, true)
	}

	for turn := 1; turn <= maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("执行被取消: %w", err)
		}
		// 字符预算熔断：transcript 过长说明任务发散，强制收敛而不是继续烧 token
		if promptChars > transcriptBudget {
			rec.event(EventError, rec.turn+1, map[string]any{"message": "上下文长度超预算，强制收敛"})
			break
		}
		rec.turn++
		t := rec.turn
		rec.setProgress(5 + turn*progressStep)

		output, err := call(composeTranscript(seed, digest, recent))
		if err != nil {
			// 预算耗尽不是故障：把已有中间结论收敛成答案返回，任务算成功
			// （否则用户拿到的是一条「失败」，而钱已经花掉了）
			if errors.Is(err, budget.ErrExceeded) {
				snap := budget.From(ctx).Snapshot()
				rec.event(EventError, t, map[string]any{
					"message":         "token 预算耗尽，提前收敛",
					"budget_exceeded": true,
					"budget":          snap,
				})
				return &RunResult{Answer: convergeAnswer(digest, recent), Turns: turn, BudgetExceeded: true}, nil
			}
			return nil, fmt.Errorf("第 %d 轮 LLM 调用失败: %w", turn, err)
		}
		act, perr := parseLLMJSON(output)
		if perr != nil {
			// 模型输出不合规：喂回错误让它自纠，连续三次直接失败
			// （已开 JSON 模式，正常情况下极少触发；仅兜底老网关降级后的场景）
			rec.event(EventThought, t, map[string]any{"thought": truncateRunes(output, 500), "parse_error": perr.Error()})
			recent = append(recent, fmt.Sprintf("\n[第 %d 轮] 输出不合规：%v。请严格按输出协议重新输出 JSON。\n", turn, perr))
			invalidCount++
			if invalidCount >= 3 {
				return nil, fmt.Errorf("模型连续 %d 轮输出不合规，终止执行", invalidCount)
			}
			continue
		}
		invalidCount = 0
		if act.Thought != "" {
			rec.event(EventThought, t, map[string]any{"thought": act.Thought})
		}

		// 最终答案
		if strings.TrimSpace(act.Final) != "" {
			rec.event(EventFinal, t, map[string]any{"answer": act.Final})
			return &RunResult{Answer: act.Final, Turns: turn}, nil
		}

		// 行动
		name := strings.TrimSpace(act.Action.Name)
		if name == "" {
			recent = append(recent, "\n[本轮] 未指定 action.name，请输出 final 或合法 action。\n")
			continue
		}
		args := act.Action.Args
		if args == nil {
			args = map[string]any{}
		}
		rec.event(EventToolCall, t, map[string]any{"name": name, "args": args})

		// 同任务内去重：完全相同的能力 + 参数直接复用结果，提示模型别重复调用
		var section string
		key := name + "|" + argsKey(args)
		if prevTurn, ok := seen[key]; ok {
			rec.event(EventToolResult, t, map[string]any{"name": name, "result": "(与先前轮次相同调用，已复用)", "is_error": false})
			section = fmt.Sprintf("\n[第 %d 轮] 调用 %s\n观察: 与第 %s 轮的调用参数完全相同，结果不变。不要重复相同调用，请基于已有信息输出 final 或换参数。\n", turn, name, prevTurn)
		} else {
			observation, callErr := u.InvokeDispatch(ctx, name, args, rec)
			if callErr != nil {
				observation = "[能力调用失败] " + callErr.Error()
			}
			rec.event(EventToolResult, t, map[string]any{"name": name, "result": truncateRunes(observation, 4000), "is_error": callErr != nil})
			seen[key] = fmt.Sprintf("第 %d 轮", turn)
			// 结构化标记包裹观察：模型定位信息更快，跨轮引用更准确
			section = fmt.Sprintf("\n[第 %d 轮] 调用 %s\n<observation tool=%q turn=%d>\n%s\n</observation>\n", turn, name, name, turn, truncateRunes(observation, 4000))
		}
		recent = append(recent, section)
		// 滚动摘要：只保留最近 keepRecentTurns 轮全文
		if len(recent) > keepRecentTurns {
			digest = append(digest, summarizeSection(recent[0]))
			recent = recent[1:]
		}
	}

	// 轮数耗尽（或预算熔断）：强制让模型收敛一次
	rec.turn++
	t := rec.turn
	transcript := composeTranscript(seed, digest, recent) + "\n已达最大轮数，请立即基于以上信息输出 final。\n"
	output, err := call(transcript)
	if err != nil {
		return nil, fmt.Errorf("收敛轮 LLM 调用失败: %w", err)
	}
	if act, perr := parseLLMJSON(output); perr == nil && strings.TrimSpace(act.Final) != "" {
		rec.event(EventFinal, t, map[string]any{"answer": act.Final})
		return &RunResult{Answer: act.Final, Turns: maxTurns + 1}, nil
	}
	return nil, fmt.Errorf("执行 %d 轮后未得到最终答案", maxTurns)
}

// composeTranscript 组装 user 消息：任务种子 + 早期轮次摘要 + 最近轮次全文。
func composeTranscript(seed string, digest, recent []string) string {
	var b strings.Builder
	b.WriteString(seed)
	if len(digest) > 0 {
		b.WriteString("\n## 早期轮次摘要\n")
		for _, d := range digest {
			b.WriteString("- ")
			b.WriteString(d)
			b.WriteString("\n")
		}
		b.WriteString("\n## 最近执行记录\n")
	}
	for _, r := range recent {
		b.WriteString(r)
	}
	return b.String()
}

// summarizeSection 规则摘要（不再调 LLM——那会反过来烧 token）：
// 保留「第几轮调了什么」+ 观察开头一小段。
func summarizeSection(section string) string {
	s := strings.TrimSpace(section)
	if idx := strings.Index(s, "<observation"); idx >= 0 {
		head := strings.TrimSpace(s[:idx]) // [第 N 轮] 调用 X
		rest := strings.TrimSpace(s[idx:]) // <observation ...>...</observation>
		rest = strings.TrimPrefix(rest, "<observation")
		if close := strings.Index(rest, ">"); close >= 0 { // 吃掉属性段
			rest = strings.TrimSpace(rest[close+1:])
		}
		rest = strings.TrimSuffix(rest, "</observation>")
		return truncateRunes(head, 80) + " → " + truncateRunes(strings.TrimSpace(rest), digestCap)
	}
	return truncateRunes(s, digestCap)
}

// argsKey 能力调用去重键：能力名 + 参数 JSON（marshal 失败退化为 fmt）
func argsKey(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprint(args)
	}
	return string(b)
}

// runChildTask 委派子 Agent：建子任务 → 内联执行 → 记链路。
// 返回子任务与最终答案（多 Agent 协作的链路树在此形成）。
func (u *UseCase) runChildTask(ctx context.Context, parent *Task, agentKey, input string, rec *Recorder) (*Task, string, error) {
	def, err := u.loadDefByKey(ctx, agentKey)
	if err != nil {
		return nil, "", fmt.Errorf("委派目标 %q 不存在或已停用: %w", agentKey, err)
	}
	if !def.Enabled {
		return nil, "", fmt.Errorf("委派目标 %q 已停用", agentKey)
	}

	child := &Task{
		AgentDefID:   def.ID,
		AgentKey:     def.AgentKey,
		Title:        truncateRunes(input, 80),
		Input:        input,
		Status:       TaskPending,
		ParentTaskID: parent.ID,
		Depth:        parent.Depth + 1,
		ConvID:       parent.ConvID, // 会话编排链路：子任务继承来源会话，保证任务树整层可按会话追溯
		UserID:       parent.UserID, // 子任务继承父任务归属，避免委派后链路被拆到不同用户
	}
	if err := u.repo.CreateTask(ctx, child); err != nil {
		return nil, "", fmt.Errorf("创建子任务失败: %w", err)
	}
	rec.event(EventDelegate, rec.turn, map[string]any{"agent": agentKey, "child_task_id": child.ID, "input": truncateRunes(input, 500)})

	answer, err := u.executeTaskNow(ctx, child, def)
	if err != nil {
		return child, "", err
	}
	return child, answer, nil
}

// convergeAnswer 预算耗尽时的收敛答案：取最近一条中间结论作为部分结果。
// 优先用 digest（早期轮次的一句话摘要），没有再退到 recent（最近轮次全文）。
func convergeAnswer(digest, recent []string) string {
	tail := ""
	for i := len(digest) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(digest[i]); v != "" {
			tail = v
			break
		}
	}
	if tail == "" {
		for i := len(recent) - 1; i >= 0; i-- {
			if v := strings.TrimSpace(recent[i]); v != "" {
				tail = v
				break
			}
		}
	}
	head := "已用尽本轮执行预算（budget.maxTokens / maxCalls），以下是目前收集到的中间结论；" +
		"任务未全部完成，可提高配额后重跑。"
	if tail == "" {
		return head + "\n（暂无可用的中间结论）"
	}
	return head + "\n\n" + truncateRunes(tail, 1200)
}

// parallelLimit 并行波次上限：未配置时用 defaultMaxParallel。
func (u *UseCase) parallelLimit() int {
	if u.opt.MaxParallel > 0 {
		return u.opt.MaxParallel
	}
	return defaultMaxParallel
}
