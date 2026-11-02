package models

import "strconv"

// agentfactory 键空间
//
//	af:agent:{agentID}     Agent 定义缓存（TTL 30min 兜底）
//	af:task:{taskID}       任务运行快照（只缓存活跃任务，实时状态轮询）
//	af:queue:tasks         任务队列（List：LPUSH 入队 / BRPOP 出队）
//	af:lock:task:{taskID}  任务执行分布式锁（Redlock，值 = 随机 token）
const (
	AgentPrefix    = "af:agent:"
	TaskPrefix     = "af:task:"
	TaskQueueKey   = "af:queue:tasks"
	TaskLockPrefix = "af:lock:task:"
	AgentTTL       = 30 * 60 // 秒
	ActiveTaskTTL  = 3600    // 秒，活跃任务快照兜底过期
)

func AgentKey(id uint64) string { return AgentPrefix + strconv.FormatUint(id, 10) }
func TaskKey(id uint64) string  { return TaskPrefix + strconv.FormatUint(id, 10) }

// TaskLockKey 任务执行锁键
func TaskLockKey(id uint64) string {
	return TaskLockPrefix + strconv.FormatUint(id, 10)
}
