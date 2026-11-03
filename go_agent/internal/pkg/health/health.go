// Package health 探活与就绪检查：把各存储组件的 ping 聚合成一个 HTTP 端点。
//
// 设计要点：注册表是进程级的，存储层在自己的构造函数里注册检查项
// （db.NewData / redis.NewRDBData / milvusdb.New），server 层只暴露 Handler，
// 这样不用为了探活把三个存储句柄一路 wire 到 transport 层。
//
//	GET /healthz  → 200 {"status":"ok","checks":{...}}   全部通过
//	              → 503 {"status":"degraded",...}        任一失败（供 LB 摘流）
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Check 单项检查：返回 nil 表示健康。
type Check func(ctx context.Context) error

type entry struct {
	name  string
	check Check
}

var (
	mu       sync.RWMutex
	registry []entry
	started  = time.Now()
)

// Register 注册检查项（同名后注册覆盖前者）。
func Register(name string, check Check) {
	if check == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range registry {
		if registry[i].name == name {
			registry[i].check = check
			return
		}
	}
	registry = append(registry, entry{name: name, check: check})
}

// Reset 清空注册表（单测用）。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	registry = nil
	started = time.Now()
}

type result struct {
	Status    string  `json:"status"`
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

type report struct {
	Status  string            `json:"status"`
	UptimeS int64             `json:"uptime_s"`
	Checks  map[string]result `json:"checks"`
}

// Probe 并发执行全部检查，返回整体是否健康与明细。
func Probe(ctx context.Context, timeout time.Duration) (bool, report) {
	mu.RLock()
	list := append([]entry(nil), registry...)
	mu.RUnlock()

	rep := report{Status: "ok", UptimeS: int64(time.Since(started).Seconds()), Checks: map[string]result{}}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	var wg sync.WaitGroup
	var rmu sync.Mutex
	for _, e := range list {
		wg.Add(1)
		go func(e entry) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			begin := time.Now()
			err := e.check(cctx)
			r := result{Status: "ok", LatencyMS: float64(time.Since(begin).Microseconds()) / 1000.0}
			if err != nil {
				r.Status = "down"
				r.Error = err.Error()
			}
			rmu.Lock()
			rep.Checks[e.name] = r
			rmu.Unlock()
		}(e)
	}
	wg.Wait()

	for _, r := range rep.Checks {
		if r.Status != "ok" {
			rep.Status = "degraded"
			break
		}
	}
	return rep.Status == "ok", rep
}

// Handler GET /healthz
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, rep := Probe(r.Context(), 2*time.Second)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		code := http.StatusOK
		if !ok {
			code = http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(rep)
	})
}
