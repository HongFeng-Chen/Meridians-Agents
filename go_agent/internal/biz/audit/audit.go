// Package audit 审计事件：把「谁在什么时候对什么做了什么、结果如何」落到
// 可查询的表里，而不是散落在日志里。
//
// 为什么不能只有日志：
//   - 日志是给排错用的，没有稳定的字段结构，无法按「用户 × 动作 × 结果」检索；
//   - 安全事件（越权被拒、高危工具调用、技能安装）需要留存与追溯，
//     日志轮转后就没了；
//   - 出了问题要回答的是「上个月谁改过 MCP 配置」，不是「grep 一下关键词」。
//
// 写入方式：异步批量。审计是横切关注点，不能让它拖慢或搞挂业务请求——
// Record 只做「塞进缓冲通道」，通道满了就丢并计数（丢的量进日志告警），
// 后台协程按批 flush 到 MySQL。审计丢几条比业务 500 可接受。
package audit

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go_agent/internal/pkg/identity"

	"github.com/go-kratos/kratos/v3/log"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
)

// 结果取值
const (
	ResultAllowed = "allowed"
	ResultDenied  = "denied"
	ResultError   = "error"
)

// Event 待记录的审计事件（身份与来源字段由 Record 从 ctx 补齐，调用方不用填）
type Event struct {
	// Action 动作标识，建议用「域.对象.动词」：mcp.server.create / skill.install
	Action string
	// Result allowed | denied | error
	Result string
	// Resource 资源类型（mcp_server / tool / skill / agent_def / auth）
	Resource string
	// ResourceID 资源标识（id 或唯一名）
	ResourceID string
	// Risk 风险等级（工具调用等场景）
	Risk string
	// Detail 附加信息，落库为 JSON（建议只放短字段，别塞全文）
	Detail map[string]any
	// LatencyMS 操作耗时（可选）
	LatencyMS int64
}

// Record 落库结构
type Record struct {
	ID         uint64    `json:"id"`
	Ts         time.Time `json:"ts"`
	Action     string    `json:"action"`
	Result     string    `json:"result"`
	Resource   string    `json:"resource,omitempty"`
	ResourceID string    `json:"resource_id,omitempty"`
	Risk       string    `json:"risk,omitempty"`
	UserID     string    `json:"user_id"`
	TenantID   string    `json:"tenant_id"`
	AuthType   string    `json:"auth_type"`
	Admin      bool      `json:"admin"`
	Local      bool      `json:"local"`
	IP         string    `json:"ip,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	TraceID    string    `json:"trace_id,omitempty"`
	LatencyMS  int64     `json:"latency_ms,omitempty"`
}

// Filter 查询条件
type Filter struct {
	Action   string
	Result   string
	Resource string
	UserID   string
	Since    time.Time
	Until    time.Time
	Limit    int
	Offset   int
}

// Repo 审计存储（实现见 data/db/repo/audit.go）
type Repo interface {
	BatchInsert(ctx context.Context, records []Record) error
	List(ctx context.Context, f Filter) ([]Record, error)
	Purge(ctx context.Context, before time.Time) (int64, error)
}

// UseCase 审计用例：异步收集 + 批量写库 + 查询 + 过期清理
type UseCase struct {
	repo Repo
	ch   chan Record
	done chan struct{}

	dropped atomic.Int64
	closed  atomic.Bool
}

const (
	// bufferSize 单进程缓冲的待写事件数
	bufferSize = 4096
	// batchSize 单次落库条数
	batchSize = 200
	// flushInterval 最长等待时间（低峰期也能及时落库）
	flushInterval = 2 * time.Second
)

// New 创建审计用例并启动后台写入协程
//
// 同时注册为进程默认记录器：横切点（授权拒绝、工具调用闸门）在服务方法内部，
// 拿不到依赖注入的实例，与 metrics.Default 的做法一致。
func New(repo Repo) *UseCase {
	u := &UseCase{
		repo: repo,
		ch:   make(chan Record, bufferSize),
		done: make(chan struct{}),
	}
	if repo != nil {
		go u.loop()
		go u.purgeLoop()
	}
	SetDefault(u)
	return u
}

// Close 停止后台协程并把缓冲写完（进程退出时由 wire cleanup 调用）
func (u *UseCase) Close() {
	if u == nil || u.closed.Swap(true) {
		return
	}
	close(u.ch)
	<-u.done
}

// Record 记录一条事件：只入缓冲，不阻塞业务
func (u *UseCase) Record(ctx context.Context, e Event) {
	if u == nil || u.repo == nil || u.closed.Load() {
		return
	}
	rec := buildRecord(ctx, e)
	select {
	case u.ch <- rec:
	default:
		// 缓冲满：宁可丢审计也不拖业务；丢的量定期告警
		if n := u.dropped.Add(1); n%1000 == 1 {
			log.Warn("[audit] 缓冲区已满，审计事件被丢弃", "dropped", n, "action", e.Action)
		}
	}
}

// List 查询审计事件
func (u *UseCase) List(ctx context.Context, f Filter) ([]Record, error) {
	if u == nil || u.repo == nil {
		return nil, nil
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	return u.repo.List(ctx, f)
}

// Dropped 已丢弃的事件数（监控用）
func (u *UseCase) Dropped() int64 {
	if u == nil {
		return 0
	}
	return u.dropped.Load()
}

// loop 批量写库
func (u *UseCase) loop() {
	defer close(u.done)
	buf := make([]Record, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := u.repo.BatchInsert(ctx, buf); err != nil {
			log.Error("[audit] 写审计事件失败", "count", len(buf), "err", err)
		}
		cancel()
		buf = buf[:0]
	}

	for {
		select {
		case rec, ok := <-u.ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, rec)
			if len(buf) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// purgeLoop 按保留期清理旧事件（默认 90 天，AUDIT_RETENTION_DAYS 可调）
func (u *UseCase) purgeLoop() {
	retention := retentionDays()
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-u.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if n, err := u.repo.Purge(ctx, time.Now().Add(-time.Duration(retention)*24*time.Hour)); err != nil {
				log.Warn("[audit] 清理历史审计事件失败", "err", err)
			} else if n > 0 {
				log.Info("[audit] 已清理历史审计事件", "count", n, "retention_days", retention)
			}
			cancel()
		}
	}
}

func retentionDays() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AUDIT_RETENTION_DAYS"))); err == nil && n > 0 {
		return n
	}
	return 90
}

// buildRecord 补齐身份与来源字段
func buildRecord(ctx context.Context, e Event) Record {
	info := identity.From(ctx)
	rec := Record{
		Ts:         time.Now(),
		Action:     strings.TrimSpace(e.Action),
		Result:     strings.TrimSpace(e.Result),
		Resource:   e.Resource,
		ResourceID: e.ResourceID,
		Risk:       e.Risk,
		UserID:     info.UserID,
		TenantID:   info.TenantID,
		AuthType:   info.Auth,
		Admin:      info.Admin,
		Local:      info.Local,
		LatencyMS:  e.LatencyMS,
	}
	if rec.Result == "" {
		rec.Result = ResultAllowed
	}
	if req, ok := khttp.RequestFromServerContext(ctx); ok && req != nil {
		rec.IP = clientIP(req)
		rec.TraceID = strings.TrimSpace(req.Header.Get("X-Request-Id"))
	}
	if len(e.Detail) > 0 {
		if raw, err := json.Marshal(e.Detail); err == nil {
			rec.Detail = string(raw)
			if len([]rune(rec.Detail)) > 2000 {
				rec.Detail = string([]rune(rec.Detail)[:2000]) + "…"
			}
		}
	}
	return rec
}

// clientIP 取真实来源 IP：优先 X-Forwarded-For 第一跳（我们都是反代后部署），
// 其次 RemoteAddr。审计要的是「谁发起」，反代场景下 RemoteAddr 只会是网关。
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return xff
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// —— 进程默认记录器 ——

var (
	defaultMu sync.RWMutex
	defaultUC *UseCase
)

// SetDefault 注册默认记录器（New 内部调用）
func SetDefault(u *UseCase) {
	defaultMu.Lock()
	defaultUC = u
	defaultMu.Unlock()
}

// Default 读取默认记录器（可能为 nil）
func Default() *UseCase {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultUC
}

// Emit 用默认记录器记录事件；未初始化时静默丢弃
func Emit(ctx context.Context, e Event) {
	if u := Default(); u != nil {
		u.Record(ctx, e)
	}
}

// Info 记录一条成功事件
func Info(ctx context.Context, action, resource, resourceID string, detail map[string]any) {
	Emit(ctx, Event{Action: action, Result: ResultAllowed, Resource: resource, ResourceID: resourceID, Detail: detail})
}

// Denied 记录一条被拒事件（越权、策略拦截）
func Denied(ctx context.Context, action, resource, resourceID, reason string) {
	Emit(ctx, Event{Action: action, Result: ResultDenied, Resource: resource, ResourceID: resourceID, Detail: map[string]any{"reason": reason}})
}
