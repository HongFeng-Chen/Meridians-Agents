package trace

import (
	"context"
	"errors"
	"testing"
	"time"
)

// setup 初始化一次性追踪并返回 store；测试结束后关闭。
func setup(t *testing.T, maxTraces int) *Store {
	t.Helper()
	h, err := Init(Config{ServiceName: "test", MaxTraces: maxTraces})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	})
	return h.Store
}

// 未初始化时埋点不能 panic，也不能产生数据（业务代码不该判空）。
func TestStartWithoutInit(t *testing.T) {
	ctx, h := Start(context.Background(), "task.run", String(AttrTaskID, "1"))
	if h == nil {
		t.Fatal("句柄不应为 nil")
	}
	h.Set(AttrLLMModel, "m1")
	h.End(errors.New("boom"))
	h.End(nil) // 重复 End 应无副作用
	if h.TraceID() != "" {
		t.Fatalf("未初始化时不应有 trace id，得到 %q", h.TraceID())
	}
	_ = ctx
}

// 父子 span 要落在同一条 trace 且层级正确。
func TestSpanTree(t *testing.T) {
	store := setup(t, 64)

	ctx, root := Start(context.Background(), "task.run",
		String(AttrKind, KindTask), String(AttrTaskID, "42"), String(AttrAgentKey, "writer"))
	if root.TraceID() == "" {
		t.Fatal("已初始化时应有 trace id")
	}
	_, llm := Start(ctx, "llm.chat", String(AttrKind, KindLLM))
	time.Sleep(2 * time.Millisecond)
	llm.End(nil)

	_, tool := Start(ctx, "mcp.call", String(AttrKind, KindTool))
	tool.End(errors.New("tool down"))

	root.End(nil)

	tr, ok := store.Get(root.TraceID())
	if !ok {
		t.Fatal("trace 未写入 store")
	}
	if len(tr.Spans) != 3 {
		t.Fatalf("应记录 3 个 span，得到 %d", len(tr.Spans))
	}
	if tr.Name != "task.run" || tr.TaskID != "42" || tr.Agent != "writer" {
		t.Fatalf("根 span 聚合字段不对: %+v", tr)
	}
	if tr.Running {
		t.Fatal("根 span 已结束，trace 不应仍在运行")
	}
	if !tr.Error {
		t.Fatal("子 span 失败应标记 trace has_error")
	}

	// 子 span 的 parent 必须是根 span（不依赖顺序：先定位根，再逐条校验）
	var rootID string
	for _, s := range tr.Spans {
		if s.Name == "task.run" {
			rootID = s.SpanID
		}
	}
	if rootID == "" {
		t.Fatal("未找到根 span")
	}
	for _, s := range tr.Spans {
		if s.Name == "task.run" {
			continue
		}
		if s.ParentID != rootID {
			t.Fatalf("%s 的 parent 不对: %q != %q", s.Name, s.ParentID, rootID)
		}
		if s.Name == "mcp.call" {
			if s.Status != "error" || s.Error == "" {
				t.Fatalf("失败 span 状态不对: status=%q err=%q", s.Status, s.Error)
			}
			if s.Kind != KindTool {
				t.Fatalf("span.kind 属性未生效: %q", s.Kind)
			}
		}
	}
}

// 运行中的任务也要能查到 trace（根 span 未结束就先占位）。
func TestRunningTraceIsVisible(t *testing.T) {
	store := setup(t, 8)
	ctx, root := Start(context.Background(), "task.run",
		String(AttrTaskID, "9"), String(AttrAgentKey, "writer"))

	list := store.List(SpanFilter{TaskID: "9"})
	if len(list) != 1 {
		t.Fatalf("根 span 刚开始就应能看到 trace，得到 %d 条", len(list))
	}
	if !list[0].Running {
		t.Fatal("根 span 未结束，trace 应标记 running")
	}
	if list[0].TaskID != "9" || list[0].Agent != "writer" {
		t.Fatalf("运行中 trace 的身份字段丢失: %+v", list[0])
	}
	if len(list[0].Spans) != 0 {
		t.Fatalf("此时还没有结束的 span，得到 %d 条", len(list[0].Spans))
	}

	// 子 span 结束即入列
	_, child := Start(ctx, "llm.chat")
	child.End(nil)
	if got := store.List(SpanFilter{TaskID: "9"})[0]; len(got.Spans) != 1 || !got.Running {
		t.Fatalf("子 span 结束后应看到 1 个 span 且仍 running: %+v", got)
	}

	root.End(nil)
	final := store.List(SpanFilter{TaskID: "9"})[0]
	if final.Running {
		t.Fatal("根 span 结束后 trace 应标记完成")
	}
	if len(final.Spans) != 2 {
		t.Fatalf("应有两个 span（根 + 子），得到 %d", len(final.Spans))
	}
}

// 按 task_id 检索（前端任务详情的数据源）。
func TestListByTaskID(t *testing.T) {
	store := setup(t, 64)
	for _, tc := range []struct{ task string }{{"100"}, {"200"}} {
		ctx, root := Start(context.Background(), "task.run", String(AttrTaskID, tc.task))
		_, child := Start(ctx, "llm.chat")
		child.End(nil)
		root.End(nil)
	}
	got := store.List(SpanFilter{TaskID: "100"})
	if len(got) != 1 {
		t.Fatalf("应命中 1 条 trace，得到 %d", len(got))
	}
	if got[0].TaskID != "100" {
		t.Fatalf("task id 过滤失效: %q", got[0].TaskID)
	}
	if all := store.List(SpanFilter{}); len(all) != 2 {
		t.Fatalf("无过滤应返回 2 条，得到 %d", len(all))
	}
}

// 超出容量淘汰最老的 trace，且迟到 span 不会把已淘汰的 trace 复活。
func TestStoreEviction(t *testing.T) {
	store := NewStore(2)
	send := func(id string, start time.Time) {
		store.Add(Span{
			TraceID: id, SpanID: id + "-root", Name: "task.run",
			Start: start, End: start.Add(time.Millisecond),
			Status: "ok", Attrs: map[string]any{AttrTaskID: id},
		})
	}
	base := time.Now()
	send("t1", base)
	send("t2", base.Add(time.Second))
	send("t3", base.Add(2*time.Second))
	if got := store.List(SpanFilter{}); len(got) != 2 {
		t.Fatalf("容量 2 应只剩 2 条，得到 %d", len(got))
	}
	for _, tr := range store.List(SpanFilter{}) {
		if tr.TraceID == "t1" {
			t.Fatal("最老的 trace 应被淘汰")
		}
	}
	// 迟到 span：淘汰后同名 trace 不应重新出现
	store.Add(Span{TraceID: "t1", SpanID: "t1-late", Name: "llm.chat",
		Start: base, End: base.Add(time.Millisecond), Status: "ok"})
	if _, ok := store.Get("t1"); ok {
		t.Fatal("已淘汰 trace 的迟到 span 不应复活它")
	}
}

// 属性类型归一：int/float/bool 都要能落进 attrs（面板按数值展示成本与 token）。
func TestAttributeTypes(t *testing.T) {
	store := setup(t, 16)
	ctx, h := Start(context.Background(), "task.run", String(AttrTaskID, "7"))
	h.Set(AttrLLMPromptTok, int64(12))
	h.Set("llm.ratio", 0.5)
	h.Set("llm.cached", true)
	h.End(nil)
	_ = ctx

	tr, ok := store.Get(h.TraceID())
	if !ok || len(tr.Spans) == 0 {
		t.Fatal("trace 缺失")
	}
	attrs := tr.Spans[0].Attrs
	if attrs[AttrLLMPromptTok] != int64(12) {
		t.Fatalf("int64 属性丢失: %#v", attrs[AttrLLMPromptTok])
	}
	if attrs["llm.ratio"] != 0.5 {
		t.Fatalf("float 属性丢失: %#v", attrs["llm.ratio"])
	}
	if attrs["llm.cached"] != true {
		t.Fatalf("bool 属性丢失: %#v", attrs["llm.cached"])
	}
}

// 名字前缀兜底分类：埋点漏了 span.kind 也不该全落到 other。
func TestKindFromName(t *testing.T) {
	cases := map[string]string{
		"task.run":       KindTask,
		"llm.chat":       KindLLM,
		"mcp.call":       KindTool,
		"skill.run":      KindTool,
		"rag.retrieve":   KindRAG,
		"graphrag.query": KindGraphRAG,
		"whatever":       "other",
	}
	for name, want := range cases {
		if got := kindFromName(name); got != want {
			t.Fatalf("kindFromName(%q) = %q, want %q", name, got, want)
		}
	}
}
