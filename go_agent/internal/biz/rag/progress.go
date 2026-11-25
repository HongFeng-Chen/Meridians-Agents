// progress.go 入库执行进度中心：IngestDocument / IngestUrl 的每个阶段
// （抓取 → 解析 → 清洗 → 分块 → 向量化 → 写 Milvus → 图谱抽取 → 完成）
// 通过 ReportProgress 上报，前端在同步请求进行中轮询 /api/v2/ingest-progress
// 展示步骤条。
//
// 进度放在进程内存即可：入库是同步 RPC，调用方在请求期间轮询，
// 单实例部署天然一致；多实例时需换 Redis（结构已经按 key-value 预留）。
package rag

import (
	"context"
	"strings"
	"sync"
	"time"
)

// IngestProgress 单次入库的进度快照
type IngestProgress struct {
	Step      string `json:"step"`                 // 当前阶段标识（fetch/parse/clean/chunk/embed/milvus/graph/done/error）
	Detail    string `json:"detail"`               // 人读的细节（如「向量化中 12/79 块」）
	Done      bool   `json:"done"`                 // 整体是否结束（成功或失败）
	Error     string `json:"error,omitempty"`      // 失败原因（Done 且 Error 非空）
	UpdatedAt string `json:"updated_at"`           // 最后上报时间
}

type progressEntry struct {
	mu sync.Mutex
	st IngestProgress
}

type progressStore struct {
	mu sync.Mutex
	m  map[string]*progressEntry
}

// IngestProgressStore 全局进度中心（key = 前端生成的 progress_id）
var IngestProgressStore = &progressStore{m: map[string]*progressEntry{}}

func (s *progressStore) set(id, step, detail string, done bool, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		e = &progressEntry{}
		s.m[id] = e
		// 简单防膨胀：超过 1024 个历史进度时清掉已完成的旧条目
		if len(s.m) > 1024 {
			for k, v := range s.m {
				v.mu.Lock()
				finished := v.st.Done
				v.mu.Unlock()
				if finished {
					delete(s.m, k)
				}
			}
		}
	}
	e.mu.Lock()
	e.st = IngestProgress{
		Step:      step,
		Detail:    detail,
		Done:      done,
		Error:     errMsg,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	e.mu.Unlock()
}

// Get 读取进度（不存在时 ok=false）
func (s *progressStore) Get(id string) (IngestProgress, bool) {
	s.mu.Lock()
	e, ok := s.m[id]
	s.mu.Unlock()
	if !ok {
		return IngestProgress{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st, true
}

// ctxKey 进度 ID 的 context 键（私有类型防碰撞）
type ctxKey struct{}

// WithIngestProgress 把进度 ID 绑进 ctx；之后的 ReportProgress 会写进度。
// id 为空（前端没启用进度）时原样返回，所有上报自动变 no-op。
func WithIngestProgress(ctx context.Context, id string) context.Context {
	if strings.TrimSpace(id) == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, strings.TrimSpace(id))
}

// progressID 从 ctx 取进度 ID
func progressID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// ReportProgress 上报当前阶段（未绑定进度 ID 时静默忽略）
func ReportProgress(ctx context.Context, step, detail string) {
	if id := progressID(ctx); id != "" {
		IngestProgressStore.set(id, step, detail, false, "")
	}
}

// FinishIngestProgress 标记入库结束：errMsg 非空 = 失败（保留失败时的阶段标识）。
func FinishIngestProgress(ctx context.Context, errMsg string) {
	id := progressID(ctx)
	if id == "" {
		return
	}
	if errMsg != "" {
		// 保留最后上报的阶段，前端据此把失败步骤标红
		step := "clean"
		if prev, ok := IngestProgressStore.Get(id); ok && prev.Step != "" && prev.Step != "done" {
			step = prev.Step
		}
		IngestProgressStore.set(id, step, "", true, errMsg)
		return
	}
	IngestProgressStore.set(id, "done", "入库完成", true, "")
}
