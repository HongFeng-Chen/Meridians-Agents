// Package platformmcp 把平台自身的「任务中心 / 会话 / 链路数据」以 MCP 工具的形式暴露出去，
// 让 Agent Factory 里的 Agent 用能力白名单（mcp:platform__xxx）调用。
//
// 设计要点：
//   - 只读：不提供提交任务、删除会话等写操作，避免 Agent 自我放大；
//   - 直接复用 biz 层用例（agentfactory / conversation / history），不新写 SQL；
//   - 输出统一裁剪：列表条数、字段长度都有上限，防止把上下文撑爆；
//   - 协议层（JSON-RPC）在 internal/server/mcp.go，本包只负责「工具语义」。
package platformmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/conversation"
	"go_agent/internal/biz/history"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/pkg/identity"
)

// 内置服务标识：MCP 服务名与命名空间都用 platform，
// 工具唯一名即 platform__<tool>，能力名为 mcp:platform__<tool>。
const (
	ServerName = mcp.InternalServerName
	Namespace  = "platform"
)

// 工具名常量（也是 MCP 协议里 tools/list 返回的 name）
const (
	ToolListTasks         = "list_tasks"
	ToolGetTask           = "get_task"
	ToolGetTaskTrace      = "get_task_trace"
	ToolListConversations = "list_conversations"
	ToolGetConversation   = "get_conversation"
	ToolConvTrace         = "get_conversation_trace"
	ToolListHistories     = "list_operation_histories"
)

// TaskSource 任务侧依赖（*agentfactory.UseCase 直接实现）
type TaskSource interface {
	ListTasks(ctx context.Context, f agentfactory.TaskFilter) ([]agentfactory.Task, error)
	GetTask(ctx context.Context, id uint64) (*agentfactory.Task, error)
	TaskEvents(ctx context.Context, taskID uint64, limit int) ([]agentfactory.TaskEvent, error)
}

// ConvSource 会话侧依赖（*conversation.UseCase 直接实现）
type ConvSource interface {
	List(ctx context.Context, f conversation.Filter) ([]conversation.Conversation, int64, error)
	Get(ctx context.Context, id string) (*conversation.Conversation, error)
	ListMessagesPage(ctx context.Context, id string, limit uint32, beforeID int64) ([]conversation.Message, bool, error)
}

// HistorySource 链路明细依赖（*history.UseCase 直接实现）
type HistorySource interface {
	List(ctx context.Context, f history.Filter) ([]history.Record, int64, error)
}

// Tool 一个 MCP 工具定义（InputSchema 为 JSON 原文）
type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema string
}

// UseCase 工具实现集合
type UseCase struct {
	tasks TaskSource
	convs ConvSource
	hist  HistorySource
}

// New 构造
func New(tasks TaskSource, convs ConvSource, hist HistorySource) *UseCase {
	return &UseCase{tasks: tasks, convs: convs, hist: hist}
}

// Tools 全部工具定义（tools/list 直接返回）
func (u *UseCase) Tools() []Tool {
	return []Tool{
		{
			Name:  ToolListTasks,
			Title: "任务列表",
			Description: "查询平台任务中心的任务列表（Agent 异步任务）。可按状态、Agent、关键词、来源会话过滤，" +
				"默认只返回根任务。用于回答「有哪些任务/哪些失败了/某 Agent 最近跑了什么」。",
			InputSchema: `{"type":"object","properties":{` +
				`"status":{"type":"string","enum":["pending","running","succeeded","failed","cancelled"],"description":"按状态过滤"},` +
				`"agent_key":{"type":"string","description":"按 Agent 标识过滤，如 agent-trace-scorer"},` +
				`"keyword":{"type":"string","description":"标题/输入关键词"},` +
				`"conv_id":{"type":"string","description":"按来源会话 id 过滤"},` +
				`"include_children":{"type":"boolean","description":"是否包含委派产生的子任务，默认 false"},` +
				`"limit":{"type":"integer","minimum":1,"maximum":100,"description":"返回条数，默认 20"}}}`,
		},
		{
			Name:  ToolGetTask,
			Title: "任务详情",
			Description: "按任务 id 获取任务详情：输入、状态、进度、结果、错误、耗时，以及委派产生的子任务列表。" +
				"需要看执行过程时用 get_task_trace。",
			InputSchema: `{"type":"object","properties":{` +
				`"task_id":{"type":"integer","description":"任务 id"},` +
				`"include_children":{"type":"boolean","description":"是否附带子任务列表，默认 true"}},` +
				`"required":["task_id"]}`,
		},
		{
			Name:  ToolGetTaskTrace,
			Title: "任务执行链路",
			Description: "获取任务完整执行链路（trace 事件流）：思考、工具调用与结果、委派、计划、自评/反思、错误、最终答案。" +
				"用于评审 Agent 执行过程；返回按 seq 排序的事件与统计（工具调用数/失败数/委派数）。",
			InputSchema: `{"type":"object","properties":{` +
				`"task_id":{"type":"integer","description":"任务 id"},` +
				`"types":{"type":"array","items":{"type":"string"},"description":"只返回这些事件类型，如 [\"tool_call\",\"tool_result\",\"final\"]"},` +
				`"limit":{"type":"integer","minimum":1,"maximum":1000,"description":"事件条数上限，默认 200"},` +
				`"max_chars":{"type":"integer","minimum":100,"maximum":4000,"description":"单条 payload 最大字符数，默认 600，超出截断"}},` +
				`"required":["task_id"]}`,
		},
		{
			Name:  ToolListConversations,
			Title: "会话列表",
			Description: "查询会话目录（任务/会话中心里的会话）：标题、默认 Agent、消息数、最近消息预览、更新时间。" +
				"支持关键词搜索与分页。",
			InputSchema: `{"type":"object","properties":{` +
				`"keyword":{"type":"string","description":"标题/最近消息关键词"},` +
				`"limit":{"type":"integer","minimum":1,"maximum":200,"description":"返回条数，默认 20"},` +
				`"offset":{"type":"integer","minimum":0,"description":"偏移量，默认 0"}}}`,
		},
		{
			Name:  ToolGetConversation,
			Title: "会话详情与消息",
			Description: "按会话 id 获取会话信息与最近消息（按 id 正序）。消息含角色、发言者、内容、meta（工具调用链）、关联任务 id。" +
				"需要工具调用/召回明细时用 get_conversation_trace。",
			InputSchema: `{"type":"object","properties":{` +
				`"conv_id":{"type":"string","description":"会话 id"},` +
				`"limit":{"type":"integer","minimum":1,"maximum":100,"description":"消息条数，默认 20"},` +
				`"before_id":{"type":"integer","description":"只取该消息 id 之前（更早）的一页，用于向上翻页"}},` +
				`"required":["conv_id"]}`,
		},
		{
			Name:  ToolConvTrace,
			Title: "会话链路明细",
			Description: "按会话 id 查询该会话内发生的工具调用、技能使用、记忆召回、RAG/GraphRAG 召回明细（操作历史）。" +
				"可按类型与成功/失败过滤，用于说明「这个会话里到底调了什么、召回了什么」。",
			InputSchema: `{"type":"object","properties":{` +
				`"conv_id":{"type":"string","description":"会话 id"},` +
				`"kind":{"type":"string","enum":["mcp_call","skill_use","memory_recall","rag_recall","graph_recall"],"description":"按类型过滤"},` +
				`"ok":{"type":"boolean","description":"只返回成功/失败记录"},` +
				`"limit":{"type":"integer","minimum":1,"maximum":200,"description":"返回条数，默认 50"}},` +
				`"required":["conv_id"]}`,
		},
		{
			Name:  ToolListHistories,
			Title: "操作历史查询",
			Description: "查询统一操作历史（MCP 调用 / 技能 / 记忆召回 / RAG / GraphRAG）：可按任务 id、会话 id、类型、对象名过滤。" +
				"适合按任务维度查看链路细节（get_task_trace 只有 Agent 事件，这里能看到工具与召回的明细）。",
			InputSchema: `{"type":"object","properties":{` +
				`"task_id":{"type":"integer","description":"按任务 id 过滤"},` +
				`"conv_id":{"type":"string","description":"按会话 id 过滤"},` +
				`"kind":{"type":"string","enum":["mcp_call","skill_use","memory_recall","rag_recall","graph_recall"],"description":"按类型过滤"},` +
				`"ref":{"type":"string","description":"按操作对象精确匹配（工具唯一名/技能名/召回源）"},` +
				`"ok":{"type":"boolean","description":"只返回成功/失败记录"},` +
				`"limit":{"type":"integer","minimum":1,"maximum":200,"description":"返回条数，默认 50"}}}`,
		},
	}
}

// Call 执行一次工具调用，返回可直接塞进 MCP content 的文本（JSON）。
func (u *UseCase) Call(ctx context.Context, name string, raw map[string]any) (string, error) {
	a := args(raw)
	switch name {
	case ToolListTasks:
		return u.listTasks(ctx, a)
	case ToolGetTask:
		return u.getTask(ctx, a)
	case ToolGetTaskTrace:
		return u.getTaskTrace(ctx, a)
	case ToolListConversations:
		return u.listConversations(ctx, a)
	case ToolGetConversation:
		return u.getConversation(ctx, a)
	case ToolConvTrace:
		return u.convTrace(ctx, a)
	case ToolListHistories:
		return u.listHistories(ctx, a)
	default:
		return "", fmt.Errorf("未知工具 %q", name)
	}
}

// —— 任务 ——

func (u *UseCase) listTasks(ctx context.Context, a args) (string, error) {
	f := agentfactory.TaskFilter{
		AgentKey: a.str("agent_key"),
		Status:   a.str("status"),
		Keyword:  a.str("keyword"),
		ConvID:   a.str("conv_id"),
		RootOnly: !a.boolean("include_children", false),
		Limit:    a.integer("limit", 20, 1, 100),
		// 平台 MCP 工具同样按调用方身份收窄，避免跨租户/跨用户列出他人任务
		TenantID: identity.TenantID(ctx),
		UserID:   identity.UserID(ctx),
	}
	tasks, err := u.tasks.ListTasks(ctx, f)
	if err != nil {
		return "", fmt.Errorf("查询任务列表失败: %w", err)
	}
	items := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		items = append(items, map[string]any{
			"task_id":        t.ID,
			"agent_key":      t.AgentKey,
			"title":          clip(t.Title, 120),
			"status":         t.Status,
			"progress":       t.Progress,
			"conv_id":        t.ConvID,
			"parent_task_id": t.ParentTaskID,
			"created_at":     ts(t.CreatedAt),
			"finished_at":    ts(t.FinishedAt),
		})
	}
	return encode(map[string]any{
		"count": len(items),
		"tasks": items,
		"hint":  "用 get_task 看详情，get_task_trace 看执行链路，list_operation_histories 看工具/召回明细",
	})
}

func (u *UseCase) getTask(ctx context.Context, a args) (string, error) {
	id := uint64(a.integer("task_id", 0, 0, 0))
	if id == 0 {
		return "", fmt.Errorf("task_id 必填")
	}
	t, err := u.tasks.GetTask(ctx, id)
	if err != nil {
		return "", fmt.Errorf("查询任务失败: %w", err)
	}
	out := map[string]any{
		"task_id":        t.ID,
		"agent_key":      t.AgentKey,
		"status":         t.Status,
		"progress":       t.Progress,
		"conv_id":        t.ConvID,
		"parent_task_id": t.ParentTaskID,
		"depth":          t.Depth,
		"created_at":     ts(t.CreatedAt),
		"started_at":     ts(t.StartedAt),
		"finished_at":    ts(t.FinishedAt),
		"duration_ms":    durationMS(t.StartedAt, t.FinishedAt),
		"input":          clip(t.Input, 2000),
		"result":         clip(t.Result, 3000),
		"error":          clip(t.ErrorMsg, 1000),
	}
	if a.boolean("include_children", true) {
		children, err := u.tasks.ListTasks(ctx, agentfactory.TaskFilter{ParentTaskID: id, Limit: 50})
		if err == nil && len(children) > 0 {
			brief := make([]map[string]any, 0, len(children))
			for _, c := range children {
				brief = append(brief, map[string]any{
					"task_id": c.ID, "agent_key": c.AgentKey, "status": c.Status,
					"title": clip(c.Title, 80), "depth": c.Depth,
				})
			}
			out["children"] = brief
		}
	}
	out["hint"] = "用 get_task_trace 查看该任务的思考/工具调用/最终答案"
	return encode(out)
}

// traceStat 汇总链路特征，便于 Agent 直接引用而不是自己数事件
type traceStat struct {
	Events      int `json:"events"`
	ToolCalls   int `json:"tool_calls"`
	ToolErrors  int `json:"tool_errors"`
	Delegates   int `json:"delegates"`
	Evaluations int `json:"evaluations"`
	Errors      int `json:"errors"`
}

func (u *UseCase) getTaskTrace(ctx context.Context, a args) (string, error) {
	id := uint64(a.integer("task_id", 0, 0, 0))
	if id == 0 {
		return "", fmt.Errorf("task_id 必填")
	}
	limit := a.integer("limit", 200, 1, 1000)
	maxChars := a.integer("max_chars", 600, 100, 4000)
	only := map[string]bool{}
	for _, t := range a.strs("types") {
		only[strings.TrimSpace(t)] = true
	}

	events, err := u.tasks.TaskEvents(ctx, id, limit)
	if err != nil {
		return "", fmt.Errorf("查询任务链路失败: %w", err)
	}
	stat := traceStat{}
	items := make([]map[string]any, 0, len(events))
	seq := 0
	for _, e := range events {
		stat.Events++
		payload := e.Payload
		var obj map[string]any
		_ = json.Unmarshal([]byte(payload), &obj)
		switch e.Type {
		case agentfactory.EventToolCall:
			stat.ToolCalls++
		case agentfactory.EventToolResult:
			if b, ok := obj["is_error"].(bool); ok && b {
				stat.ToolErrors++
			}
		case agentfactory.EventDelegate:
			stat.Delegates++
		case agentfactory.EventEvaluate:
			stat.Evaluations++
		case agentfactory.EventError:
			stat.Errors++
		}
		if len(only) > 0 && !only[e.Type] {
			continue
		}
		seq++
		item := map[string]any{
			"seq":  seq,
			"type": e.Type,
			"at":   ts(e.CreatedAt),
		}
		if t := intFromAny(obj["turn"]); t > 0 {
			item["turn"] = t
		}
		// payload 统一返回对象：超长字符串字段逐个截断（保持结构可解析，避免对象/字符串类型来回变）
		if obj == nil {
			item["payload"] = map[string]any{"raw": clip(payload, maxChars)}
			item["truncated"] = len([]rune(payload)) > maxChars
		} else {
			clipped := clipJSON(obj, maxChars)
			item["payload"] = clipped
			if len([]rune(payload)) > maxChars {
				item["truncated"] = true
			}
		}
		items = append(items, item)
	}
	return encode(map[string]any{
		"task_id": id,
		"count":   len(items),
		"stat":    stat,
		"events":  items,
		"hint":    "多个 Agent 协作时，delegate 事件里的 child_task_id 可用 get_task_trace 继续展开",
	})
}

// —— 会话 ——

func (u *UseCase) listConversations(ctx context.Context, a args) (string, error) {
	f := conversation.Filter{
		Kw:     a.str("keyword"),
		Limit:  a.integer("limit", 20, 1, 200),
		Offset: a.integer("offset", 0, 0, 0),
	}
	convs, total, err := u.convs.List(ctx, f)
	if err != nil {
		return "", fmt.Errorf("查询会话列表失败: %w", err)
	}
	items := make([]map[string]any, 0, len(convs))
	for _, c := range convs {
		items = append(items, map[string]any{
			"conv_id":       c.ID,
			"title":         clip(c.Title, 120),
			"agent_key":     c.AgentKey,
			"message_count": c.MessageCount,
			"last_message":  clip(c.LastMessage, 200),
			"has_debate":    c.HasDebate,
			"created_at":    ts(c.CreatedAt),
			"updated_at":    ts(c.UpdatedAt),
		})
	}
	return encode(map[string]any{
		"total":         total,
		"count":         len(items),
		"conversations": items,
		"hint":          "用 get_conversation 看消息，get_conversation_trace 看该会话的工具调用与召回明细",
	})
}

func (u *UseCase) getConversation(ctx context.Context, a args) (string, error) {
	id := a.str("conv_id")
	if id == "" {
		return "", fmt.Errorf("conv_id 必填")
	}
	c, err := u.convs.Get(ctx, id)
	if err != nil {
		return "", fmt.Errorf("查询会话失败: %w", err)
	}
	limit := uint32(a.integer("limit", 20, 1, 100))
	before := int64(a.integer("before_id", 0, 0, 0))
	msgs, hasMore, err := u.convs.ListMessagesPage(ctx, id, limit, before)
	if err != nil {
		return "", fmt.Errorf("查询会话消息失败: %w", err)
	}
	items := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		item := map[string]any{
			"message_id": m.ID,
			"role":       m.Role,
			"speaker":    m.Speaker,
			"content":    clip(m.Content, 1500),
			"created_at": ts(m.CreatedAt),
		}
		if m.TaskID > 0 {
			item["task_id"] = m.TaskID
		}
		if strings.TrimSpace(m.Meta) != "" {
			item["meta"] = clip(m.Meta, 600)
		}
		items = append(items, item)
	}
	return encode(map[string]any{
		"conversation": map[string]any{
			"conv_id":       c.ID,
			"title":         c.Title,
			"agent_key":     c.AgentKey,
			"message_count": c.MessageCount,
			"created_at":    ts(c.CreatedAt),
			"updated_at":    ts(c.UpdatedAt),
		},
		"count":    len(items),
		"has_more": hasMore,
		"messages": items,
		"hint":     "还有更早消息时传 before_id=最早一条 message_id 继续翻页",
	})
}

func (u *UseCase) convTrace(ctx context.Context, a args) (string, error) {
	id := a.str("conv_id")
	if id == "" {
		return "", fmt.Errorf("conv_id 必填")
	}
	return u.queryHistories(ctx, a, id)
}

func (u *UseCase) listHistories(ctx context.Context, a args) (string, error) {
	return u.queryHistories(ctx, a, a.str("conv_id"))
}

func (u *UseCase) queryHistories(ctx context.Context, a args, convID string) (string, error) {
	f := history.Filter{
		Kind:   a.str("kind"),
		TaskID: uint64(a.integer("task_id", 0, 0, 0)),
		ConvID: convID,
		Ref:    a.str("ref"),
		Limit:  a.integer("limit", 50, 1, 200),
	}
	if v, ok := a["ok"]; ok {
		if b, isBool := v.(bool); isBool {
			f.OK = &b
		}
	}
	recs, total, err := u.hist.List(ctx, f)
	if err != nil {
		return "", fmt.Errorf("查询操作历史失败: %w", err)
	}
	items := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		item := map[string]any{
			"kind":       r.Kind,
			"ref":        r.Ref,
			"ok":         r.OK,
			"latency_ms": r.LatencyMS,
			"created_at": ts(r.CreatedAt),
		}
		if r.TaskID > 0 {
			item["task_id"] = r.TaskID
		}
		if r.ConvID != "" {
			item["conv_id"] = r.ConvID
		}
		if r.AgentKey != "" {
			item["agent_key"] = r.AgentKey
		}
		if q := clip(r.Query, 400); q != "" {
			item["query"] = q
		}
		if s := clip(r.Summary, 600); s != "" {
			item["summary"] = s
		}
		if d := clip(r.Detail, 800); d != "" {
			item["detail"] = d
		}
		items = append(items, item)
	}
	return encode(map[string]any{
		"total":   total,
		"count":   len(items),
		"records": items,
	})
}

// —— 小工具 ——

type args map[string]any

func (a args) str(k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	}
	return ""
}

func (a args) integer(k string, def, min, max int) int {
	n := def
	switch v := a[k].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			n = int(i)
		}
	}
	if max > 0 && n > max {
		n = max
	}
	if n < min {
		n = min
	}
	return n
}

func (a args) boolean(k string, def bool) bool {
	if v, ok := a[k].(bool); ok {
		return v
	}
	return def
}

func (a args) strs(k string) []string {
	raw, ok := a[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func encode(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("…（截断，原 %d 字）", len(r))
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func durationMS(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

// intFromAny 解析事件 payload 里的 turn（JSON 数字默认是 float64）
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

// clipJSON 递归截断 JSON 里的超长字符串，保持结构不变
func clipJSON(v any, max int) any {
	switch x := v.(type) {
	case string:
		return clip(x, max)
	case map[string]any:
		for k, vv := range x {
			x[k] = clipJSON(vv, max)
		}
		return x
	case []any:
		for i, vv := range x {
			x[i] = clipJSON(vv, max)
		}
		return x
	}
	return v
}
