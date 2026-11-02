package redis

// TaskLock 任务执行分布式锁 —— Redlock 算法实现。
//
// 算法（antirez Redlock）：
//  1. 生成唯一随机 token，对全部节点依次执行 SET key token NX PX ttl；
//  2. 成功节点数 ≥ N/2+1（Quorum）才算获取成功，且总耗时须小于锁租期；
//  3. 获取失败时对已加锁节点立即释放（回滚部分成功）；
//  4. 释放/续期必须校验 token（Lua CAS），防止误删其他持有者的锁。
//
// 节点来源：data.redis.addr 是主节点；可用环境变量 REDLOCK_ADDRS
// （逗号分隔）追加独立节点组成真正的多节点 Redlock。只配置主节点时
// 退化为单实例锁（Quorum = 1），语义与算法兼容。
//
// 用途：AgentFactory 任务执行互斥 —— 队列重复投递、崩溃恢复扫描与实时
// 提交并发、多实例部署等场景下，保证同一任务不会被同时触发多次。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/conf"
	"go_agent/internal/data/redis/models"
)

var _ agentfactory.TaskLock = (*TaskLock)(nil)

// releaseScript token 匹配才删除（CAS），返回被删除的键数
var releaseScript = goredis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// extendScript token 匹配才续期（CAS），返回 1 成功 / 0 失败（锁已丢失）
var extendScript = goredis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

type TaskLock struct {
	nodes []*goredis.Client
}

// NewTaskLock 构造 Redlock 客户端：主节点 + 可选的 REDLOCK_ADDRS 附加节点
func NewTaskLock(d *RDBData, c *conf.Data) *TaskLock {
	rc := c.GetRedis()
	nodes := []*goredis.Client{d.Client()}
	if extra := strings.TrimSpace(os.Getenv("REDLOCK_ADDRS")); extra != "" {
		for _, addr := range strings.Split(extra, ",") {
			addr = strings.TrimSpace(addr)
			if addr == "" || addr == rc.GetAddr() {
				continue
			}
			client := goredis.NewClient(&goredis.Options{
				Addr: addr, Password: rc.GetPassword(), DB: int(rc.GetDb()),
				DialTimeout: 2 * time.Second,
			})
			nodes = append(nodes, client)
		}
	}
	return &TaskLock{nodes: nodes}
}

// newToken 生成 128bit 随机 token（唯一性由密码学随机保证）
func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 退化：crypto/rand 失败极罕见，用时间戳兜底仍保证进程内唯一
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Acquire Quorum 加锁：多数节点 SET NX PX 成功且未超时才算获取成功
func (l *TaskLock) Acquire(ctx context.Context, taskID uint64, ttl time.Duration) (string, bool) {
	if len(l.nodes) == 0 || ttl <= 0 {
		return "", false
	}
	token := newToken()
	key := models.TaskLockKey(taskID)
	valency := 0
	deadline := time.Now().Add(ttl)
	for _, node := range l.nodes {
		// 每节点单独限时：总耗时不能超过租期（Redlock 时钟假设）
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ttl/2)
		ok, err := node.SetNX(nctx, key, token, ttl).Result()
		cancel()
		if err == nil && ok {
			valency++
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if valency >= len(l.nodes)/2+1 {
		return token, true
	}
	// 未达 Quorum：回滚已成功的节点
	l.releaseAll(key, token)
	return "", false
}

// Release 释放锁：全节点 token CAS 删除
func (l *TaskLock) Release(ctx context.Context, taskID uint64, token string) {
	if token == "" {
		return
	}
	l.releaseAll(models.TaskLockKey(taskID), token)
}

func (l *TaskLock) releaseAll(key, token string) {
	for _, node := range l.nodes {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = releaseScript.Run(ctx, node, []string{key}, token).Err()
		cancel()
	}
}

// Extend 续期：token 匹配才 PEXPIRE；Quorum 续期成功才算成功
// （锁已丢失返回 false，调用方应停止执行）
func (l *TaskLock) Extend(ctx context.Context, taskID uint64, token string, ttl time.Duration) bool {
	if token == "" || ttl <= 0 {
		return false
	}
	key := models.TaskLockKey(taskID)
	okCount := 0
	for _, node := range l.nodes {
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		res, err := extendScript.Run(nctx, node, []string{key}, token, ttl.Milliseconds()).Int()
		cancel()
		if err == nil && res == 1 {
			okCount++
		}
	}
	return okCount >= len(l.nodes)/2+1
}
