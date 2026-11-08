package service

// 外部 AI 平台接入面（千问 / 百炼 MCP 广场）。
//
// 实现 api/aiplatform/v1/aiplatform.proto：平台 CRUD / 默认切换 / 连通性测试，
// 以及「从平台目录搜索 MCP 服务」与「一键接入成本地 MCP 服务」。
// 规则全部在 internal/biz/aiplatform：接入动作复用 biz/mcp 的建服务→连接→同步工具链路。

import (
	"context"
	"strings"

	v1 "go_agent/api/aiplatform/v1"
	"go_agent/internal/biz/aiplatform"
	"go_agent/internal/pkg/redact"
)

// AIPlatformService 外部 AI 平台接入面。
type AIPlatformService struct {
	v1.UnimplementedAIPlatformServiceServer

	uc *aiplatform.UseCase
}

var (
	_ v1.AIPlatformServiceHTTPServer = (*AIPlatformService)(nil)
	_ v1.AIPlatformServiceServer     = (*AIPlatformService)(nil)
)

// NewAIPlatformService 创建服务
func NewAIPlatformService(uc *aiplatform.UseCase) *AIPlatformService {
	return &AIPlatformService{uc: uc}
}

// ListPlatforms 平台列表。
func (s *AIPlatformService) ListPlatforms(ctx context.Context, req *v1.ListPlatformsRequest) (*v1.ListPlatformsReply, error) {
	f := aiplatform.Filter{Keyword: req.GetQ(), Limit: int(req.GetLimit())}
	switch strings.ToLower(strings.TrimSpace(req.GetEnabled())) {
	case "1", "true":
		v := true
		f.Enabled = &v
	case "0", "false":
		v := false
		f.Enabled = &v
	}
	list, err := s.uc.List(ctx, f)
	if err != nil {
		return nil, mapAIPlatformErr(err)
	}
	out := &v1.ListPlatformsReply{Platforms: make([]*v1.Platform, 0, len(list)), Total: int32(len(list))}
	for i := range list {
		out.Platforms = append(out.Platforms, toPlatform(&list[i]))
	}
	return out, nil
}

// CreatePlatform 新增平台。
func (s *AIPlatformService) CreatePlatform(ctx context.Context, req *v1.CreatePlatformRequest) (*v1.PlatformReply, error) {
	in := aiplatform.Input{
		Name:    req.GetName(),
		Kind:    req.GetKind(),
		Headers: req.GetHeaders(),
	}
	in.BaseURL = strPtr(req.BaseUrl)
	in.APIKey = strPtr(req.ApiKey)
	in.AuthScheme = strPtr(req.AuthScheme)
	in.AuthHeader = strPtr(req.AuthHeader)
	in.CatalogPath = strPtr(req.CatalogPath)
	in.StreamablePath = strPtr(req.StreamablePath)
	in.SSEPath = strPtr(req.SsePath)
	in.SkillBaseURL = strPtr(req.SkillBaseUrl)
	in.Description = strPtr(req.Description)
	in.Enabled = req.Enabled
	in.IsDefault = req.IsDefault

	p, warn, err := s.uc.Create(ctx, in)
	if err != nil {
		return nil, mapAIPlatformErr(err)
	}
	return &v1.PlatformReply{Platform: toPlatform(p), Warning: warn}, nil
}

// UpdatePlatform 修改平台。
func (s *AIPlatformService) UpdatePlatform(ctx context.Context, req *v1.UpdatePlatformRequest) (*v1.PlatformReply, error) {
	in := aiplatform.Input{
		Name:           req.GetName(),
		Headers:        req.GetHeaders(),
		BaseURL:        req.BaseUrl,
		APIKey:         req.ApiKey,
		AuthScheme:     req.AuthScheme,
		AuthHeader:     req.AuthHeader,
		CatalogPath:    req.CatalogPath,
		StreamablePath: req.StreamablePath,
		SSEPath:        req.SsePath,
		SkillBaseURL:   req.SkillBaseUrl,
		Description:    req.Description,
		Enabled:        req.Enabled,
		IsDefault:      req.IsDefault,
	}
	if req.Kind != nil {
		in.Kind = *req.Kind
	}
	if len(req.GetHeaders()) == 0 {
		in.Headers = nil // 未传 headers 表示不改
	} else {
		// 读接口给的是掩码：写回时还原成旧值，否则「只改个描述」保存一次
		// 就会把网关凭证写成 ******（凭证被悄悄换掉，鉴权全挂）
		cur, gerr := s.uc.Get(ctx, req.GetName())
		if gerr != nil {
			return nil, mapAIPlatformErr(gerr)
		}
		var old map[string]string
		if cur != nil {
			old = cur.Headers
		}
		in.Headers = redact.MergeMasked(req.GetHeaders(), old)
	}

	p, warn, err := s.uc.Update(ctx, req.GetName(), in)
	if err != nil {
		return nil, mapAIPlatformErr(err)
	}
	return &v1.PlatformReply{Platform: toPlatform(p), Warning: warn}, nil
}

// DeletePlatform 删除平台。
func (s *AIPlatformService) DeletePlatform(ctx context.Context, req *v1.DeletePlatformRequest) (*v1.DeletePlatformReply, error) {
	if err := s.uc.Delete(ctx, req.GetName()); err != nil {
		return nil, mapAIPlatformErr(err)
	}
	return &v1.DeletePlatformReply{Name: req.GetName(), Deleted: true}, nil
}

// SetDefaultPlatform 设为默认。
func (s *AIPlatformService) SetDefaultPlatform(ctx context.Context, req *v1.SetDefaultPlatformRequest) (*v1.PlatformReply, error) {
	p, err := s.uc.SetDefault(ctx, req.GetName())
	if err != nil {
		return nil, mapAIPlatformErr(err)
	}
	return &v1.PlatformReply{Platform: toPlatform(p)}, nil
}

// TestPlatform 连通性测试。
func (s *AIPlatformService) TestPlatform(ctx context.Context, req *v1.TestPlatformRequest) (*v1.TestResult, error) {
	res, err := s.uc.Test(ctx, req.GetName())
	if err != nil {
		return nil, mapAIPlatformErr(err)
	}
	return &v1.TestResult{
		Ok:             res.OK,
		Message:        res.Message,
		LatencyMs:      res.LatencyMS,
		CatalogOk:      res.CatalogOK,
		SampleCount:    int32(res.SampleCount),
		AuthConfigured: res.AuthConfigured,
	}, nil
}

// SearchMCP 从平台目录搜索 MCP 服务。
func (s *AIPlatformService) SearchMCP(ctx context.Context, req *v1.SearchMCPRequest) (*v1.SearchMCPReply, error) {
	items, err := s.uc.SearchMCP(ctx, req.GetPlatform(), req.GetKeyword(), int(req.GetLimit()))
	if err != nil {
		return nil, mapAIPlatformErr(err)
	}
	out := &v1.SearchMCPReply{Items: make([]*v1.RemoteMCP, 0, len(items)), Total: int32(len(items))}
	for i := range items {
		out.Items = append(out.Items, toRemoteMCP(&items[i]))
	}
	return out, nil
}

// ImportMCP 把平台服务接入成本地 MCP 服务。
func (s *AIPlatformService) ImportMCP(ctx context.Context, req *v1.ImportMCPRequest) (*v1.ImportResult, error) {
	res, err := s.uc.ImportMCP(ctx, aiplatform.ImportInput{
		Platform:    req.GetPlatform(),
		Code:        req.GetCode(),
		Name:        req.GetName(),
		Namespace:   req.GetNamespace(),
		Transport:   req.GetTransport(),
		Description: req.GetDescription(),
		Enabled:     req.Enabled,
		SyncTools:   req.SyncTools,
	})
	if err != nil {
		return nil, mapAIPlatformErr(err)
	}
	return &v1.ImportResult{
		McpServerId: int64(res.ServerID),
		ServerName:  res.ServerName,
		Namespace:   res.Namespace,
		Endpoint:    res.Endpoint,
		ToolCount:   int32(res.ToolCount),
		Reused:      res.Reused,
		Message:     res.Message,
	}, nil
}

// —— 映射 ——

func toPlatform(p *aiplatform.Platform) *v1.Platform {
	if p == nil {
		return nil
	}
	return &v1.Platform{
		Id:         int64(p.ID),
		Name:       p.Name,
		Kind:       p.Kind,
		BaseUrl:    p.BaseURL,
		HasKey:     p.HasKey,
		AuthScheme: p.AuthScheme,
		AuthHeader: p.AuthHeader,
		// 网关头可能带 Authorization / 签名，读接口只给键名 + 掩码（P1-03）
		Headers:        redact.MaskMap(p.Headers),
		CatalogPath:    p.CatalogPath,
		StreamablePath: p.StreamablePath,
		SsePath:        p.SSEPath,
		SkillBaseUrl:   p.SkillBaseURL,
		SkillHub:       p.SkillHub,
		Description:    p.Description,
		Enabled:        p.Enabled,
		IsDefault:      p.IsDefault,
		CreatedAt:      p.CreatedAt.Format(timeLayout),
		UpdatedAt:      p.UpdatedAt.Format(timeLayout),
	}
}

func toRemoteMCP(m *aiplatform.RemoteMCP) *v1.RemoteMCP {
	return &v1.RemoteMCP{
		Code:        m.Code,
		Name:        m.Name,
		Description: m.Description,
		Provider:    m.Provider,
		Transport:   m.Transport,
		Endpoint:    m.Endpoint,
		ToolCount:   int32(m.ToolCount),
		Tags:        m.Tags,
		Installed:   m.Installed,
		ServerName:  m.ServerName,
	}
}

// strPtr 空串视为「未传」，便于复用 biz 的「不改」语义。
func strPtr(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}
