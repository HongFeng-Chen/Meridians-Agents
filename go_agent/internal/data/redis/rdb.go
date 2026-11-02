// Package redis 集中所有 Redis 访问：连接管理（RDBData）与各业务缓存实现。
// 目录名 redis、包名 redis；第三方 go-redis 客户端统一以别名 goredis 引入。
package redis

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/conf"

	goredis "github.com/redis/go-redis/v9"
)

// RDBData 持有共享的 Redis 客户端
type RDBData struct {
	rdb *goredis.Client
}

// Client 返回底层 redis 客户端（供各 cache 使用）
func (d *RDBData) Client() *goredis.Client { return d.rdb }

// NewRDBData 建立 Redis 连接（启动时 Ping 一次，失败即报错）
func NewRDBData(c *conf.Data) (*RDBData, func(), error) {
	rc := c.GetRedis()
	readTimeout := rc.GetReadTimeout().AsDuration()
	if readTimeout <= 0 {
		readTimeout = time.Second
	}
	writeTimeout := rc.GetWriteTimeout().AsDuration()
	if writeTimeout <= 0 {
		writeTimeout = time.Second
	}
	dialTimeout := rc.GetDialTimeout().AsDuration()
	if dialTimeout <= 0 {
		dialTimeout = 3 * time.Second
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:         rc.GetAddr(),
		Password:     rc.GetPassword(),
		DB:           int(rc.GetDb()),
		DialTimeout:  dialTimeout,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("redis 连接失败 addr=%s%s: %w", rc.GetAddr(), redisHint(err), err)
	}
	return &RDBData{rdb: rdb}, func() {
		_ = rdb.Close()
	}, nil
}

// redisHint 把 go-redis 的原始报错翻译成可操作的提示，
// 否则 NOAUTH 这类错误只会显示 "NOAUTH Authentication required."，看不出是 Redis。
func redisHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "NOAUTH"), strings.Contains(msg, "WRONGPASS"):
		return "（Redis 要求密码：检查 data.redis.password / REDIS_PASSWORD，需与 redis-server --requirepass 一致）"
	case strings.Contains(msg, "refused"), strings.Contains(msg, "no such host"):
		return "（Redis 未启动或地址不对：检查 data.redis.addr）"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "（Redis 响应超时：检查网络与 data.redis.dialTimeout）"
	default:
		return ""
	}
}
