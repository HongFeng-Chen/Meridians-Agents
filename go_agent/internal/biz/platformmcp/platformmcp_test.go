package platformmcp

// 工具层单测：用内存 fake 覆盖过滤参数、截断、统计与错误分支。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/conversation"
	"go_agent/internal/biz/history"
)

type fakeTasks struct {
	lastFilter agentfactory.TaskFilter
	tasks      []agentfactory.Task
	task       *agentfactory.Task
	events     []agentfactory.TaskEvent
}

func (f *fakeTasks) ListTasks(_ context.Context, in agentfactory.TaskFilter) ([]agentfactory.Task, error) {
	f.lastFilter = in
	return f.tasks, nil
}

func (f *fakeTasks) GetTask(_ context.Context, id uint64) (*agentfactory.Task, error) {
	if f.task != nil {
		return f.task, nil
	}
	return &agentfactory.Task{ID: id, AgentKey: "a", Status: "succeeded"}, nil
}

func (f *fakeTasks) TaskEvents(_ context.Context, taskID uint64, limit int) ([]agentfactory.TaskEvent, error) {
	return f.events, nil
}

type fakeConvs struct {
	lastFilter conversation.Filter
	convs      []conversation.Conversation
	msgs       []conversation.Message
	hasMore    bool
}

func (f *fakeConvs) List(_ context.Context, in conversation.Filter) ([]conversation.Conversation, int64, error) {
	f.lastFilter = in
	return f.convs, int64(len(f.convs)), nil
}

func (f *fakeConvs) Get(_ context.Context, id string) (*conversation.Conversation, error) {
	return &conversation.Conversation{ID: id, Title: "会话", MessageCount: 3}, nil
}

func (f *fakeConvs) ListMessagesPage(_ context.Context, id string, limit uint32, beforeID int64) ([]conversation.Message, bool, error) {
	return f.msgs, f.hasMore, nil
}

type fakeHist struct {
	lastFilter history.Filter
	recs       []history.Record
}

func (f *fakeHist) List(_ context.Context, in history.Filter) ([]history.Record, int64, error) {
	f.lastFilter = in
	return f.recs, int64(len(f.recs)), nil
}

func newTestUC() (*UseCase, *fakeTasks, *fakeConvs, *fakeHist) {
	t, c, h := &fakeTasks{}, &fakeConvs{}, &fakeHist{}
	return New(t, c, h), t, c, h
}

func TestToolsDefinition(t *testing.T) {
	uc, _, _, _ := newTestUC()
	tools := uc.Tools()
	if len(tools) != 7 {
		t.Fatalf("工具数量 = %d, want 7", len(tools))
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.Name] {
			t.Fatalf("工具名重复: %s", tool.Name)
		}
		seen[tool.Name] = true
		if tool.Description == "" {
			t.Fatalf("%s 缺少描述（模型靠它选工具）", tool.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(tool.InputSchema), &schema); err != nil {
			t.Fatalf("%s inputSchema 不是合法 JSON: %v", tool.Name, err)
		}
		if schema["type"] != "object" {
			t.Fatalf("%s inputSchema 顶层应为 object", tool.Name)
		}
	}
}

func TestListTasksFiltersAndClamp(t *testing.T) {
	uc, tasks, _, _ := newTestUC()
	tasks.tasks = []agentfactory.Task{{
		ID: 42, AgentKey: "issue-researcher", Title: strings.Repeat("标题", 100),
		Status: "succeeded", ConvID: "c1", CreatedAt: time.Now(),
	}}
	out, err := uc.Call(context.Background(), ToolListTasks, map[string]any{
		"status": "failed", "agent_key": "a", "keyword": "kw", "limit": float64(500),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if tasks.lastFilter.Status != "failed" || tasks.lastFilter.AgentKey != "a" || tasks.lastFilter.Keyword != "kw" {
		t.Fatalf("过滤条件未透传: %+v", tasks.lastFilter)
	}
	if tasks.lastFilter.Limit != 100 {
		t.Fatalf("limit 应钳制到 100, got %d", tasks.lastFilter.Limit)
	}
	if !tasks.lastFilter.RootOnly {
		t.Fatal("默认只列根任务")
	}
	var res struct {
		Count int `json:"count"`
		Tasks []struct {
			Title string `json:"title"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	if res.Count != 1 || !strings.Contains(res.Tasks[0].Title, "截断") {
		t.Fatalf("标题应被截断: %s", res.Tasks[0].Title)
	}
}

func TestTaskTraceStatsAndTruncate(t *testing.T) {
	uc, tasks, _, _ := newTestUC()
	tasks.events = []agentfactory.TaskEvent{
		{Type: agentfactory.EventThought, Payload: `{"turn":1,"thought":"思考"}`},
		{Type: agentfactory.EventToolCall, Payload: `{"turn":1,"name":"mcp:a__b","args":{}}`},
		{Type: agentfactory.EventToolResult, Payload: `{"turn":1,"is_error":true,"result":"` + strings.Repeat("报错", 200) + `"}`},
		{Type: agentfactory.EventDelegate, Payload: `{"turn":2,"agent":"x","child_task_id":7}`},
		{Type: agentfactory.EventEvaluate, Payload: `{"round":1,"pass":true,"score":88}`},
		{Type: agentfactory.EventFinal, Payload: `{"answer":"ok"}`},
	}
	out, err := uc.Call(context.Background(), ToolGetTaskTrace, map[string]any{
		"task_id": float64(42), "max_chars": float64(120),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var res struct {
		Count int `json:"count"`
		Stat  struct {
			ToolCalls  int `json:"tool_calls"`
			ToolErrors int `json:"tool_errors"`
			Delegates  int `json:"delegates"`
		} `json:"stat"`
		Events []struct {
			Seq       int            `json:"seq"`
			Type      string         `json:"type"`
			Payload   map[string]any `json:"payload"`
			Truncated bool           `json:"truncated"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, out)
	}
	if res.Stat.ToolCalls != 1 || res.Stat.ToolErrors != 1 || res.Stat.Delegates != 1 {
		t.Fatalf("统计错误: %+v", res.Stat)
	}
	if res.Count != 6 {
		t.Fatalf("事件数 = %d, want 6", res.Count)
	}
	if !res.Events[2].Truncated {
		t.Fatal("超长 payload 应标记 truncated")
	}
	if got, _ := res.Events[2].Payload["result"].(string); !strings.Contains(got, "截断") {
		t.Fatalf("超长字段应被截断: %v", res.Events[2].Payload["result"])
	}
	if res.Events[0].Payload["thought"] != "思考" {
		t.Fatalf("短字段应保留结构: %+v", res.Events[0].Payload)
	}

	// types 过滤
	out, err = uc.Call(context.Background(), ToolGetTaskTrace, map[string]any{
		"task_id": float64(42), "types": []any{"tool_call", "final"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("types 过滤后 = %d, want 2", res.Count)
	}
}

func TestConversationTools(t *testing.T) {
	uc, _, convs, hist := newTestUC()
	convs.convs = []conversation.Conversation{{ID: "c1", Title: "任务/会话"}}
	convs.msgs = []conversation.Message{{ID: 10, Role: "user", Content: "hi"}, {ID: 11, Role: "assistant", Content: "yo", TaskID: 42}}
	convs.hasMore = true
	hist.recs = []history.Record{{Kind: history.KindMCPCall, Ref: "a__b", OK: true, TaskID: 42, ConvID: "c1"}}

	if _, err := uc.Call(context.Background(), ToolListConversations, map[string]any{"keyword": "任务"}); err != nil {
		t.Fatalf("list_conversations: %v", err)
	}
	if convs.lastFilter.Kw != "任务" {
		t.Fatalf("关键词未透传: %+v", convs.lastFilter)
	}

	out, err := uc.Call(context.Background(), ToolGetConversation, map[string]any{"conv_id": "c1", "limit": float64(2)})
	if err != nil {
		t.Fatalf("get_conversation: %v", err)
	}
	if !strings.Contains(out, `"has_more":true`) || !strings.Contains(out, `"task_id":42`) {
		t.Fatalf("会话消息输出异常: %s", out)
	}

	if _, err := uc.Call(context.Background(), ToolConvTrace, map[string]any{"conv_id": "c1", "ok": false}); err != nil {
		t.Fatalf("conv trace: %v", err)
	}
	if hist.lastFilter.ConvID != "c1" || hist.lastFilter.OK == nil || *hist.lastFilter.OK {
		t.Fatalf("会话链路过滤条件错误: %+v", hist.lastFilter)
	}
}

func TestErrorsAndUnknownTool(t *testing.T) {
	uc, _, _, _ := newTestUC()
	if _, err := uc.Call(context.Background(), "nope", map[string]any{}); err == nil {
		t.Fatal("未知工具应报错")
	}
	if _, err := uc.Call(context.Background(), ToolGetTask, map[string]any{}); err == nil {
		t.Fatal("缺少 task_id 应报错")
	}
	if _, err := uc.Call(context.Background(), ToolGetConversation, map[string]any{}); err == nil {
		t.Fatal("缺少 conv_id 应报错")
	}
}
