// store.go 进程内 span 存储：把已结束的 Span 摘要按 Trace 分组保留在环形缓冲里。
//
// 为什么要有它：OTLP 导出是「有 collector 才有」的能力，但「这条任务到底慢在哪」
// 是运维每天都要回答的问题。装不起 Jaeger 的环境（单机部署、内网隔离）不该因此
// 丧失链路可观测性——内存里留最近 N 条 trace，任务详情页就能直接看 span 树。
//
// 取舍：只存摘要（名称 / 父 span / 起止 / 状态 / 属性），不存事件与链接；
// 进程重启即丢，量级由 max 控制。要长期留存请配 OTEL_EXPORTER_OTLP_ENDPOINT。
package trace

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Span 一条 span 的摘要。
type Span struct {
	TraceID  string `json:"trace_id"`
	SpanID   string `json:"span_id"`
	ParentID string `json:"parent_id,omitempty"`
	Name     string `json:"name"`
	// Kind 分组用的粗粒度类型：task / llm / tool / rag / graphrag / other
	Kind string `json:"kind"`

	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// DurationMS 耗时（毫秒，便于前端直接排序）
	DurationMS int64 `json:"duration_ms"`
	// Status ok / error / unset
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	Attrs map[string]any `json:"attrs,omitempty"`
}

// Trace 一次请求（同一 TraceID）的全部 span。
type Trace struct {
	TraceID string `json:"trace_id"`
	// Name 根 span 名称（无根时取最早开始的 span）
	Name string `json:"name"`
	// TaskID 从根 span 属性里提取，方便按任务检索（空 = 与任务无关的请求）
	TaskID  string    `json:"task_id,omitempty"`
	Agent   string    `json:"agent_key,omitempty"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Spans   []Span    `json:"spans"`
	Error   bool      `json:"has_error"`
	Running bool      `json:"running"`
}

// SpanFilter trace 列表过滤条件
type SpanFilter struct {
	TaskID string
	// Name 根 span 名称前缀过滤（如 "harness"）
	Name  string
	Limit int
}

// Store 进程内 trace 环形存储（并发安全）。
type Store struct {
	mu      sync.RWMutex
	max     int
	order   []string            // traceID 到达顺序（FIFO 淘汰）
	traces  map[string]*Trace   // traceID → trace
	pending map[string]struct{} // 已淘汰但仍有 span 迟到的 trace（丢弃，避免无界增长）
}

// NewStore 创建存储；max <= 0 时用 512。
func NewStore(max int) *Store {
	if max <= 0 {
		max = 512
	}
	return &Store{
		max:     max,
		order:   make([]string, 0, max),
		traces:  make(map[string]*Trace, max),
		pending: make(map[string]struct{}, 64),
	}
}

// Add 写入一条 span；超出容量时淘汰最早开始的 trace。
func (s *Store) Add(sp Span) {
	if s == nil || sp.TraceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.traces[sp.TraceID]
	if !ok {
		if _, dropped := s.pending[sp.TraceID]; dropped {
			return // 这条 trace 已被淘汰：迟到的 span 直接丢弃
		}
		t = &Trace{TraceID: sp.TraceID, Start: sp.Start}
		s.traces[sp.TraceID] = t
		s.order = append(s.order, sp.TraceID)
	}
	t.Spans = append(t.Spans, sp)
	if t.Start.IsZero() || sp.Start.Before(t.Start) {
		t.Start = sp.Start
	}
	if sp.End.After(t.End) {
		t.End = sp.End
	}
	// 根 span：没有父，或父不在本次采样里
	if sp.ParentID == "" {
		t.Name = sp.Name
		if v, ok := sp.Attrs["task.id"]; ok {
			t.TaskID = toString(v)
		}
		if v, ok := sp.Attrs["task.agent_key"]; ok && t.Agent == "" {
			t.Agent = toString(v)
		}
	}
	if t.TaskID == "" {
		if v, ok := sp.Attrs["task.id"]; ok {
			t.TaskID = toString(v)
		}
	}
	if sp.Status == "error" {
		t.Error = true
	}
	s.evictLocked()
}

// Ensure 登记一条「正在执行」的 trace（根 span 开始时调用）。
//
// 为什么不等根 span 结束再登记：任务跑几分钟时，运维已经在问「它卡在哪」。
// 先占位（Running=true、spans 为空），子 span 结束会陆续追加，根 span 结束时
// 由 maybeFinish 置为已完成。这样列表里「运行中」与「已完成」一眼可分。
func (s *Store) Ensure(traceID, name, taskID, agent string, start time.Time) {
	if s == nil || traceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.traces[traceID]; ok {
		t.Running = true
		return
	}
	if _, dropped := s.pending[traceID]; dropped {
		return
	}
	s.traces[traceID] = &Trace{
		TraceID: traceID,
		Name:    name,
		TaskID:  taskID,
		Agent:   agent,
		Start:   start,
		Running: true,
	}
	s.order = append(s.order, traceID)
	s.evictLocked()
}

// Finish 标记一条 trace 的根 span 已结束（用于区分「进行中」与「已完成」）。
//
// 不走 Add 的原因：根 span 结束时才知道整条 trace 的结束时间，
// 这里只更新聚合字段，不重复写 span。
func (s *Store) Finish(traceID string) {
	if s == nil || traceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.traces[traceID]; ok {
		t.Running = false
	}
}

// MarkRunning 标记 trace 仍在执行（根 span 尚未结束）。
func (s *Store) MarkRunning(traceID string) {
	if s == nil || traceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.traces[traceID]; ok {
		t.Running = true
	}
}

func (s *Store) evictLocked() {
	for len(s.order) > s.max {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.traces, old)
		s.pending[old] = struct{}{}
	}
	// pending 只需要挡住「刚被淘汰的 trace 的迟到 span」，按同样容量封顶
	for len(s.pending) > s.max {
		for k := range s.pending {
			delete(s.pending, k)
			break
		}
	}
}

// List 按过滤条件返回 trace（时间倒序，最多 limit 条）。
func (s *Store) List(f SpanFilter) []Trace {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Trace, 0, 16)
	for _, t := range s.traces {
		if f.TaskID != "" && t.TaskID != f.TaskID {
			continue
		}
		if f.Name != "" && !strings.HasPrefix(t.Name, f.Name) {
			continue
		}
		out = append(out, t.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// Get 按 traceID 取单条 trace（含完整 span 列表，按开始时间排序）。
func (s *Store) Get(traceID string) (*Trace, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.traces[traceID]
	if !ok {
		return nil, false
	}
	c := t.clone()
	return &c, true
}

// Stats 概览：条数与容量（/api/v2/traces 的 meta 用）。
func (s *Store) Stats() map[string]any {
	if s == nil {
		return map[string]any{"enabled": false}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	errs := 0
	for _, t := range s.traces {
		if t.Error {
			errs++
		}
	}
	return map[string]any{
		"enabled":  true,
		"traces":   len(s.traces),
		"capacity": s.max,
		"errors":   errs,
	}
}

// clone 深拷贝（读接口不能把内部切片交出去：调用方 append 会污染存储）
func (t *Trace) clone() Trace {
	c := *t
	c.Spans = append([]Span(nil), t.Spans...)
	// 稳定排序：Windows 上 time.Now 精度约 15ms，父子 span 起点可能完全相同，
	// 非稳定排序会让同一份数据两次查询顺序不一（前端树跳动）。
	sort.SliceStable(c.Spans, func(i, j int) bool {
		if c.Spans[i].Start.Equal(c.Spans[j].Start) {
			return c.Spans[i].SpanID < c.Spans[j].SpanID
		}
		return c.Spans[i].Start.Before(c.Spans[j].Start)
	})
	return c
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(v)
	}
}
