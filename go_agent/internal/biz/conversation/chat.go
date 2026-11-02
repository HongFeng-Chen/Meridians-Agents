// 会话聊天编排：与 LLM 多轮对话，LLM 自主决定是否调用 Agent / MCP 工具。
//
// 流程（Chat）：
//  1. 追加 user 消息
//  2. 自动召回上下文（长期记忆 / RAG 知识库 / GraphRAG 图谱，全部 best-effort）
//  3. 组装工具目录（启用的 Agent 定义 + 启用的 MCP 工具）与最近对话记录，
//     LLM 循环输出 JSON：要么 {"final"} 直答，要么 {"action"} 调工具拿观察
//  4. 最终回复作为 assistant 消息落库，meta JSON 记录本轮链路
package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/history"
	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/memory"
	"go_agent/internal/biz/rag"
	"go_agent/internal/biz/rag/graphrag"
)

const (
	chatMaxTurns      = 6    // LLM 决策循环最大轮数
	chatMaxHistory    = 20   // 注入提示词的最近消息条数
	chatHistoryClip   = 800  // 每条历史消息截断
	chatObservationCl = 3000 // 工具观察截断
	chatRagTopK       = 5    // RAG 召回条数
	chatMemTopK       = 5    // 记忆召回条数
	chatToolLimit     = 50   // 工具目录上限
	agentWaitTimeout  = 5 * time.Minute
	agentPollInterval = 2 * time.Second
)

// ChatDeps 聊天编排依赖（均为可选，nil 时跳过对应能力）
type ChatDeps struct {
	LLM    *llm.Manager          // 对话模型
	Rag    *rag.Rag              // RAG 检索 + 图谱召回
	Memory *memory.UseCase       // 长期记忆召回
	MCP    *mcp.UseCase          // MCP 工具目录与调用
	Agents *agentfactory.UseCase // Agent 目录与任务执行
}

// chatAction LLM 输出协议
type chatAction struct {
	Thought string `json:"thought"`
	Final   string `json:"final"`
	Action  struct {
		Type string         `json:"type"` // agent | mcp
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"action"`
}

// Chat 一轮完整对话：user 消息入库 → LLM 编排 → assistant 回复入库。
// 返回两条消息；LLM/工具失败时不返回 error，而是落一条失败的 assistant 消息（meta.ok=false）。
func (u *UseCase) Chat(ctx context.Context, id, content string) (*Message, *Message, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil, fmt.Errorf("%w: 消息内容不能为空", ErrInvalidConv)
	}
	if utf8.RuneCountInString(content) > 20000 {
		return nil, nil, fmt.Errorf("%w: 消息过长（≤20000 字）", ErrInvalidConv)
	}
	conv, err := u.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	// 会话身份进 context：本轮对话内的记忆/RAG/GraphRAG 召回、MCP 调用
	// 落 op_histories 时自动带上 conv_id（会话链路时间线数据源）。
	ctx = history.WithConv(ctx, conv.ID)

	userMsg, err := u.Append(ctx, conv.ID, RoleUser, content, "", 0)
	if err != nil {
		return nil, nil, err
	}

	answer, meta := u.orchestrate(ctx, conv.ID, content)
	metaJSON := ""
	if len(meta) > 0 {
		b, _ := json.Marshal(meta)
		metaJSON = string(b)
	}
	assistantMsg, err := u.Append(ctx, conv.ID, RoleAssistant, answer, metaJSON, 0)
	if err != nil {
		return userMsg, nil, err
	}
	return userMsg, assistantMsg, nil
}

// orchestrate 上下文召回 + LLM 工具循环，返回最终回复与链路 meta
func (u *UseCase) orchestrate(ctx context.Context, convID, content string) (string, map[string]any) {
	meta := map[string]any{"op": "chat"}

	// 1. 自动召回上下文（best-effort）
	blocks, rmeta := u.recallContext(ctx, content)
	for k, v := range rmeta {
		meta[k] = v
	}

	// 2. 工具目录 + 最近对话记录
	agents, mcps := u.toolCatalog(ctx)
	if len(agents)+len(mcps) == 0 {
		// 没有任何工具：单轮直答
		out, err := u.deps.LLM.Chat(ctx, u.chatSystem("", blocks), u.historyBlock(ctx, convID, content))
		if err != nil {
			return u.failAnswer(err), map[string]any{"op": "chat", "ok": false, "error": err.Error()}
		}
		meta["turns"] = 1
		return strings.TrimSpace(out), meta
	}

	system := u.chatSystem(u.toolCatalogBlock(agents, mcps), blocks)
	transcript := u.historyBlock(ctx, convID, content)

	var observations []string
	var toolsUsed []string
	final := ""

	for turn := 1; turn <= chatMaxTurns; turn++ {
		userPayload := transcript
		for _, ob := range observations {
			userPayload += "\n## 工具观察\n" + ob
		}
		userPayload += "\n请根据以上信息输出严格 JSON：需要更多信息就调用工具，已足够则输出 final。"

		out, err := u.deps.LLM.Chat(ctx, system, userPayload)
		if err != nil {
			return u.failAnswer(err), metaWithTurn(meta, toolsUsed, turn, true, err.Error())
		}
		act, perr := parseChatJSON(out)
		if perr != nil {
			observations = append(observations, fmt.Sprintf("[第 %d 轮] 输出不合规（%v），请严格输出 JSON 协议。", turn, perr))
			continue
		}
		if strings.TrimSpace(act.Final) != "" {
			final = strings.TrimSpace(act.Final)
			break
		}
		obs, toolLabel, oerr := u.execAction(ctx, convID, act)
		if toolLabel != "" {
			toolsUsed = append(toolsUsed, toolLabel)
		}
		if oerr != nil {
			obs = "[调用失败] " + oerr.Error()
		}
		observations = append(observations, fmt.Sprintf("[第 %d 轮] 调用 %s\n观察: %s", turn, toolLabel, clipRunes(obs, chatObservationCl)))
	}

	if final == "" {
		// 轮数耗尽：让模型基于全部观察收敛一次
		userPayload := transcript
		for _, ob := range observations {
			userPayload += "\n## 工具观察\n" + ob
		}
		userPayload += "\n已达最大轮数，请立即输出 final。"
		out, err := u.deps.LLM.Chat(ctx, system, userPayload)
		if err != nil {
			return u.failAnswer(err), metaWithTurn(meta, toolsUsed, chatMaxTurns, true, err.Error())
		}
		if act, perr := parseChatJSON(out); perr == nil && strings.TrimSpace(act.Final) != "" {
			final = strings.TrimSpace(act.Final)
		} else {
			final = "（本轮未能生成有效回复，请重试）"
		}
	}
	return final, metaWithTurn(meta, toolsUsed, 0, false, "")
}

// recallContext 自动召回：长期记忆 + RAG 检索 + 图谱召回，全部 best-effort
func (u *UseCase) recallContext(ctx context.Context, content string) (blocks []string, meta map[string]any) {
	meta = map[string]any{}
	if u.deps == nil {
		return nil, meta
	}

	// 长期记忆（含记忆图谱上下文）
	if u.deps.Memory != nil {
		if res, err := u.deps.Memory.Recall(ctx, content, "default", chatMemTopK, true, memory.RecallSourceChat); err == nil {
			if lines := memoryLines(res); len(lines) > 0 {
				blocks = append(blocks, "### 长期记忆\n"+strings.Join(lines, "\n"))
				meta["memories"] = len(lines)
				// 明细展开：逐条记忆（已含得分/类别/内容截断）
				meta["memories_list"] = lines
			}
			if res.Graph != nil {
				meta["mem_graph_entities"] = len(res.Graph.Entities)
				meta["mem_graph_relations"] = len(res.Graph.Relations)
				meta["mem_graph_entities_list"] = entitySummaries(res.Graph.Entities)
				meta["mem_graph_relations_list"] = relationSummaries(res.Graph.Relations)
			}
		} else {
			meta["mem_error"] = err.Error()
		}
	}

	if u.deps.Rag != nil {
		// 知识库向量/混合检索
		if hits, err := u.deps.Rag.Retrieve(ctx, content, ""); err == nil && len(hits) > 0 {
			lines := make([]string, 0, len(hits))
			for i, h := range hits {
				if i >= chatRagTopK {
					break
				}
				lines = append(lines, fmt.Sprintf("- [%.3f] %s（来源 %s）", h.Score, clipRunes(h.Text, 400), h.Source))
			}
			blocks = append(blocks, "### 知识库检索\n"+strings.Join(lines, "\n"))
			meta["rag_hits"] = min(len(hits), chatRagTopK)
			// 明细展开：逐条命中（得分 / 来源 / 片段正文截断）
			meta["rag_hits_list"] = ragHitSummaries(hits, chatRagTopK)
		} else if err != nil {
			meta["rag_error"] = err.Error()
		}

		// 图谱召回（只检索不生成）
		if g, err := u.deps.Rag.GraphRecall(ctx, content); err == nil {
			if lines := graphLines(g); len(lines) > 0 {
				blocks = append(blocks, "### 图谱上下文\n"+strings.Join(lines, "\n"))
				meta["graph_entities"] = len(g.Entities)
				meta["graph_relations"] = len(g.Relations)
				meta["graph_entities_list"] = entitySummaries(g.Entities)
				meta["graph_relations_list"] = relationSummaries(g.Relations)
			}
		} else {
			meta["graph_error"] = err.Error()
		}
	}
	return blocks, meta
}

// —— 召回明细展开（会话链路时间线用，写入消息 meta）——

const recallDetailLimit = 20 // 明细列表条数上限

// ragHitSummaries RAG 命中明细：得分 / 来源 / 片段正文（截断）
func ragHitSummaries(hits []rag.Hit, k int) []map[string]any {
	out := make([]map[string]any, 0, min(len(hits), k))
	for i, h := range hits {
		if i >= k {
			break
		}
		out = append(out, map[string]any{
			"score":  h.Score,
			"source": h.Source,
			"doc_id": h.DocID,
			"text":   clipRunes(h.Text, 240),
		})
	}
	return out
}

// entitySummaries 图谱实体明细：名称 / 类型 / 描述（截断）
func entitySummaries(ents []graphrag.Entity) []map[string]any {
	out := make([]map[string]any, 0, min(len(ents), recallDetailLimit))
	for i, e := range ents {
		if i >= recallDetailLimit {
			break
		}
		out = append(out, map[string]any{
			"name": e.Name,
			"type": e.Type,
			"desc": clipRunes(e.Description, 160),
		})
	}
	return out
}

// relationSummaries 图谱关系明细：「头 -关系-> 尾」行
func relationSummaries(rels []graphrag.Relation) []string {
	out := make([]string, 0, min(len(rels), recallDetailLimit))
	for i, r := range rels {
		if i >= recallDetailLimit {
			break
		}
		out = append(out, fmt.Sprintf("%s —[%s]→ %s", r.Source, r.Relation, r.Target))
	}
	return out
}

// toolCatalog 可用 Agent 与 MCP 工具（best-effort）
func (u *UseCase) toolCatalog(ctx context.Context) ([]agentfactory.AgentDef, []mcp.Tool) {
	if u.deps == nil {
		return nil, nil
	}
	enabled := true
	if u.deps.Agents != nil {
		if defs, err := u.deps.Agents.ListDefs(ctx, agentfactory.DefFilter{Enabled: &enabled, Limit: chatToolLimit}); err == nil {
			return defs, nil
		}
	}
	if u.deps.MCP != nil {
		if tools, err := u.deps.MCP.ListTools(ctx, mcp.ToolFilter{Enabled: &enabled, Limit: chatToolLimit}); err == nil {
			return nil, tools
		}
	}
	return nil, nil
}

// execAction 执行一次工具调用（Agent 委派 / MCP 调用）。
// convID 写入任务的来源会话：任务链路（任务中心）与会话链路共用同一编号。
func (u *UseCase) execAction(ctx context.Context, convID string, act *chatAction) (observation, toolLabel string, err error) {
	typ := strings.ToLower(strings.TrimSpace(act.Action.Type))
	name := strings.TrimSpace(act.Action.Name)
	args := act.Action.Args
	if args == nil {
		args = map[string]any{}
	}
	switch typ {
	case "agent":
		if u.deps == nil || u.deps.Agents == nil {
			return "", "", fmt.Errorf("Agent 执行环境未配置")
		}
		input := agentInput(args)
		t, err := u.deps.Agents.SubmitTask(ctx, agentfactory.TaskInput{AgentKey: name, Input: input, ConvID: convID})
		if err != nil {
			return "", "agent:" + name, err
		}
		deadline := time.Now().Add(agentWaitTimeout)
		for {
			cur, err := u.deps.Agents.GetTask(ctx, t.ID)
			if err != nil {
				return "", "agent:" + name, err
			}
			switch cur.Status {
			case "succeeded":
				return cur.Result, fmt.Sprintf("agent:%s(#%d)", name, t.ID), nil
			case "failed", "canceled":
				return "", fmt.Sprintf("agent:%s(#%d)", name, t.ID), fmt.Errorf("任务 #%d %s: %s", t.ID, cur.Status, cur.ErrorMsg)
			}
			if time.Now().After(deadline) {
				return "", fmt.Sprintf("agent:%s(#%d)", name, t.ID), fmt.Errorf("任务 #%d 执行超时", t.ID)
			}
			select {
			case <-ctx.Done():
				return "", fmt.Sprintf("agent:%s(#%d)", name, t.ID), ctx.Err()
			case <-time.After(agentPollInterval):
			}
		}
	case "mcp":
		if u.deps == nil || u.deps.MCP == nil {
			return "", "", fmt.Errorf("MCP 执行环境未配置")
		}
		res, err := u.deps.MCP.CallTool(ctx, 0, name, args)
		if err != nil {
			return "", "mcp:" + name, err
		}
		text := res.Structured
		if text == "" {
			parts := make([]string, 0, len(res.Content))
			for _, c := range res.Content {
				if c.Text != "" {
					parts = append(parts, c.Text)
				} else {
					b, _ := json.Marshal(c)
					parts = append(parts, string(b))
				}
			}
			text = strings.Join(parts, "\n")
		}
		if res.IsError {
			return text, "mcp:" + name, fmt.Errorf("工具返回错误: %s", clipRunes(text, 200))
		}
		return text, "mcp:" + name, nil
	default:
		return "", "", fmt.Errorf("未知工具类型 %q（仅支持 agent / mcp）", act.Action.Type)
	}
}

// —— 提示词组装 ——

// toolCatalogBlock 工具目录提示词块（Agent + MCP）
func (u *UseCase) toolCatalogBlock(agents []agentfactory.AgentDef, tools []mcp.Tool) string {
	var b strings.Builder
	if len(agents) > 0 {
		b.WriteString("### Agent（委派一个专责 Agent 执行任务，type=agent，name 填 agent_key）\n")
		for _, a := range agents {
			desc := clipRunes(a.Description, 120)
			if desc == "" {
				desc = clipRunes(a.System, 120)
			}
			fmt.Fprintf(&b, "- %s（%s）：%s\n", a.Name, a.AgentKey, desc)
		}
	}
	if len(tools) > 0 {
		b.WriteString("### MCP 工具（type=mcp，name 填 unique_name）\n")
		for _, t := range tools {
			fmt.Fprintf(&b, "- %s（%s）：%s\n", t.DisplayName, t.UniqueName, clipRunes(t.Description, 120))
		}
	}
	return strings.TrimSpace(b.String())
}

func (u *UseCase) chatSystem(toolBlock string, ctxBlocks []string) string {
	var b strings.Builder
	b.WriteString("你是智能平台的会话助手，与用户多轮对话。")
	b.WriteString("\n回答规则：\n")
	b.WriteString("1. 优先使用「已召回的上下文」回答；\n")
	b.WriteString("2. 上下文不足且可用工具能补齐时，调用工具获取；能直接回答就直接回答，不要为了调用而调用；\n")
	b.WriteString("3. 始终用中文回复。\n")
	if toolBlock != "" {
		b.WriteString("\n## 可用工具\n")
		b.WriteString(toolBlock)
		b.WriteString("\n调用协议：输出严格 JSON（不要输出其他文字）：\n")
		b.WriteString(`{"thought":"简短说明为什么调用","action":{"type":"agent或mcp","name":"工具名","args":{...}}}` + "\n")
		b.WriteString(`已有足够信息时输出：{"final":"给用户的最终回复"}` + "\n")
		b.WriteString("Agent 的 args 用 {\"input\":\"要委派的任务描述\"}；MCP 工具的 args 按其参数 schema 给出。\n")
	}
	if len(ctxBlocks) > 0 {
		b.WriteString("\n## 已召回的上下文（系统自动检索，可直接引用）\n")
		b.WriteString(strings.Join(ctxBlocks, "\n\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// historyBlock 最近对话记录 + 本轮输入
func (u *UseCase) historyBlock(ctx context.Context, convID, content string) string {
	msgs, err := u.repo.ListMessages(ctx, convID)
	var b strings.Builder
	b.WriteString("## 对话记录（最近）\n")
	if err == nil {
		start := 0
		if len(msgs) > chatMaxHistory {
			start = len(msgs) - chatMaxHistory
		}
		for _, m := range msgs[start:] {
			role := "用户"
			if m.Role == RoleAssistant {
				role = "助手"
			}
			fmt.Fprintf(&b, "%s：%s\n", role, clipRunes(m.Content, chatHistoryClip))
		}
	}
	return b.String()
}

// —— 小工具 ——

func memoryLines(res *memory.RecallResult) []string {
	if res == nil {
		return nil
	}
	lines := make([]string, 0, len(res.Memories))
	for _, m := range res.Memories {
		if m.LongTermMemory == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("- [%.3f][%s] %s", m.Score, m.Kind, clipRunes(m.Content, 300)))
	}
	return lines
}

func graphLines(g *graphrag.GraphAnswer) []string {
	if g == nil || (len(g.Entities) == 0 && len(g.Relations) == 0) {
		return nil
	}
	lines := make([]string, 0, len(g.Entities)+len(g.Relations))
	for i, e := range g.Entities {
		if i >= 8 {
			break
		}
		desc := clipRunes(e.Description, 120)
		if desc != "" {
			desc = "：" + desc
		}
		lines = append(lines, fmt.Sprintf("- 实体 %s%s", e.Name, desc))
	}
	for i, r := range g.Relations {
		if i >= 12 {
			break
		}
		lines = append(lines, fmt.Sprintf("- %s -[%s]-> %s", r.Source, r.Relation, r.Target))
	}
	return lines
}

func agentInput(args map[string]any) string {
	if v, ok := args["input"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := args["task"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	b, _ := json.Marshal(args)
	return string(b)
}

func metaWithTurn(meta map[string]any, tools []string, turns int, failed bool, errMsg string) map[string]any {
	if len(tools) > 0 {
		meta["tools"] = tools
	}
	meta["turns"] = turns
	if failed {
		meta["ok"] = false
		meta["error"] = errMsg
	}
	return meta
}

func (u *UseCase) failAnswer(err error) string {
	return "本轮执行失败：" + err.Error()
}

func clipRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// parseChatJSON 从模型输出中提取第一个平衡的 JSON 对象并解析
func parseChatJSON(text string) (*chatAction, error) {
	raw, err := extractJSON(text)
	if err != nil {
		return nil, err
	}
	var act chatAction
	if err := json.Unmarshal([]byte(raw), &act); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	return &act, nil
}
