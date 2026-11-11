package harness

import (
	"context"
	"time"

	"go_agent/internal/biz/llm"
)

// 端口：接口定义在 biz，实现在 data / 其他 biz 包（与 agentfactory.ports.go 同风格）。
// wire 接线时：Repo → data/db/repo，Runner → agentfactory 适配器，Chatter → llm.Manager。

// Repo MySQL 持久化：用例集、用例、运行与逐例结果
type Repo interface {
	// —— 用例集 ——
	ListSets(ctx context.Context, f SetFilter) ([]Set, error)
	GetSet(ctx context.Context, id uint64) (*Set, error)
	CreateSet(ctx context.Context, s *Set) error
	SaveSet(ctx context.Context, s *Set) error
	DeleteSet(ctx context.Context, id uint64) error
	CountCases(ctx context.Context, setID uint64, enabledOnly bool) (int, error)

	// —— 用例 ——
	ListCases(ctx context.Context, setID uint64, enabledOnly bool) ([]Case, error)
	GetCase(ctx context.Context, id uint64) (*Case, error)
	CreateCase(ctx context.Context, c *Case) error
	SaveCase(ctx context.Context, c *Case) error
	DeleteCase(ctx context.Context, id uint64) error

	// —— 运行 ——
	CreateRun(ctx context.Context, r *Run) error
	// SaveRun 全量保存（进度推进即调用；列数少，不做局部 UPDATE）
	SaveRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id uint64) (*Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)
	DeleteRun(ctx context.Context, id uint64) error

	// —— 逐例结果 ——
	CreateItem(ctx context.Context, it *RunItem) error
	ListItems(ctx context.Context, runID uint64) ([]RunItem, error)
	// DeleteItemsOfRun 清空某次运行的逐例结果（重跑前调用）
	DeleteItemsOfRun(ctx context.Context, runID uint64) error
}

// Runner 被测对象执行器（黑盒）：harness 不关心被测对象内部怎么跑。
//
// 实现（data 层适配器）按 target 分派：
//   - agent:<key>  → agentfactory.UseCase 同步执行一次（复用执行图与能力白名单，
//     产生的 trace 事件 / LLM 调用记录自动关联，链路完整可查）；
//   - model:<name> → llm.Manager 指定模型直连对话（模型级横评）。
type Runner interface {
	Run(ctx context.Context, target, input string) (string, error)
	// Resolve 解析被测对象的版本信息（快照 / 模型）。
	//
	// 放在 Runner 上而不是 harness 里：target 的语义只有执行器知道
	// （agent: 要去 agentfactory 查最新快照，model: 直接就是模型名）。
	// 解析失败时返回零值，不阻断运行——版本绑定是「尽可能记全」，
	// 不是评测能否开跑的前置条件。
	Resolve(ctx context.Context, target string) TargetInfo
}

// Chatter LLM 对话（绑定 llm.Manager）：LLM-as-Judge 用，签名与 agentfactory.Chatter 一致
type Chatter interface {
	Chat(ctx context.Context, system, user string) (string, error)
	ChatTemp(ctx context.Context, system, user string, temperature float64) (string, error)
	// ChatOpts 统一参数通道（JSON 模式 / max_tokens），与 agentfactory.Chatter 一致
	ChatOpts(ctx context.Context, o llm.ChatOptions) (string, error)
	Model() string
}

// clock 便于测试注入
type clock func() time.Time

// ScoreRecorder 提示词版本得分回写（实现：prompt.UseCase.RecordPromptScore）。
// RunConfig 带 prompt_key/prompt_version 标注时，运行终态把得分回写提示词工程。
type ScoreRecorder interface {
	RecordPromptScore(ctx context.Context, key string, version int, runID uint64, score float64, cases int) error
}
