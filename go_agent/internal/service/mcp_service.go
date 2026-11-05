package service

// MCP 服务与工具管理面。
//
// 实现 api/mcp/v1/mcp.proto：服务 CRUD、连接/断开/探活、工具同步、
// 工具启停与调用、语义召回、运行时 http.Client 覆盖。规则全部在 internal/biz/mcp。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "go_agent/api/mcp/v1"
	"go_agent/internal/biz/mcp"
)

// MCPService MCP 管理面
type MCPService struct {
	v1.UnimplementedMCPServiceServer

	uc *mcp.UseCase
}

var (
	_ v1.MCPServiceHTTPServer = (*MCPService)(nil)
	_ v1.MCPServiceServer     = (*MCPService)(nil)
)

// NewMCPService 创建 MCP 服务
func NewMCPService(uc *mcp.UseCase) *MCPService { return &MCPService{uc: uc} }

// GetMCPStats 规模统计。
func (s *MCPService) GetMCPStats(ctx context.Context, _ *v1.GetMCPStatsRequest) (*v1.MCPStats, error) {
	st, err := s.uc.Stats(ctx)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return &v1.MCPStats{
		Servers:        st.Servers,
		EnabledServers: st.EnabledServers,
		Running:        st.Running,
		Tools:          st.Tools,
		EnabledTools:   st.EnabledTools,
		Vectorized:     st.Vectorized,
		Namespaces:     st.Namespaces,
	}, nil
}

// ListMCPNamespaces 命名空间清单。
func (s *MCPService) ListMCPNamespaces(ctx context.Context, _ *v1.ListMCPNamespacesRequest) (*v1.ListMCPNamespacesReply, error) {
	list, err := s.uc.Namespaces(ctx)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return &v1.ListMCPNamespacesReply{Namespaces: list}, nil
}

// ListMCPServers 服务列表。
func (s *MCPService) ListMCPServers(ctx context.Context, req *v1.ListMCPServersRequest) (*v1.ListMCPServersReply, error) {
	list, err := s.uc.ListServers(ctx, mcp.ServerFilter{
		Keyword:   req.GetQ(),
		Namespace: req.GetNamespace(),
		Transport: req.GetTransport(),
		Enabled:   parseBoolFilter(req.GetEnabled()),
		Limit:     int(req.GetLimit()),
	})
	if err != nil {
		return nil, mapMCPErr(err)
	}
	out := &v1.ListMCPServersReply{Servers: make([]*v1.MCPServer, 0, len(list)), Total: int32(len(list))}
	for i := range list {
		out.Servers = append(out.Servers, toMCPServer(&list[i], false))
	}
	return out, nil
}

// CreateMCPServer 新增服务。
func (s *MCPService) CreateMCPServer(ctx context.Context, req *v1.CreateMCPServerRequest) (*v1.MCPServer, error) {
	in, err := serverInput(req.GetName(), req.GetNamespace(), req.GetTransport(), req.GetDescription(),
		req.GetEndpoint(), req.GetHeaders(), req.GetCommand(), req.GetArgs(), req.GetEnv(),
		req.GetHealthCheckInterval(), req.GetEnabled(), req.GetProxyConfig())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	sv, err := s.uc.CreateServer(ctx, in)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	// 代理模式：创建即尝试解析文档并同步工具（失败不阻塞创建，状态里能看到原因）
	if sv.Transport == mcp.TransportProxy {
		if _, serr := s.uc.SyncTools(ctx, sv.ID); serr != nil {
			if latest, gerr := s.uc.GetServer(ctx, sv.ID); gerr == nil {
				return toMCPServer(latest, true), nil
			}
		} else if latest, gerr := s.uc.GetServer(ctx, sv.ID); gerr == nil {
			return toMCPServer(latest, true), nil
		}
	}
	return toMCPServer(sv, true), nil
}

// UpdateMCPServer 更新服务。
func (s *MCPService) UpdateMCPServer(ctx context.Context, req *v1.UpdateMCPServerRequest) (*v1.MCPServer, error) {
	in, err := serverInput(req.GetName(), req.GetNamespace(), req.GetTransport(), req.GetDescription(),
		req.GetEndpoint(), req.GetHeaders(), req.GetCommand(), req.GetArgs(), req.GetEnv(),
		req.GetHealthCheckInterval(), req.GetEnabled(), req.GetProxyConfig())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	sv, err := s.uc.UpdateServer(ctx, req.GetId(), in)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	// 代理模式：配置变更后重新解析（旧的 route 映射可能已失效）
	if sv.Transport == mcp.TransportProxy {
		if _, serr := s.uc.SyncTools(ctx, sv.ID); serr == nil {
			if latest, gerr := s.uc.GetServer(ctx, sv.ID); gerr == nil {
				return toMCPServer(latest, true), nil
			}
		}
	}
	return toMCPServer(sv, true), nil
}

// GetMCPServer 服务详情（含工具）。
func (s *MCPService) GetMCPServer(ctx context.Context, req *v1.GetMCPServerRequest) (*v1.MCPServer, error) {
	sv, err := s.uc.GetServer(ctx, req.GetId())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toMCPServer(sv, true), nil
}

// DeleteMCPServer 删除服务。
func (s *MCPService) DeleteMCPServer(ctx context.Context, req *v1.DeleteMCPServerRequest) (*v1.DeleteMCPServerReply, error) {
	if err := s.uc.DeleteServer(ctx, req.GetId()); err != nil {
		return nil, mapMCPErr(err)
	}
	return &v1.DeleteMCPServerReply{Id: req.GetId(), Deleted: true}, nil
}

// ToggleMCPServer 启停服务。
func (s *MCPService) ToggleMCPServer(ctx context.Context, req *v1.ToggleMCPServerRequest) (*v1.MCPServer, error) {
	sv, err := s.uc.SetServerEnabled(ctx, req.GetId(), req.GetEnabled())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toMCPServer(sv, true), nil
}

// ConnectMCPServer 建立连接。
func (s *MCPService) ConnectMCPServer(ctx context.Context, req *v1.ConnectMCPServerRequest) (*v1.MCPStatus, error) {
	st, err := s.uc.ConnectServer(ctx, req.GetId())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toMCPStatus(st), nil
}

// DisconnectMCPServer 断开连接。
func (s *MCPService) DisconnectMCPServer(ctx context.Context, req *v1.DisconnectMCPServerRequest) (*v1.MCPStatus, error) {
	st, err := s.uc.DisconnectServer(ctx, req.GetId())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toMCPStatus(st), nil
}

// PingMCPServer 探活。
func (s *MCPService) PingMCPServer(ctx context.Context, req *v1.PingMCPServerRequest) (*v1.MCPStatus, error) {
	st, err := s.uc.PingServer(ctx, req.GetId())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toMCPStatus(st), nil
}

// SyncMCPServer 同步工具清单。
func (s *MCPService) SyncMCPServer(ctx context.Context, req *v1.SyncMCPServerRequest) (*v1.SyncMCPServerReply, error) {
	res, err := s.uc.SyncTools(ctx, req.GetId())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return &v1.SyncMCPServerReply{Result: toSyncResult(*res)}, nil
}

// SyncAllMCPServers 同步全部启用服务。
func (s *MCPService) SyncAllMCPServers(ctx context.Context, _ *v1.SyncAllMCPServersRequest) (*v1.SyncAllMCPServersReply, error) {
	list, err := s.uc.SyncAll(ctx)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	out := &v1.SyncAllMCPServersReply{Results: make([]*v1.SyncResult, 0, len(list))}
	for _, r := range list {
		out.Results = append(out.Results, toSyncResult(r))
	}
	return out, nil
}

// SetMCPClient 运行时替换 http.Client 参数。
func (s *MCPService) SetMCPClient(ctx context.Context, req *v1.SetMCPClientRequest) (*v1.MCPStatus, error) {
	opts := mcp.ClientOptions{}
	if c := req.GetClient(); c != nil {
		opts = mcp.ClientOptions{
			TimeoutSec:  int(c.GetTimeoutSeconds()),
			ProxyURL:    strings.TrimSpace(c.GetProxyUrl()),
			InsecureTLS: c.GetInsecureTls(),
		}
	}
	st, err := s.uc.SetClientOptions(ctx, req.GetId(), opts)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toMCPStatus(st), nil
}

// ListMCPTools 工具列表。
func (s *MCPService) ListMCPTools(ctx context.Context, req *v1.ListMCPToolsRequest) (*v1.ListMCPToolsReply, error) {
	list, err := s.uc.ListTools(ctx, mcp.ToolFilter{
		ServerID:   req.GetMcpServerId(),
		Namespace:  req.GetNamespace(),
		Keyword:    req.GetQ(),
		Enabled:    parseBoolFilter(req.GetEnabled()),
		Vectorized: parseBoolFilter(req.GetVectorized()),
		Limit:      int(req.GetLimit()),
	})
	if err != nil {
		return nil, mapMCPErr(err)
	}
	out := &v1.ListMCPToolsReply{Tools: make([]*v1.MCPTool, 0, len(list)), Total: int32(len(list))}
	for i := range list {
		out.Tools = append(out.Tools, toMCPTool(&list[i]))
	}
	return out, nil
}

// GetMCPTool 工具详情。
func (s *MCPService) GetMCPTool(ctx context.Context, req *v1.GetMCPToolRequest) (*v1.MCPTool, error) {
	t, err := s.uc.GetTool(ctx, req.GetId())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toMCPTool(t), nil
}

// ToggleMCPTool 启停工具。
func (s *MCPService) ToggleMCPTool(ctx context.Context, req *v1.ToggleMCPToolRequest) (*v1.MCPTool, error) {
	t, err := s.uc.SetToolEnabled(ctx, req.GetId(), req.GetEnabled())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toMCPTool(t), nil
}

// DeleteMCPTool 删除工具。
func (s *MCPService) DeleteMCPTool(ctx context.Context, req *v1.DeleteMCPToolRequest) (*v1.DeleteMCPToolReply, error) {
	if err := s.uc.DeleteTool(ctx, req.GetId()); err != nil {
		return nil, mapMCPErr(err)
	}
	return &v1.DeleteMCPToolReply{Id: req.GetId(), Deleted: true}, nil
}

// SearchMCPTools 语义召回。
func (s *MCPService) SearchMCPTools(ctx context.Context, req *v1.SearchMCPToolsRequest) (*v1.SearchMCPToolsReply, error) {
	hits, err := s.uc.SearchTools(ctx, req.GetQ(), int(req.GetLimit()), req.GetNamespace())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	out := &v1.SearchMCPToolsReply{Hits: make([]*v1.MCPToolHit, 0, len(hits))}
	for _, h := range hits {
		out.Hits = append(out.Hits, &v1.MCPToolHit{
			Tool:   toMCPTool(&h.Tool),
			Score:  h.Score,
			Source: h.Source,
		})
	}
	return out, nil
}

// CallMCPTool 按 id 调用工具。
func (s *MCPService) CallMCPTool(ctx context.Context, req *v1.CallMCPToolRequest) (*v1.CallMCPToolReply, error) {
	args, err := parseCallArgs(req.GetArguments())
	if err != nil {
		return nil, err
	}
	res, err := s.uc.CallTool(ctx, req.GetId(), req.GetUniqueName(), args)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toCallReply(res), nil
}

// CallMCPToolByName 按唯一名调用工具。
func (s *MCPService) CallMCPToolByName(ctx context.Context, req *v1.CallMCPToolRequest) (*v1.CallMCPToolReply, error) {
	args, err := parseCallArgs(req.GetArguments())
	if err != nil {
		return nil, err
	}
	res, err := s.uc.CallTool(ctx, req.GetId(), req.GetUniqueName(), args)
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return toCallReply(res), nil
}

// PreviewMCPProxySpec 预览 Swagger/OpenAPI 解析结果（保存代理前确认接口清单）。
func (s *MCPService) PreviewMCPProxySpec(ctx context.Context, req *v1.PreviewMCPProxySpecRequest) (*v1.PreviewMCPProxySpecReply, error) {
	preview, err := s.uc.PreviewProxy(ctx, req.GetProxyConfig())
	if err != nil {
		return nil, mapMCPErr(err)
	}
	out := &v1.PreviewMCPProxySpecReply{
		BaseUrl:  preview.BaseURL,
		Title:    preview.Title,
		Version:  preview.Version,
		Warnings: preview.Warnings,
	}
	out.Operations = make([]*v1.MCPProxyOperation, 0, len(preview.Ops))
	for _, op := range preview.Ops {
		out.Operations = append(out.Operations, &v1.MCPProxyOperation{
			Name:        op.Name,
			Method:      op.Method,
			Path:        op.Path,
			Summary:     op.Summary,
			Description: op.Description,
			Tags:        op.Tags,
			Deprecated:  op.Deprecated,
			InputSchema: op.InputSchema(),
		})
	}
	return out, nil
}

// —— 转换 ——

func serverInput(name, namespace, transport, desc, endpoint string, headers map[string]string,
	command string, args []string, env map[string]string, health int64, enabled, proxyConfig string) (mcp.ServerInput, error) {
	in := mcp.ServerInput{
		Name:                name,
		Namespace:           namespace,
		Transport:           transport,
		Description:         desc,
		Endpoint:            endpoint,
		Headers:             headers,
		Command:             command,
		Args:                args,
		Env:                 env,
		HealthCheckInterval: health,
		Enabled:             parseBoolFilter(enabled),
		ProxyConfig:         proxyConfig,
	}
	return in, nil
}

func toMCPServer(s *mcp.Server, withTools bool) *v1.MCPServer {
	out := &v1.MCPServer{
		Id:                  s.ID,
		Name:                s.Name,
		Namespace:           s.EffectiveNamespace(),
		Transport:           s.Transport,
		Enabled:             s.Enabled,
		Description:         s.Description,
		Endpoint:            s.Endpoint,
		Headers:             s.Headers,
		Command:             s.Command,
		Args:                s.Args,
		Env:                 s.Env,
		HealthCheckInterval: s.HealthCheckInterval,
		ProxyConfig:         s.ProxyConfig,
		ToolCount:           int32(s.ToolCount),
		Status:              toMCPStatus(s.Status),
		CreatedAt:           formatTS(s.CreatedAt),
		UpdatedAt:           formatTS(s.UpdatedAt),
	}
	if withTools {
		out.Tools = make([]*v1.MCPTool, 0, len(s.Tools))
		for i := range s.Tools {
			out.Tools = append(out.Tools, toMCPTool(&s.Tools[i]))
		}
	}
	if out.ToolCount == 0 && len(s.Tools) > 0 {
		out.ToolCount = int32(len(s.Tools))
	}
	return out
}

func toMCPTool(t *mcp.Tool) *v1.MCPTool {
	return &v1.MCPTool{
		Id:            t.ID,
		McpServerId:   t.ServerID,
		McpServerName: t.ServerName,
		Namespace:     t.Namespace,
		ToolName:      t.ToolName,
		UniqueName:    t.UniqueName,
		DisplayName:   t.DisplayName,
		Description:   t.Description,
		InputSchema:   t.InputSchema,
		OutputSchema:  t.OutputSchema,
		Examples:      t.Examples,
		Tags:          t.Tags,
		Enabled:       t.Enabled,
		Vectorized:    t.Vectorized,
		Version:       t.Version,
		Route:         t.Route,
		CreatedAt:     formatTS(t.CreatedAt),
		UpdatedAt:     formatTS(t.UpdatedAt),
	}
}

func toMCPStatus(st mcp.Status) *v1.MCPStatus {
	return &v1.MCPStatus{
		McpServerId:     st.ServerID,
		ServerName:      st.ServerName,
		Running:         st.Running,
		LastPingAt:      formatTS(st.LastPingAt),
		LastSyncAt:      formatTS(st.LastSyncAt),
		ToolCount:       int32(st.ToolCount),
		ErrorMsg:        st.ErrorMsg,
		HttpClient:      st.HTTPClient,
		ProtocolVersion: st.ProtocolVersion,
		ServerVersion:   st.ServerVersion,
	}
}

func toSyncResult(r mcp.SyncResult) *v1.SyncResult {
	return &v1.SyncResult{
		McpServerId:   r.ServerID,
		McpServerName: r.ServerName,
		Fetched:       int32(r.Fetched),
		Added:         int32(r.Added),
		Updated:       int32(r.Updated),
		Removed:       int32(r.Removed),
		Vectorized:    int32(r.Vectorized),
		Warning:       r.Warning,
	}
}

func toCallReply(r *mcp.CallResult) *v1.CallMCPToolReply {
	out := &v1.CallMCPToolReply{
		IsError:    r.IsError,
		Structured: r.Structured,
		ElapsedMs:  r.ElapsedMs,
		Content:    make([]*v1.MCPContent, 0, len(r.Content)),
	}
	for _, c := range r.Content {
		out.Content = append(out.Content, &v1.MCPContent{Type: c.Type, Text: c.Text, Data: c.Data})
	}
	return out
}

// parseCallArgs 工具参数：空串视为无参，非法 JSON 直接 400
func parseCallArgs(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, badRequest(fmt.Sprintf("arguments 不是合法的 JSON 对象: %v", err))
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// parseBoolFilter "1/true/0/false/空"
func parseBoolFilter(s string) *bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		v := true
		return &v
	case "0", "false", "no":
		v := false
		return &v
	default:
		return nil
	}
}

// formatTS 零值时间输出空串，避免前端显示 0001-01-01
func formatTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
