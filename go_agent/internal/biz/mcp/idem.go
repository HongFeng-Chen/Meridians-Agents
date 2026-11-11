package mcp

// 外部副作用工具调用的幂等。
//
// 问题：任务重试链路（失联回收 → retry_wait → pending → 重新认领）会把任务
// 从 checkpoint 恢复后重跑。LLM 决定「重发刚才那次工具调用」时，如果上一次
// 调用其实已经执行成功（进程死在「工具已执行、结果没来得及落 checkpoint」
// 的窗口里），同一个副作用就会发生两次：重复下单、重复发消息、重复建资源。
//
// 方案：任务执行链路里每次工具调用都带一个**幂等键**（任务 + 轮次 + 工具 +
// 参数摘要）。mcp.UseCase.CallTool 执行前先查记录，命中且成功就直接回放
// 上次的结果，不再真正调用；执行成功后写记录。
//
// 语义边界（有意为之）：
//   - 失败不缓存：调用报错时副作用状态未知，重试就是要再试一次；
//     IsError 结果同理——工具端报错不等于没执行，但缓存一个错误回放只会
//     让任务永远停在同一个错上，不如让下一轮尝试有翻身的可能。
//   - attempt 不参与 key：重试是「同一次逻辑调用」的再执行，key 必须
//     跨 attempt 稳定才能拦住重复副作用。
//   - 只拦任务执行链路：管理界面手动调试、会话编排直连等不带 key 的调用
//     不去重——人手动点两次「调用」就是要执行两次。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// idemKeyCtx 幂等键的 context 载体
type idemKeyCtx struct{}

// WithIdemKey 给本次调用带上幂等键。空键等于不带（不去重）。
func WithIdemKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, idemKeyCtx{}, key)
}

// IdemKeyFrom 读取调用方注入的幂等键（没有则为空串）
func IdemKeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(idemKeyCtx{}).(string)
	return key
}

// TaskCallKey 任务执行链路的幂等键。
//
// 任务 ID 是自增主键，天然全局唯一，租户之间不可能撞键；
// turn 恢复自 checkpoint（RunState.Turn），同一逻辑轮次在续跑后编号不变，
// 这正是「恢复后 LLM 重发同一调用」能被拦住的前提。
func TaskCallKey(taskID uint64, turn int, uniqueName string, args map[string]any) string {
	return fmt.Sprintf("task:%d:turn:%d:%s:%s", taskID, turn, uniqueName, ArgsHash(args))
}

// ArgsHash 参数摘要（sha256 前 16 位）。encoding/json 对 map 按 key 排序输出，
// 同一参数无论构造顺序如何都得到同一串。
func ArgsHash(args map[string]any) string {
	sum := sha256.Sum256([]byte(canonicalArgs(args)))
	return hex.EncodeToString(sum[:])[:16]
}

// canonicalArgs 参数的稳定序列化。encoding/json 对 map 按 key 排序输出，
// 同一参数无论构造顺序如何都得到同一串。
func canonicalArgs(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		// 无法序列化的参数（含 chan/func 等真实场景不会出现的值）退化为 fmt
		return fmt.Sprintf("%v", args)
	}
	return string(b)
}

// IdemRecord 一次已执行调用的记录（命中即回放 Result，不真正调用）
type IdemRecord struct {
	IdemKey string
	TaskID  uint64
	Tool    string
	// ArgsHash 参数摘要（观测用：排查「为什么这个 key 没拦住」时先对它）
	ArgsHash string
	// OK 只有成功的调用才落记录（见包注释的语义边界）
	OK        bool
	Result    *CallResult
	CreatedAt time.Time
}

// IdemStore 幂等记录存储（gorm 实现见 data/db/repo/mcp.go，表 mcp_call_records）
type IdemStore interface {
	// GetCallRecord 按 key 精确查询；不存在返回 (nil, nil)
	GetCallRecord(ctx context.Context, key string) (*IdemRecord, error)
	// SaveCallRecord 写入记录。并发下两个执行体同时写同一 key：
	// 靠 idem_key 唯一索引兜底，后到的一方覆盖（结果等价，多执行一次
	// 的副作用已经发生，这里只保证账面一致）。
	SaveCallRecord(ctx context.Context, rec *IdemRecord) error
}
