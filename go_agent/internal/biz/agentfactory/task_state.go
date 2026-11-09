package agentfactory

// 任务状态机（P1-05）。
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
//	pending  → running（认领） / canceled（未开始就取消）
//	running  → succeeded / failed / canceled / paused
//	paused   → pending（批准续跑） / failed（驳回） / canceled（挂起期取消）
var taskEdges = map[string][]string{
	TaskPending: {TaskRunning, TaskCanceled},
	TaskRunning: {TaskSucceeded, TaskFailed, TaskCanceled, TaskPaused},
	TaskPaused:  {TaskPending, TaskFailed, TaskCanceled},
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

// TransitableFrom 反查某个目标状态允许从哪些状态迁入（CAS 的 from 集合）。
// 终态（succeeded/failed/canceled）不在此列——写到终态一律以「当前状态」为准，
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
