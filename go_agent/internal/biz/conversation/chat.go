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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/history"
	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/memory"
	"go_agent/internal/biz/rag"
	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/mediaauth"
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

	// —— 召回质量与延迟 ——
	recallTimeout   = 8 * time.Second // 单个召回源超时：慢源不拖垮整轮对话
	chatMemMinScore = 0.15            // 记忆绝对分数线（相似度 0~1）：低于此值视为噪声，
	//                                   注入只会稀释模型注意力、拉长 prompt（实测弱相关常在 0.03 量级）。
	chatRagRelKeep = 0.5 // RAG 相对分数线：RRF 融合分跨次召回不可比（绝对阈值无意义），
	//                        只保留 ≥ 同次最高分 × 0.5 的命中，砍掉长尾噪声。
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
// atts 为本轮附件（先经 SaveMedia 上传；图片会以视觉方式交给 LLM，视频仅作文字说明）。
// 返回两条消息；LLM/工具失败时不返回 error，而是落一条失败的 assistant 消息（meta.ok=false）。
func (u *UseCase) Chat(ctx context.Context, id, content string, atts []Attachment) (*Message, *Message, error) {
	content = strings.TrimSpace(content)
	if content == "" && len(atts) == 0 {
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

	userMsg, err := u.AppendWithAttachments(ctx, conv.ID, RoleUser, content, "", 0, atts)
	if err != nil {
		return nil, nil, err
	}

	answer, meta := u.orchestrate(ctx, conv.ID, content, atts)
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
func (u *UseCase) orchestrate(ctx context.Context, convID, content string, atts []Attachment) (string, map[string]any) {
	meta := map[string]any{"op": "chat"}
	if n := attachmentMeta(atts); n != nil {
		meta["attachments"] = n
	}

	// 1. 自动召回上下文（best-effort）
	blocks, rmeta := u.recallContext(ctx, content)
	for k, v := range rmeta {
		meta[k] = v
	}
	if block := mediaBlock(atts); block != "" {
		blocks = append(blocks, block)
	}

	// 2. 工具目录 + 最近对话记录
	agents, mcps := u.toolCatalog(ctx)
	if len(agents)+len(mcps) == 0 {
		// 没有任何工具：单轮直答
		out, err := u.chatLLM(ctx, u.chatSystem("", blocks), u.historyBlock(ctx, convID, content), atts)
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

		out, err := u.chatLLM(ctx, system, userPayload, atts)
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
		out, err := u.chatLLM(ctx, system, userPayload, atts)
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

// recallContext 自动召回：长期记忆 + RAG 检索 + 图谱召回，全部 best-effort。
// 三路**并发**执行（此前为串行，最慢源决定总耗时），每路独立 8s 超时；
// 结果块按下标固定顺序拼接（记忆 → 知识库 → 图谱），保证 prompt 结构稳定。
func (u *UseCase) recallContext(ctx context.Context, content string) (blocks []string, meta map[string]any) {
	meta = map[string]any{}
	if u.deps == nil {
		return nil, meta
	}

	parts := make([]string, 3) // 0 记忆 / 1 知识库 / 2 图谱
	metas := make([]map[string]any, 3)
	var wg sync.WaitGroup
	run := func(i int, fn func(context.Context) (string, map[string]any)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, recallTimeout)
			defer cancel()
			parts[i], metas[i] = fn(c)
		}()
	}

	if u.deps.Memory != nil {
		run(0, func(c context.Context) (string, map[string]any) { return u.recallMemory(c, content) })
	}
	if u.deps.Rag != nil {
		run(1, func(c context.Context) (string, map[string]any) { return u.recallRag(c, content) })
		run(2, func(c context.Context) (string, map[string]any) { return u.recallGraph(c, content) })
	}
	wg.Wait()

	for i, p := range parts {
		if p == "" {
			continue
		}
		blocks = append(blocks, p)
		for k, v := range metas[i] {
			meta[k] = v
		}
	}
	return blocks, meta
}

// recallMemory 长期记忆（含记忆图谱）：绝对分数线过滤弱相关噪声
func (u *UseCase) recallMemory(ctx context.Context, content string) (string, map[string]any) {
	meta := map[string]any{}
	// 召回命名空间 = 当前请求用户（而不是写死的 "default"，否则所有用户的
	// 长期记忆会串在同一个命名空间里 —— 数据串号级事故）。
	res, err := u.deps.Memory.Recall(ctx, content, identity.UserID(ctx), chatMemTopK, true, memory.RecallSourceChat)
	if err != nil {
		meta["mem_error"] = err.Error()
		return "", meta
	}
	kept := filterMemories(res.Memories, chatMemMinScore)
	if filtered := len(res.Memories) - len(kept); filtered > 0 {
		meta["memories_filtered"] = filtered
	}
	if lines := memoryLinesOf(kept); len(lines) > 0 {
		meta["memories"] = len(lines)
		meta["memories_list"] = lines
	}
	if res.Graph != nil {
		meta["mem_graph_entities"] = len(res.Graph.Entities)
		meta["mem_graph_relations"] = len(res.Graph.Relations)
		meta["mem_graph_entities_list"] = entitySummaries(res.Graph.Entities)
		meta["mem_graph_relations_list"] = relationSummaries(res.Graph.Relations)
	}
	// 图谱块与记忆块合并输出（都归属「长期记忆」来源）
	var b strings.Builder
	if lines := memoryLinesOf(kept); len(lines) > 0 {
		b.WriteString("### 长期记忆\n" + strings.Join(lines, "\n"))
	}
	return b.String(), meta
}

// recallRag 知识库混合检索：RRF 相对分数线过滤长尾
func (u *UseCase) recallRag(ctx context.Context, content string) (string, map[string]any) {
	meta := map[string]any{}
	// 按当前用户隔离检索：只召回本人文档的分块（认证未启用时 = default 作用域）
	hits, err := u.deps.Rag.RetrieveForUser(ctx, content, identity.UserID(ctx))
	if err != nil {
		meta["rag_error"] = err.Error()
		return "", meta
	}
	kept := filterHits(hits, chatRagRelKeep)
	if filtered := len(hits) - len(kept); filtered > 0 {
		meta["rag_filtered"] = filtered
	}
	if len(kept) > chatRagTopK {
		kept = kept[:chatRagTopK]
	}
	if len(kept) == 0 {
		return "", meta
	}
	lines := make([]string, 0, len(kept))
	for _, h := range kept {
		lines = append(lines, fmt.Sprintf("- [%.3f] %s（来源 %s）", h.Score, clipRunes(h.Text, 400), h.Source))
	}
	meta["rag_hits"] = len(kept)
	meta["rag_hits_list"] = ragHitSummaries(kept, chatRagTopK)
	return "### 知识库检索\n" + strings.Join(lines, "\n"), meta
}

// recallGraph 图谱召回（只检索不生成）
func (u *UseCase) recallGraph(ctx context.Context, content string) (string, map[string]any) {
	meta := map[string]any{}
	g, err := u.deps.Rag.GraphRecallForUser(ctx, content, identity.UserID(ctx))
	if err != nil {
		meta["graph_error"] = err.Error()
		return "", meta
	}
	lines := graphLines(g)
	if len(lines) == 0 {
		return "", meta
	}
	meta["graph_entities"] = len(g.Entities)
	meta["graph_relations"] = len(g.Relations)
	meta["graph_entities_list"] = entitySummaries(g.Entities)
	meta["graph_relations_list"] = relationSummaries(g.Relations)
	return "### 图谱上下文\n" + strings.Join(lines, "\n"), meta
}

// filterMemories 记忆过滤：相似度 < minScore 的视为噪声丢弃（保持原有降序）。
func filterMemories(mems []memory.RecalledMemory, minScore float64) []memory.RecalledMemory {
	out := make([]memory.RecalledMemory, 0, len(mems))
	for _, m := range mems {
		if float64(m.Score) >= minScore {
			out = append(out, m)
		}
	}
	return out
}

// filterHits RRF 命中过滤：只保留 ≥ 同次最高分 × relKeep 的条目。
// RRF 分数（≈Σ1/(k+rank)）只在同一次召回内可比，不能用固定绝对阈值。
func filterHits(hits []rag.Hit, relKeep float64) []rag.Hit {
	if len(hits) == 0 {
		return nil
	}
	top := float32(0)
	for _, h := range hits {
		if h.Score > top {
			top = h.Score
		}
	}
	threshold := top * float32(relKeep)
	out := make([]rag.Hit, 0, len(hits))
	for _, h := range hits {
		if h.Score >= threshold {
			out = append(out, h)
		}
	}
	return out
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
			line := clipRunes(m.Content, chatHistoryClip)
			if tag := attachmentTag(m.Attachments); tag != "" {
				line = strings.TrimSpace(line + " " + tag)
			}
			fmt.Fprintf(&b, "%s：%s\n", role, line)
		}
	}
	return b.String()
}

// —— 小工具 ——

func memoryLinesOf(mems []memory.RecalledMemory) []string {
	lines := make([]string, 0, len(mems))
	for _, m := range mems {
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

// —— 附件与多模态 ——

const (
	// chatMaxImages 单轮随消息送给 LLM 的图片上限（控制 token 成本）
	chatMaxImages = 4
)

// chatLLM 带图/纯文本自适应调用：有图片附件走 ChatVision（模型不支持时
// Manager 内部自动降级纯文本），否则走普通 Chat。
func (u *UseCase) chatLLM(ctx context.Context, system, user string, atts []Attachment) (string, error) {
	images, err := imageDataURIs(atts)
	if err != nil {
		// 图片读盘失败不阻塞对话：退化为纯文本，附件以文字说明参与
		images = nil
	}
	if len(images) == 0 {
		return u.deps.LLM.Chat(ctx, system, user)
	}
	return u.deps.LLM.ChatVision(ctx, system, user, images)
}

// imageDataURIs 把图片附件读成 data URI（OpenAI 兼容 image_url 格式）。
// 最多取前 chatMaxImages 张；URL 必须是本服务签发的 /api/files/ 地址。
func imageDataURIs(atts []Attachment) ([]string, error) {
	var out []string
	for _, a := range atts {
		if len(out) >= chatMaxImages {
			break
		}
		if !a.IsImage() || strings.TrimSpace(a.URL) == "" {
			continue
		}
		rel, err := mediaDiskRel(a.URL)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(mediaRoot, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		mimeType := a.MIME
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		out = append(out, fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data)))
	}
	return out, nil
}

// mediaDiskRel /api/files/{bucket}/{name} → {bucket}/{name}（拒绝路径穿越）。
// 地址可能带访问签名（?sig=…），先剥掉查询串再映射磁盘路径。
func mediaDiskRel(url string) (string, error) {
	url = mediaauth.PathOnly(url)
	rel := strings.TrimPrefix(url, mediaURLPrefix)
	if rel == url || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return "", fmt.Errorf("非法附件地址 %q", url)
	}
	return rel, nil
}

// mediaBlock 本轮附件的提示词说明块（视频无法解析内容，靠文字说明兜底）
func mediaBlock(atts []Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### 本轮附件（用户随消息上传）")
	for _, a := range atts {
		size := fmt.Sprintf("%.1fMB", float64(a.Size)/(1<<20))
		if a.IsImage() {
			fmt.Fprintf(&b, "\n- 图片 %s（%s, %s）：已随消息提供，若你具备视觉能力请直接查看分析",
				a.Name, a.MIME, size)
		} else {
			fmt.Fprintf(&b, "\n- 视频 %s（%s, %s）：内容无法直接解析，请基于用户文字说明回应，必要时提示上传关键帧",
				a.Name, a.MIME, size)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// attachmentMeta meta 里记录的附件摘要
func attachmentMeta(atts []Attachment) map[string]any {
	if len(atts) == 0 {
		return nil
	}
	images, videos := 0, 0
	for _, a := range atts {
		switch {
		case a.IsImage():
			images++
		case a.IsVideo():
			videos++
		}
	}
	return map[string]any{"total": len(atts), "images": images, "videos": videos}
}

// attachmentTag 历史消息里的附件标注（LLM 需知道此前轮次带过媒体）
func attachmentTag(atts []Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	images, videos, other := 0, 0, 0
	for _, a := range atts {
		switch {
		case a.IsImage():
			images++
		case a.IsVideo():
			videos++
		default:
			other++
		}
	}
	parts := make([]string, 0, 3)
	if images > 0 {
		parts = append(parts, fmt.Sprintf("图片×%d", images))
	}
	if videos > 0 {
		parts = append(parts, fmt.Sprintf("视频×%d", videos))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("文件×%d", other))
	}
	return "[附件：" + strings.Join(parts, "、") + "]"
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
