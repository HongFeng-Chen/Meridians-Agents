package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/conf"
	"go_agent/internal/pkg/health"

	"github.com/go-kratos/kratos/v3/log"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// mysqlHint 给常见连接错误补一句可操作提示（gorm 原始报错只有驱动信息）。
func mysqlHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Access denied"):
		return "（账号或密码错误：检查 data.database.source 里的 user:pass）"
	case strings.Contains(msg, "Unknown database"):
		return "（库不存在：先创建 MYSQL_DATABASE 指定的库，或检查 DSN 末尾的库名）"
	case strings.Contains(msg, "refused"), strings.Contains(msg, "no such host"):
		return "（MySQL 未启动或地址不对：检查 data.database.source 的 host:port）"
	case strings.Contains(msg, "timeout"):
		return "（MySQL 响应超时：检查网络与 timeout 参数）"
	case strings.Contains(msg, "invalid connection"), strings.Contains(msg, "unexpected EOF"):
		// TCP 能建连、握手中途被对端关掉。本机用 Docker 起 MySQL 时最常见的原因是
		// 宿主机端口映射失效（容器重启/休眠唤醒后），容器内 mysql 客户端却一切正常。
		return "（TCP 已连通但握手被中断：多为端口映射失效，docker restart 对应容器；" +
			"容器内 `mysql -h127.0.0.1` 正常即可确认）"
	default:
		return ""
	}
}

// Data holds the long-lived storage clients shared by repos.
type Data struct {
	db *gorm.DB
}

// DB 返回底层 gorm 句柄（供各 repo 使用）
func (d *Data) DB() *gorm.DB { return d.db }

// NewData opens the database client and returns it with a cleanup function.
func NewData(c *conf.Data) (*Data, func(), error) {
	dc := c.GetDatabase()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       dc.Source, // DSN data source name
		DefaultStringSize:         256,       // string 类型字段的默认长度
		DisableDatetimePrecision:  true,      // 禁用 datetime 精度，MySQL 5.6 之前的数据库不支持
		DontSupportRenameIndex:    true,      // 重命名索引时采用删除并新建的方式，MySQL 5.7 之前的数据库和 MariaDB 不支持重命名索引
		DontSupportRenameColumn:   true,      // 用 `change` 重命名列，MySQL 8 之前的数据库和 MariaDB 不支持重命名列
		SkipInitializeWithVersion: false,     // 根据当前 MySQL 版本自动配置
	}), &gorm.Config{})
	if err != nil {
		return nil, nil, fmt.Errorf("mysql 连接失败%s: %w", mysqlHint(err), err)
	}
	if dc.GetDebug() {
		db = db.Debug()
	}
	// Auto migration is a convenience for local development. In production,
	// apply schema changes as a separate reviewed step instead.
	sqlDB, sqlErr := db.DB()
	if sqlErr != nil {
		log.Error("failed to get sql.DB", "err", sqlErr)
		return nil, nil, fmt.Errorf("mysql 连接失败%s: %w", mysqlHint(sqlErr), sqlErr)
	}
	// 连接池：默认的「永不过期」在 MySQL 侧 wait_timeout（常配 8h）或中间代理
	// 主动掐连接时，会把死连接留在池子里，下一次取用才报 invalid connection。
	// 主动轮换比事后重试便宜：30 分钟一轮，连接建立的开销可以忽略。
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(10 * time.Minute)
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(10)
	cleanup := func() {
		log.Info("closing the data resources")
		if err := sqlDB.Close(); err != nil {
			log.Error("failed closing the database", "err", err)
		}
	}
	// /healthz 探活：SELECT 1（连接池空闲连接可能已断开，Ping 会重连）
	health.Register("mysql", func(ctx context.Context) error { return sqlDB.PingContext(ctx) })
	return &Data{db: db}, cleanup, nil
}
