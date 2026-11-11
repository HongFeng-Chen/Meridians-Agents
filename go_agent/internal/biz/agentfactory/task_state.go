package agentfactory

// 任务状态机（P1-05 / V2-P2）。
//
// 背景：状态迁移原来由各业务函数「先读状态、再决定、最后写库」完成，
// 读与写之间没有任何约束。典型竞态：
//
//	请求 A：读到 pending
//	Worker：ClaimTask（pending → running），开始执行并产生外部副作用
//	请求 A：按「pending 取消」分支走 FinishTask → 库里的 running 被改成 canceled
//
// 结果：任务显示已取消，副作用却已经发生，事后无从追溯。
//
// 现在所有跨状态迁移都必须带「期望的源状态」下推到 SQL（CAS）：
//
//	UPDATE agent_tasks SET status = ? WHERE id = ? AND status IN (?)
//
// 影响行数为 0 就说明状态已经变了，调用方重新读一次再决策，而不是覆盖别人的结论。

// taskEdges 允许的状态迁移（有向边）。终态没有出边。
//
//	pending     → running（认领） / canceled（未开始就取消） / failed（定义已删除，无法执行）
//	running     → succeeded / failed / canceled / timeout（超时） / retry_wait（租约过期或允许重试） / paused
//	retry_wait  → pending（退避到期重新入队） / canceled / failed（重试次数用尽 → 死信）
//	paused      → pending（批准续跑） / failed（驳回/超时） / canceled（挂起期取消）
//
// 「重新入队」必须经过 retry_wait 而不是直接回 pending：直接回 pending 会让
// attempt 计数失去意义，死循环的任务永远撞不到上限。
var taskEdges = map[string][]string{
	TaskPending:   {TaskRunning, TaskCanceled, TaskFailed},
	TaskRunning:   {TaskSucceeded, TaskFailed, TaskCanceled, TaskTimeout, TaskRetryWait, TaskPaused},
	TaskRetryWait: {TaskPending, TaskCanceled, TaskFailed},
	TaskPaused:    {TaskPending, TaskFailed, TaskCanceled},
}

// CanTransit 状态迁移是否合法（业务侧自检；数据库侧仍以 CAS 为准）
func CanTransit(from, to string) bool {
	for _, next := range taskEdges[from] {
		if next == to {
			return true
		}
	}
	return false
}

// InPool 该状态的任务还在任务池表（agent_tasks）里，而不是历史表。
//
// 仓储的 ListTasks 用它决定查哪张表。漏掉一个「池内状态」不只是查不到数据：
// 会走到 default 分支报「未知任务状态」，调用方（审批超时扫描）每次扫描都失败，
// 表现为挂起任务永远不超时。retry_wait 同理——漏掉它，前端「重试等待」页签
// 会报 400，清扫协程也没有任务可捞。
func InPool(status string) bool {
	switch status {
	case TaskPending, TaskRunning, TaskRetryWait, TaskPaused:
		return true
	}
	return false
}

// TransitableFrom 反查某个目标状态允许从哪些状态迁入（CAS 的 from 集合）。
// 终态（succeeded/failed/canceled/timeout）不在此列——写到终态一律以「当前状态」为准，
// 由调用方显式给出，避免「任何状态都能变成功」。
func TransitableFrom(to string) []string {
	out := make([]string, 0, 3)
	for from, nexts := range taskEdges {
		for _, n := range nexts {
			if n == to {
				out = append(out, from)
				break
			}
		}
	}
	return out
}
