// Package milvusdb 提供 Milvus 3.x 的连接管理与存储分层：
// models 定义集合 Schema（字段/索引），repo 实现各业务 Store。
package milvusdb

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"go_agent/internal/conf"
	"go_agent/internal/pkg/health"

	"github.com/go-kratos/kratos/v3/log"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// Client Milvus 连接（主集合与图集合共用一条 gRPC 连接）
type Client struct {
	cli *milvusclient.Client
	cfg *conf.Data_Milvus
	dim int
}

// New 建立 Milvus 连接。
//
// Milvus Go SDK 内部使用 grpc.WithBlock 且默认没有拨号超时，Milvus 未启动时
// New 会无限重连、进程静默挂住（表现为“没有任何日志”）。
// 因此先做 TCP 探测 + 拨号超时，让不可达场景秒级失败并给出明确提示。
func New(c *conf.Data) (*Client, func(), error) {
	cfg := c.GetMilvus()
	dim := int(c.GetEmbedding().GetDimension())
	if dim <= 0 {
		dim = 1024
	}
	addr := grpcAddr(cfg.GetUri())

	if err := probeTCP(addr, 3*time.Second); err != nil {
		return nil, nil, fmt.Errorf(
			"无法连接 Milvus %s: %w\n  请确认 Milvus 已启动（docker compose up -d / K8s 中 milvus Pod 是否 Ready）",
			addr, err)
	}

	timeout := cfg.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cli, err := milvusclient.New(dialCtx, &milvusclient.ClientConfig{
		Address: cfg.GetUri(),
		APIKey:  cfg.GetToken(),
		DBName:  "default",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("connect milvus %s (timeout %s): %w", addr, timeout, err)
	}
	log.Info("milvus connected", "addr", addr, "collection", cfg.GetCollection(), "dim", dim)

	// /healthz 探活：Milvus 客户端无 CheckHealth API，这里做 TCP 可达性探测
	// （与启动时同一套探测逻辑，能反映「端口挂了 / 网络不通」这类最常见故障）
	health.Register("milvus", func(ctx context.Context) error { return probeTCP(addr, 2*time.Second) })

	client := &Client{cli: cli, cfg: cfg, dim: dim}
	cleanup := func() {
		_ = cli.Close(context.Background())
	}
	return client, cleanup, nil
}

// —— 供 repo 子包使用的导出访问器 ——

// CLI 返回底层 Milvus gRPC 客户端
func (c *Client) CLI() *milvusclient.Client { return c.cli }

// Collection 主集合名
func (c *Client) Collection() string { return c.cfg.GetCollection() }

// Dim 向量维度
func (c *Client) Dim() int { return c.dim }

// LoadTimeout 集合加载超时
func (c *Client) LoadTimeout() time.Duration {
	timeout := c.cfg.GetLoadTimeout().AsDuration()
	if timeout <= 0 {
		return 120 * time.Second
	}
	return timeout
}

// grpcAddr 从 http://host:19530 之类的 URI 中提取 host:port
func grpcAddr(uri string) string {
	s := strings.TrimSpace(uri)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if !strings.Contains(s, ":") {
		s += ":19530"
	}
	return s
}

// probeTCP 探测端口连通性。localhost 可能同时解析到 ::1 与 127.0.0.1，
// 只试其中一个容易误判（容器常只映射 IPv4），因此逐个尝试。
func probeTCP(addr string, timeout time.Duration) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "19530"
	}
	targets := []string{addr}
	if host == "localhost" || host == "" {
		targets = []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("::1", port)}
	} else if ips, err := net.LookupHost(host); err == nil && len(ips) > 1 {
		targets = targets[:0]
		for _, ip := range ips {
			targets = append(targets, net.JoinHostPort(ip, port))
		}
	}
	var lastErr error
	for _, t := range targets {
		conn, err := net.DialTimeout("tcp", t, timeout)
		if err == nil {
			return conn.Close()
		}
		lastErr = err
	}
	return lastErr
}
