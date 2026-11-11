// cost.go 成本记账端口：每次 LLM 调用结束把「用了多少 token、折多少钱」交给成本中心。
//
// 为什么不直接依赖 usage 包：llm 是底层能力（rag / agentfactory / prompt 都依赖它），
// usage 是上层业务（有 principal、租户校验）。让底层反向依赖上层会把两个包焊死。
// 所以这里只定义端口，实现在 data 层（见 repo/usage_sink.go）。
//
// 与 CallRepo 的分工：
//   - CallRepo 记「调用历史」——含 prompt 原文，用于排查「模型当时看到了什么」；
//   - CostSink 记「账单」——tokens 与成本，用于按租户 / Agent / 任务聚合。
//
// 两者都写会让存储翻倍，但用途完全不同，不能互相替代：
// 历史可以按天清理，账单必须长期可查。
package llm

import (
	"context"
	"fmt"
	"strconv"
)

// CostEvent 一次 LLM 调用的计费事实（不含原文，只有数字）。
type CostEvent struct {
	TenantID string
	UserID   string
	// AgentKey 关联 Agent（来自 CallMeta；直连调用为空）
	AgentKey string
	// TaskID 关联任务（任务链路才有）：单任务成本据此聚合
	TaskID uint64

	Provider string
	Model    string
	Purpose  string
	OK       bool

	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	// CostMicros 本次成本（微元，1 元 = 1_000_000）：整数运算，避免浮点累加误差
	CostMicros int64
	Currency   string
	// PriceVersion 计价规则版本：模型调价后旧账单不能被新价格重算
	PriceVersion string
}

// CostSink 成本记账端口（可为 nil：未接线时只打指标不落库）。
type CostSink interface {
	RecordCost(ctx context.Context, ev CostEvent) error
}

// PriceVersion 由单价生成计价版本标识（如 "in=2.0000/out=8.0000"）。
//
// 用它而不是人工版本号：改配置里的单价，版本自动变，历史账单自然分组；
// 忘了升版本号导致「新价格重算历史账单」是成本系统最经典的错。
func PriceVersion(priceIn, priceOut float64) string {
	return "in=" + strconv.FormatFloat(priceIn, 'f', 4, 64) +
		"/out=" + strconv.FormatFloat(priceOut, 'f', 4, 64)
}

// CostMicros 按「元 / 百万 token」单价算成本（微元）。
//
// 推导：tokens / 1e6 * 单价 = 元；元 * 1e6 = 微元 → tokens * 单价 = 微元。
// 所以不必先除再乘，直接整数域相乘，规避浮点精度问题。
func CostMicros(promptTokens, completionTokens int64, priceIn, priceOut float64) int64 {
	v := float64(promptTokens)*priceIn + float64(completionTokens)*priceOut
	if v <= 0 {
		return 0
	}
	return int64(v)
}

// String 便于日志
func (e CostEvent) String() string {
	return fmt.Sprintf("%s/%s task=%d tokens=%d cost=%dμ",
		e.Provider, e.Model, e.TaskID, e.TotalTokens, e.CostMicros)
}
