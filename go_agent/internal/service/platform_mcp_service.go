package service

// 平台内置 MCP 服务：把 biz/platformmcp 的工具语义包装成可被 MCP 管理器注册的服务。
// 与 internal/server/mcp.go 的协议层配合：server 只做 JSON-RPC，这里负责工具实现与自注册。

import (
	"context"
	"fmt"
	"net"
	"strings"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/platformmcp"
	"go_agent/internal/conf"
)

// PlatformMCPPath 平台 MCP 端点路径（与 internal/server/mcp.go 保持一致）
const PlatformMCPPath = "/mcp/platform"

// PlatformMCPService 平台数据 MCP 服务
type PlatformMCPService struct {
	uc       *platformmcp.UseCase
	mcp      *mcp.UseCase
	endpoint string
}

// NewPlatformMCPService 构造；endpoint 由 HTTP 监听地址推导（0.0.0.0 → 127.0.0.1）
func NewPlatformMCPService(uc *platformmcp.UseCase, mcpUC *mcp.UseCase, c *conf.Server) *PlatformMCPService {
	return &PlatformMCPService{uc: uc, mcp: mcpUC, endpoint: platformEndpoint(c)}
}

// UseCase 暴露给协议层
func (s *PlatformMCPService) UseCase() *platformmcp.UseCase { return s.uc }

// Endpoint 本服务对内地址
func (s *PlatformMCPService) Endpoint() string { return s.endpoint }

func platformEndpoint(c *conf.Server) string {
	addr := "127.0.0.1:8000"
	if c != nil && c.GetHttp() != nil && strings.TrimSpace(c.GetHttp().GetAddr()) != "" {
		addr = c.GetHttp().GetAddr()
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = "127.0.0.1", "8000"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s%s", host, port, PlatformMCPPath)
}

// EnsureServer 幂等注册内置 MCP 服务并同步工具清单。
// 启动时调用：首次创建 mcp_servers 行 + tools 行（含向量化），之后每次只做同步，
// 保证新增工具（本程序升级）能自动出现在能力目录里。
func (s *PlatformMCPService) EnsureServer(ctx context.Context) error {
	if s == nil || s.mcp == nil {
		return fmt.Errorf("MCP 用例未初始化")
	}
	enabled := true
	in := mcp.ServerInput{
		Name:                platformmcp.ServerName,
		Namespace:           platformmcp.Namespace,
		Transport:           mcp.TransportHTTP,
		Description:         "平台内置只读数据源：任务中心、会话列表/消息、执行链路与工具/召回明细",
		Endpoint:            s.endpoint,
		HealthCheckInterval: 60,
		Enabled:             &enabled,
	}

	var id uint64
	servers, err := s.mcp.ListServers(ctx, mcp.ServerFilter{Keyword: platformmcp.ServerName, Limit: 100})
	if err != nil {
		return fmt.Errorf("查询 MCP 服务失败: %w", err)
	}
	for _, srv := range servers {
		if srv.Name == platformmcp.ServerName {
			id = srv.ID
			// 端口/路径变化时更新端点，保证自调用地址始终正确
			if srv.Endpoint != s.endpoint || srv.Transport != mcp.TransportHTTP || !srv.Enabled {
				if _, err := s.mcp.UpdateServer(ctx, id, in); err != nil {
					return fmt.Errorf("更新内置 MCP 服务失败: %w", err)
				}
			}
			break
		}
	}
	if id == 0 {
		created, err := s.mcp.CreateServer(ctx, in)
		if err != nil {
			return fmt.Errorf("创建内置 MCP 服务失败: %w", err)
		}
		id = created.ID
	}

	res, err := s.mcp.SyncTools(ctx, id)
	if err != nil {
		return fmt.Errorf("同步内置 MCP 工具失败: %w", err)
	}
	if res != nil && res.Warning != "" {
		// 向量化失败不算致命：工具已落库，语义召回暂时降级
		return fmt.Errorf("工具已同步但向量化失败: %s", res.Warning)
	}
	return nil
}
