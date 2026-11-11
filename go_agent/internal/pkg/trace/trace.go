// Package trace OpenTelemetry 接入：一次请求一条 Trace，Span 覆盖 LLM / RAG / MCP / Skill。
//
// 两条落点，各有分工：
//
//   - OTLP 导出（可选）：配了 OTEL_EXPORTER_OTLP_ENDPOINT 就送 Jaeger / Tempo，
//     适合有 collector 的集群；这是标准的分布式追踪能力。
//   - 进程内环形存储（始终开启）：最近 N 条 trace 留在内存，`/api/v2/traces`
//     直接按 task_id 查 span 树。单机部署 / 内网隔离环境没有 collector，
//     但「这条任务慢在 LLM 还是 MCP」这个问题照样要回答。
//
// 埋点约定（属性名是查询与聚合的契约，不要随意改）：
//
//	span.kind        task / llm / tool / rag / graphrag —— 分类与配色
//	task.id          任务 ID（字符串，便于与 usage_records 关联）
//	task.parent_id   父任务 ID（子 Agent 委派）
//	task.agent_key   Agent 键
//	task.snapshot_id 执行所用的配置快照
//	task.attempt     第几次尝试（重试链路看这里）
//	llm.model / llm.tokens_* / llm.cost_micros
//	tool.name / mcp.replayed
//	rag.hits / graphrag.nodes
//
// 设计取舍：不在这里做 context 之外的状态管理。未初始化时 otel 全局 provider
// 是 noop，所有 Start 返回空句柄，业务代码无需判断「追踪是否开启」。
package trace

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// scopeName instrumentation 名称（在 collector 里作为 library name 展示）
const scopeName = "go_agent"

// 属性键（契约，见包注释）
const (
	AttrKind          = "span.kind"
	AttrTaskID        = "task.id"
	AttrTaskParentID  = "task.parent_id"
	AttrAgentKey      = "task.agent_key"
	AttrSnapshotID    = "task.snapshot_id"
	AttrAttempt       = "task.attempt"
	AttrDepth         = "task.depth"
	AttrTenantID      = "task.tenant_id"
	AttrLLMModel      = "llm.model"
	AttrLLMSource     = "llm.source"
	AttrLLMPurpose    = "llm.purpose"
	AttrLLMPromptTok  = "llm.prompt_tokens"
	AttrLLMComplTok   = "llm.completion_tokens"
	AttrLLMCostMicros = "llm.cost_micros"
	AttrToolName      = "tool.name"
	AttrToolReplayed  = "mcp.replayed"
	AttrRAGHits       = "rag.hits"
	AttrGraphNodes    = "graphrag.nodes"
)

// Span 类型
const (
	KindTask     = "task"
	KindLLM      = "llm"
	KindTool     = "tool"
	KindRAG      = "rag"
	KindGraphRAG = "graphrag"
)

// Config 初始化参数
type Config struct {
	ServiceName    string
	ServiceVersion string
	// Endpoint OTLP gRPC 端点（host:port）。空 = 只保留进程内存储，不导出。
	Endpoint string
	// Insecure 端点是否明文（内网 collector 常为 true）
	Insecure bool
	// Headers OTLP 元数据（云厂商鉴权用）
	Headers map[string]string
	// SampleRatio 采样比例（0,1]，默认 1。按 TraceID 概率采样，父级决定子级。
	SampleRatio float64
	// MaxTraces 进程内保留的 trace 条数，默认 512
	MaxTraces int
}

// Handle 初始化结果：持有进程内存储与关闭函数
type Handle struct {
	Store *Store
	// Shutdown 冲刷并关闭 exporter（未配 OTLP 时为空操作）
	Shutdown func(context.Context) error
}

var (
	curMu sync.RWMutex
	cur   *Handle
)

// Init 初始化全局 TracerProvider。
//
// 重复调用会先关闭上一份（测试里换配置方便）；返回的 Handle.Shutdown 需要
// 在进程退出前调用，否则 batcher 里未导出的 span 会丢。
func Init(cfg Config) (*Handle, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "go_agent"
	}
	if cfg.SampleRatio <= 0 || cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1
	}
	store := NewStore(cfg.MaxTraces)

	opts := []sdktrace.TracerProviderOption{
		// 内存存储永远开着：它是「没有 collector 也能查链路」的唯一保障
		sdktrace.WithSpanProcessor(&storeProcessor{s: store}),
	}
	if cfg.SampleRatio < 1 {
		opts = append(opts, sdktrace.WithSampler(
			sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))))
	}

	// semconv 的包路径随版本变化，这里直接写标准键名：service.name / service.version
	attrs := []attribute.KeyValue{attribute.String("service.name", cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.ServiceVersion))
	}
	if res, err := resource.New(context.Background(), resource.WithAttributes(attrs...)); err == nil {
		opts = append(opts, sdktrace.WithResource(res))
	}

	flush := func(context.Context) error { return nil }
	// exp 由 otlp.go 提供：把「导出」与「存储」解耦，未配端点的部署不加载 grpc
	if strings.TrimSpace(cfg.Endpoint) != "" {
		exp, err := newExporter(cfg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
		flush = func(ctx context.Context) error { return exp.Shutdown(ctx) }
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	h := &Handle{Store: store, Shutdown: func(ctx context.Context) error {
		// 先停 provider 把 batcher 刷出去，再关 exporter 连接
		perr := tp.Shutdown(ctx)
		if ferr := flush(ctx); ferr != nil && perr == nil {
			return ferr
		}
		return perr
	}}

	curMu.Lock()
	prev := cur
	cur = h
	curMu.Unlock()
	if prev != nil && prev.Shutdown != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = prev.Shutdown(ctx)
		cancel()
	}
	return h, nil
}

// Current 返回当前 Handle（未初始化时 nil）。
func Current() *Handle {
	curMu.RLock()
	defer curMu.RUnlock()
	return cur
}

// Store 返回进程内存储（未初始化时 nil，查询侧需容忍）。
func StoreOf() *Store {
	if h := Current(); h != nil {
		return h.Store
	}
	return nil
}

// Enabled 是否已初始化追踪
func Enabled() bool { return Current() != nil }

// —— 埋点 API ——

// Attr 属性（对 otel attribute 的轻包装，业务侧不必 import otel）
type Attr = attribute.KeyValue

// String / Int / Int64 / Bool / Float 构造属性
func String(k, v string) Attr { return attribute.String(k, v) }
func Int(k string, v int) Attr {
	return attribute.Int(k, v)
}
func Int64(k string, v int64) Attr { return attribute.Int64(k, v) }
func Bool(k string, v bool) Attr   { return attribute.Bool(k, v) }
func Float(k string, v float64) Attr {
	return attribute.Float64(k, v)
}

// SpanHandle 埋点句柄。nil 安全：未初始化追踪时全是空操作，业务侧无需判空。
type SpanHandle struct {
	span    oteltrace.Span
	store   *Store
	traceID string
	ended   bool
}

// Start 开始一个 span 并从 ctx 派生新 ctx（子调用拿到新 ctx 即自动成为子 span）。
func Start(ctx context.Context, name string, attrs ...Attr) (context.Context, *SpanHandle) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, sp := otel.Tracer(scopeName).Start(ctx, name,
		oteltrace.WithAttributes(attrs...),
		oteltrace.WithSpanKind(oteltrace.SpanKindInternal))
	h := &SpanHandle{span: sp, store: StoreOf()}
	// noop tracer 返回的是全零 SpanContext（IsValid=false），不能当成 trace id
	if sc := sp.SpanContext(); sc.IsValid() {
		h.traceID = sc.TraceID().String()
	}
	return ctx, h
}

// TraceID 当前 span 所属 trace（未采样/未初始化时为空串）。
// 任务行上记这个值，就能从任务直接跳到 trace 详情。
func (h *SpanHandle) TraceID() string {
	if h == nil {
		return ""
	}
	return h.traceID
}

// Set 追加单个属性（类型自动归一：string / int / int64 / float64 / bool）。
func (h *SpanHandle) Set(k string, v any) {
	if h == nil || h.span == nil {
		return
	}
	switch x := v.(type) {
	case string:
		h.span.SetAttributes(attribute.String(k, x))
	case int:
		h.span.SetAttributes(attribute.Int(k, x))
	case int64:
		h.span.SetAttributes(attribute.Int64(k, x))
	case uint64:
		h.span.SetAttributes(attribute.Int64(k, int64(x)))
	case float64:
		h.span.SetAttributes(attribute.Float64(k, x))
	case bool:
		h.span.SetAttributes(attribute.Bool(k, x))
	case time.Duration:
		h.span.SetAttributes(attribute.Int64(k, x.Milliseconds()))
	default:
		h.span.SetAttributes(attribute.String(k, toString(v)))
	}
}

// SetAttrs 批量追加属性
func (h *SpanHandle) SetAttrs(attrs ...Attr) {
	if h == nil || h.span == nil || len(attrs) == 0 {
		return
	}
	h.span.SetAttributes(attrs...)
}

// Event 在 span 上记一条时间点事件（失败原因、缓存命中等）
func (h *SpanHandle) Event(name string, attrs ...Attr) {
	if h == nil || h.span == nil {
		return
	}
	h.span.AddEvent(name, oteltrace.WithAttributes(attrs...))
}

// End 结束 span。err 非空时置为 Error 并记录异常。
// 注意：defer 时 err 还没赋值，要用 EndPtr。
func (h *SpanHandle) End(err error) {
	if h == nil || h.span == nil || h.ended {
		return
	}
	h.ended = true
	if err != nil {
		h.span.RecordError(err)
		h.span.SetStatus(codes.Error, err.Error())
	}
	h.span.End()
	if h.store != nil && h.traceID != "" {
		// 只有根 span 结束才算整条 trace 结束；非根调用无副作用（幂等置 false）
		h.store.maybeFinish(h.traceID)
	}
}

// EndPtr 供 defer 使用：defer h.EndPtr(&err) —— 延迟求值拿到最终错误。
func (h *SpanHandle) EndPtr(errp *error) {
	if h == nil {
		return
	}
	if errp != nil {
		h.End(*errp)
		return
	}
	h.End(nil)
}

// Context 返回带 span 的 ctx（有时只需要把 ctx 往后传，不需要句柄）
func (h *SpanHandle) Context(ctx context.Context) context.Context {
	if h == nil || h.span == nil {
		return ctx
	}
	return oteltrace.ContextWithSpan(ctx, h.span)
}

// FromContext 取当前 ctx 上的 span 句柄（给不方便层层传参的地方加属性用）。
func FromContext(ctx context.Context) *SpanHandle {
	if ctx == nil {
		return nil
	}
	sp := oteltrace.SpanFromContext(ctx)
	if sp == nil {
		return nil
	}
	h := &SpanHandle{span: sp, store: StoreOf()}
	if sc := sp.SpanContext(); sc.IsValid() {
		h.traceID = sc.TraceID().String()
	}
	return h
}

// SetOn 在当前 ctx 的 span 上加属性（无 span 时静默返回）。
func SetOn(ctx context.Context, k string, v any) {
	FromContext(ctx).Set(k, v)
}

// —— SDK 处理器：把结束的 span 写进内存存储 ——

type storeProcessor struct{ s *Store }

// OnStart 只为根 span 登记 trace 占位：任务跑几分钟时，列表里就能看到「执行中」，
// 而不是等全部结束才出现。子 span 不登记（它们会随父一起被 Add 收进来）。
func (p *storeProcessor) OnStart(_ context.Context, rs sdktrace.ReadWriteSpan) {
	if p.s == nil {
		return
	}
	sc := rs.SpanContext()
	if !sc.IsValid() {
		return
	}
	// 有同 trace 的父 span → 不是根，跳过
	if par := rs.Parent(); par.IsValid() && par.TraceID() == sc.TraceID() {
		return
	}
	var taskID, agent string
	for _, kv := range rs.Attributes() {
		switch string(kv.Key) {
		case AttrTaskID:
			taskID = kv.Value.AsString()
		case AttrAgentKey:
			agent = kv.Value.AsString()
		}
	}
	p.s.Ensure(sc.TraceID().String(), rs.Name(), taskID, agent, rs.StartTime())
}

func (p *storeProcessor) OnEnd(ro sdktrace.ReadOnlySpan) {
	if p.s == nil {
		return
	}
	sc := ro.SpanContext()
	if !sc.IsValid() {
		return
	}
	sp := Span{
		TraceID:    sc.TraceID().String(),
		SpanID:     sc.SpanID().String(),
		Name:       ro.Name(),
		Start:      ro.StartTime(),
		End:        ro.EndTime(),
		Status:     statusString(ro),
		Attrs:      make(map[string]any, len(ro.Attributes())),
		DurationMS: ro.EndTime().Sub(ro.StartTime()).Milliseconds(),
		Kind:       "other",
	}
	// 父 span 只在同一 trace 内才算父子（跨 trace 的 link 不构成层级）
	if par := ro.Parent(); par.IsValid() && par.TraceID() == sc.TraceID() {
		sp.ParentID = par.SpanID().String()
	}
	for _, kv := range ro.Attributes() {
		k := string(kv.Key)
		v := kv.Value.AsInterface()
		sp.Attrs[k] = v
		switch k {
		case AttrKind:
			if s, ok := v.(string); ok && s != "" {
				sp.Kind = s
			}
		case AttrTaskID:
			// 任务 ID 在 trace 聚合时单独取，这里保留在 attrs 里即可
		}
	}
	if sp.Kind == "other" {
		sp.Kind = kindFromName(ro.Name())
	}
	if st := ro.Status(); st.Code == codes.Error {
		sp.Error = st.Description
	}
	p.s.Add(sp)
}

func (p *storeProcessor) Shutdown(context.Context) error   { return nil }
func (p *storeProcessor) ForceFlush(context.Context) error { return nil }

// maybeFinish 根 span（无父）结束时把 trace 标记为已完成。
func (s *Store) maybeFinish(traceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.traces[traceID]
	if !ok {
		return
	}
	// 判根：trace 里没有父 span 的即根
	for _, x := range t.Spans {
		if x.ParentID == "" {
			t.Running = false
			return
		}
	}
}

func statusString(ro sdktrace.ReadOnlySpan) string {
	switch ro.Status().Code {
	case codes.Error:
		return "error"
	case codes.Ok:
		return "ok"
	default:
		return "unset"
	}
}

// kindFromName 兜底分类：属性忘了打 span.kind 时按命名前缀推断，
// 避免整棵树全落进 other（埋点遗漏不该让面板失去分组能力）。
func kindFromName(name string) string {
	switch {
	case strings.HasPrefix(name, "task."):
		return KindTask
	case strings.HasPrefix(name, "llm."):
		return KindLLM
	case strings.HasPrefix(name, "mcp."), strings.HasPrefix(name, "tool."), strings.HasPrefix(name, "skill."):
		return KindTool
	case strings.HasPrefix(name, "graphrag."):
		return KindGraphRAG
	case strings.HasPrefix(name, "rag."):
		return KindRAG
	default:
		return "other"
	}
}
