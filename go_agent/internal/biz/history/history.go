package history

// 统一操作历史用例：
//   - Record：写入口。各 biz（mcp / memory / rag / skill）持有 Recorder 接口，
//     在调用 / 召回完成后异步落一条历史；任务上下文由 WithTask 放进 context 自动取出。
//   - List / Stats：查询面（service 层透传给前端）。
//
// 写路径与 llm_calls 一致：goroutine 异步、失败仅告警，不影响业务主流程。

import (
	"context"
	"time"

	"github.com/go-kratos/kratos/v3/log"
)

// Recorder 历史记录接口（消费方依赖此接口，wire 绑定到 *UseCase）
type Recorder interface {
	Record(ctx context.Context, rec *Record)
}

// ctxMetaKey context 元数据键
type ctxMetaKey struct{}

// ctxMeta 任务上下文元数据
type ctxMeta struct {
	taskID   uint64
	agentKey string
	convID   string
}

// WithTask 把任务身份放进 context：AgentFactory 执行任务时塞入，
// 任务内的每次 MCP 调用 / 技能使用 / 各类召回自动关联到该任务。
// 读改写：保留 ctx 里已有的会话身份（与 WithConv 可叠加，不互相覆盖）。
func WithTask(ctx context.Context, taskID uint64, agentKey string) context.Context {
	m := ctxMeta{taskID: taskID, agentKey: agentKey}
	if old, ok := ctx.Value(ctxMetaKey{}).(ctxMeta); ok {
		m.convID = old.convID
	}
	return context.WithValue(ctx, ctxMetaKey{}, m)
}

// WithConv 把会话身份放进 context：会话编排（聊天）时塞入，
// 会话内的各类召回 / 工具调用自动关联到该会话（会话链路时间线用）。
// 与 WithTask 可叠加：Agent 委派产生的记录同时带 task 与 conv。
func WithConv(ctx context.Context, convID string) context.Context {
	if m, ok := ctx.Value(ctxMetaKey{}).(ctxMeta); ok {
		m.convID = convID
		return context.WithValue(ctx, ctxMetaKey{}, m)
	}
	return context.WithValue(ctx, ctxMetaKey{}, ctxMeta{convID: convID})
}

// MetaFrom 取出任务上下文（没有则返回零值 = 控制台手动调用）
func MetaFrom(ctx context.Context) (taskID uint64, agentKey string) {
	if m, ok := ctx.Value(ctxMetaKey{}).(ctxMeta); ok {
		return m.taskID, m.agentKey
	}
	return 0, ""
}

// ConvFrom 取出会话上下文（没有则返回空 = 会话外调用）
func ConvFrom(ctx context.Context) string {
	if m, ok := ctx.Value(ctxMetaKey{}).(ctxMeta); ok {
		return m.convID
	}
	return ""
}

// UseCase 统一操作历史用例
type UseCase struct {
	repo Repo
}

var _ Recorder = (*UseCase)(nil)

// New 创建用例（repo 不可缺；写路径失败仅告警）
func New(repo Repo) *UseCase {
	return &UseCase{repo: repo}
}

// Record 异步写入一条历史：
//   - rec.TaskID / rec.AgentKey 未显式指定时，自动从 context 取（WithTask 塞入的）；
//   - CreatedAt 缺省补 now；
//   - 任何错误只告警，不返回。
func (u *UseCase) Record(ctx context.Context, rec *Record) {
	if u == nil || u.repo == nil || rec == nil || rec.Kind == "" {
		return
	}
	if rec.TaskID == 0 || rec.AgentKey == "" {
		if taskID, agentKey := MetaFrom(ctx); taskID > 0 {
			if rec.TaskID == 0 {
				rec.TaskID = taskID
			}
			if rec.AgentKey == "" {
				rec.AgentKey = agentKey
			}
		}
	}
	if rec.ConvID == "" {
		rec.ConvID = ConvFrom(ctx)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	go func(c context.Context, r Record) {
		// 脱离请求生命周期：用独立超时，避免取消涟漪丢记录
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c
		if e := u.repo.Insert(ctx2, &r); e != nil {
			log.Warn("[history] 操作历史写入失败", "kind", r.Kind, "err", e)
		}
	}(ctx, *rec)
}

// List 查询历史（时间倒序 + 总数）
func (u *UseCase) List(ctx context.Context, f Filter) ([]Record, int64, error) {
	if !ValidateKind(f.Kind) {
		return nil, 0, ErrInvalidKind
	}
	return u.repo.List(ctx, f)
}

// Stats 全局统计
func (u *UseCase) Stats(ctx context.Context) (Stats, error) {
	return u.repo.Stats(ctx)
}

// ErrInvalidKind 非法 kind
var ErrInvalidKind = errInvalidKind{}

type errInvalidKind struct{}

func (errInvalidKind) Error() string { return "history: 非法的 kind 类型" }
