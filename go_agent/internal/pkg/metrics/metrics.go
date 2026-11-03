// Package metrics 轻量指标注册表：计数器 / 仪表 / 直方图 + Prometheus 文本格式暴露。
//
// 为什么不用 prometheus/client_golang：本项目的指标维度很稳定（方法、路由、状态码、
// 模型、场景），手写注册表只有 200 行，却能避免为一堆 `// indirect` 依赖再引入一层
// 版本约束；且 /metrics 的输出格式是公开协议，换成官方 SDK 时只需替换本文件。
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 指标类型
const (
	TypeCounter   = "counter"
	TypeGauge     = "gauge"
	TypeHistogram = "histogram"
)

// LatencyBuckets 默认时延分桶（秒）：覆盖 5ms~10s，足够定位「慢在 LLM 还是 MCP」。
var LatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Labels 标签集合（键值对，序列化时按键排序保证稳定）
type Labels map[string]string

// series 一条时间序列
type series struct {
	name   string
	labels Labels
	kind   string
	value  float64
	// 直方图专用
	counts []uint64
	sum    float64
	count  uint64
}

// Registry 指标注册表（并发安全）
type Registry struct {
	mu     sync.RWMutex
	meta   map[string][2]string // name → {help, type}
	series map[string]*series   // name + labels → series
}

// NewRegistry 新建注册表
func NewRegistry() *Registry {
	return &Registry{
		meta:   make(map[string][2]string, 32),
		series: make(map[string]*series, 64),
	}
}

// Default 进程级默认注册表
var Default = NewRegistry()

// Describe 声明指标元信息（help 与类型）。重复声明同名同类型是幂等的。
func (r *Registry) Describe(name, help, kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta[name] = [2]string{help, kind}
}

func seriesKey(name string, l Labels) string {
	if len(l) == 0 {
		return name + "{}"
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strconv.Quote(l[k]))
	}
	b.WriteString("}")
	return b.String()
}

func (r *Registry) get(name string, l Labels, kind string) *series {
	// 调用方需持有写锁
	key := seriesKey(name, l)
	s, ok := r.series[key]
	if !ok {
		cp := make(Labels, len(l))
		for k, v := range l {
			cp[k] = v
		}
		s = &series{name: name, labels: cp, kind: kind}
		if kind == TypeHistogram {
			s.counts = make([]uint64, len(LatencyBuckets))
		}
		r.series[key] = s
	}
	if _, ok := r.meta[name]; !ok {
		r.meta[name] = [2]string{"", kind}
	}
	return s
}

// Add 计数器累加（负值忽略）
func (r *Registry) Add(name string, l Labels, delta float64) {
	if delta == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(name, l, TypeCounter).value += delta
}

// Inc 计数器 +1，等价于 Add(name, l, 1)
func (r *Registry) Inc(name string, l Labels) { r.Add(name, l, 1) }

// Set 仪表赋值
func (r *Registry) Set(name string, l Labels, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(name, l, TypeGauge).value = v
}

// AddGauge 仪表增减
func (r *Registry) AddGauge(name string, l Labels, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(name, l, TypeGauge).value += delta
}

// Observe 直方图观测（单位：秒）
func (r *Registry) Observe(name string, l Labels, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(name, l, TypeHistogram)
	for i, b := range LatencyBuckets {
		if seconds <= b {
			s.counts[i]++
		}
	}
	s.sum += seconds
	s.count++
}

// ObserveSince 便捷方法：观测从 start 到现在的耗时
func (r *Registry) ObserveSince(name string, l Labels, start time.Time) {
	r.Observe(name, l, time.Since(start).Seconds())
}

// WriteText 按 Prometheus 文本格式输出全部指标。
func (r *Registry) WriteText(w io.Writer) {
	r.mu.RLock()
	// 快照，避免长时间持锁
	type snap struct {
		meta [2]string
		s    series
	}
	list := make([]snap, 0, len(r.series))
	for _, s := range r.series {
		cp := *s
		if s.counts != nil {
			cp.counts = append([]uint64(nil), s.counts...)
		}
		list = append(list, snap{meta: r.meta[s.name], s: cp})
	}
	r.mu.RUnlock()

	sort.Slice(list, func(i, j int) bool {
		if list[i].s.name != list[j].s.name {
			return list[i].s.name < list[j].s.name
		}
		return seriesKey(list[i].s.name, list[i].s.labels) < seriesKey(list[j].s.name, list[j].s.labels)
	})

	lastName := ""
	for _, it := range list {
		s := it.s
		if s.name != lastName {
			if it.meta[0] != "" {
				fmt.Fprintf(w, "# HELP %s %s\n", s.name, it.meta[0])
			}
			fmt.Fprintf(w, "# TYPE %s %s\n", s.name, it.meta[1])
			lastName = s.name
		}
		switch s.kind {
		case TypeHistogram:
			for i, b := range LatencyBuckets {
				fmt.Fprintf(w, "%s_bucket%s %d\n", s.name, withLE(s.labels, b), s.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", s.name, withLE(s.labels, math.Inf(1)), s.count)
			fmt.Fprintf(w, "%s_sum%s %s\n", s.name, renderLabels(s.labels), formatFloat(s.sum))
			fmt.Fprintf(w, "%s_count%s %d\n", s.name, renderLabels(s.labels), s.count)
		default:
			fmt.Fprintf(w, "%s%s %s\n", s.name, renderLabels(s.labels), formatFloat(s.value))
		}
	}
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func renderLabels(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strconv.Quote(l[k]))
	}
	b.WriteString("}")
	return b.String()
}

func withLE(l Labels, le float64) string {
	merged := make(Labels, len(l)+1)
	for k, v := range l {
		merged[k] = v
	}
	merged["le"] = formatFloat(le)
	return renderLabels(merged)
}

// Handler GET /metrics
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		r.WriteText(w)
	})
}
