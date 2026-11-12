package service

// 大模型数据源管理面。
//
// 实现 api/llm/v1/llm.proto：多数据源 CRUD、默认切换、连通性测试、
// 远端模型列表与调试对话。配置以 MySQL 为准，业务侧走 biz/llm.Manager 的
// Chatter 接口，不直接依赖这里。api_key 只进不出——读取接口只返回 has_key。

import (
	"context"
	"strings"

	v1 "go_agent/api/llm/v1"
	"go_agent/internal/biz/llm"
)

// LLMService 大模型数据源管理面。
type LLMService struct {
	v1.UnimplementedLLMServiceServer

	uc    *llm.Manager
	media *llm.MediaManager
}

var (
	_ v1.LLMServiceHTTPServer = (*LLMService)(nil)
	_ v1.LLMServiceServer     = (*LLMService)(nil)
)

// NewLLMService 创建 LLM 管理服务（media 为媒体生成用例，可为 nil：媒体能力降级报错）
func NewLLMService(uc *llm.Manager, media *llm.MediaManager) *LLMService {
	return &LLMService{uc: uc, media: media}
}

// ListProviders 数据源列表。
func (s *LLMService) ListProviders(ctx context.Context, req *v1.ListProvidersRequest) (*v1.ListProvidersReply, error) {
	list, err := s.uc.ListProviders(ctx, req.GetAll())
	if err != nil {
		return nil, mapLLMErr(err)
	}
	out := &v1.ListProvidersReply{Providers: make([]*v1.Provider, 0, len(list)), Total: int32(len(list))}
	for _, p := range list {
		out.Providers = append(out.Providers, toProvider(p))
	}
	return out, nil
}

// GetProvider 数据源详情。
func (s *LLMService) GetProvider(ctx context.Context, req *v1.GetProviderRequest) (*v1.Provider, error) {
	p, err := s.uc.GetProvider(ctx, req.GetName())
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return toProvider(*p), nil
}

// CreateProvider 新建数据源。
func (s *LLMService) CreateProvider(ctx context.Context, req *v1.CreateProviderRequest) (*v1.Provider, error) {
	if strings.TrimSpace(req.GetName()) == "" ||
		strings.TrimSpace(req.GetBaseUrl()) == "" ||
		strings.TrimSpace(req.GetDefaultModel()) == "" {
		return nil, badRequest("请求体无效，需要 name/base_url/default_model 字段")
	}
	p, err := s.uc.CreateProvider(ctx, llm.CreateProviderInput{
		Name:         req.GetName(),
		Kind:         req.GetKind(),
		BaseURL:      req.GetBaseUrl(),
		APIKey:       req.GetApiKey(),
		Models:       req.GetModels(),
		DefaultModel: req.GetDefaultModel(),
		ImageModels:  req.GetImageModels(),
		VideoModels:  req.GetVideoModels(),
		Temperature:  req.GetTemperature(),
		MaxTokens:    int(req.GetMaxTokens()),
		TimeoutSec:   int(req.GetTimeoutSeconds()),
		Enabled:      req.Enabled,
		IsDefault:    req.GetIsDefault(),
		Priority:     int(req.GetPriority()),
		Scenes:       req.GetScenes(),
		Description:  req.GetDescription(),
	})
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return toProvider(*p), nil
}

// UpdateProvider 修改数据源。
//
// proto3 的 repeated 无法区分「未传」与「传空」，models/scenes 为空视为不改。
func (s *LLMService) UpdateProvider(ctx context.Context, req *v1.UpdateProviderRequest) (*v1.Provider, error) {
	in := llm.UpdateProviderInput{
		Kind:         req.Kind,
		BaseURL:      req.BaseUrl,
		APIKey:       req.ApiKey,
		DefaultModel: req.DefaultModel,
		Temperature:  req.Temperature,
		MaxTokens:    intPtr(req.MaxTokens),
		TimeoutSec:   intPtr(req.TimeoutSeconds),
		Enabled:      req.Enabled,
		IsDefault:    req.IsDefault,
		Priority:     intPtr(req.Priority),
		Description:  req.Description,
	}
	if models := req.GetModels(); len(models) > 0 {
		in.Models = &models
	}
	if scenes := req.GetScenes(); len(scenes) > 0 {
		in.Scenes = &scenes
	}
	// 媒体模型：repeated 无法区分「未传」与「清空」，遵循 models/scenes 同款约定
	// ——传了才改；清空场景让用户在列表里留一个占位值或直接置空数组走前端显式提交。
	// 为支持清空，前端编辑弹层永远显式携带完整数组。
	if imgs := req.GetImageModels(); req.ImageModels != nil {
		in.ImageModels = &imgs
	}
	if vids := req.GetVideoModels(); req.VideoModels != nil {
		in.VideoModels = &vids
	}
	p, err := s.uc.UpdateProvider(ctx, req.GetName(), in)
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return toProvider(*p), nil
}

// DeleteProvider 删除数据源（唯一启用的数据源不允许删除）。
func (s *LLMService) DeleteProvider(ctx context.Context, req *v1.DeleteProviderRequest) (*v1.DeleteProviderReply, error) {
	if err := s.uc.DeleteProvider(ctx, req.GetName()); err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.DeleteProviderReply{Deleted: true}, nil
}

// EnableProvider 启用数据源。
func (s *LLMService) EnableProvider(ctx context.Context, req *v1.ToggleProviderRequest) (*v1.Provider, error) {
	return s.toggle(ctx, req.GetName(), true)
}

// DisableProvider 停用数据源。
func (s *LLMService) DisableProvider(ctx context.Context, req *v1.ToggleProviderRequest) (*v1.Provider, error) {
	return s.toggle(ctx, req.GetName(), false)
}

func (s *LLMService) toggle(ctx context.Context, name string, enabled bool) (*v1.Provider, error) {
	p, err := s.uc.SetEnabled(ctx, name, enabled)
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return toProvider(*p), nil
}

// SetDefaultProvider 设为默认数据源。
func (s *LLMService) SetDefaultProvider(ctx context.Context, req *v1.ToggleProviderRequest) (*v1.Provider, error) {
	p, err := s.uc.SetDefault(ctx, req.GetName())
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return toProvider(*p), nil
}

// TestProvider 连通性测试（会真实发起一次对话）。
func (s *LLMService) TestProvider(ctx context.Context, req *v1.ToggleProviderRequest) (*v1.TestResult, error) {
	res, err := s.uc.TestProvider(ctx, req.GetName())
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.TestResult{
		Ok:        res.OK,
		Provider:  res.Provider,
		Model:     res.Model,
		LatencyMs: res.LatencyMS,
		Reply:     res.Reply,
		Error:     res.Error,
	}, nil
}

// ListRemoteModels 拉取远端 /models（不落库，供前端勾选）。
func (s *LLMService) ListRemoteModels(ctx context.Context, req *v1.RemoteModelsRequest) (*v1.RemoteModelsReply, error) {
	models, err := s.uc.RemoteModels(ctx, req.GetName())
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.RemoteModelsReply{Models: models, Total: int32(len(models))}, nil
}

// GetLLMStats 数据源规模。
func (s *LLMService) GetLLMStats(ctx context.Context, _ *v1.GetLLMStatsRequest) (*v1.LLMStats, error) {
	st, err := s.uc.Stats(ctx)
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.LLMStats{Providers: st.Providers, Enabled: st.Enabled, Default: st.Default}, nil
}

// GetCurrent 当前生效的数据源与模型（前端状态栏用）。
func (s *LLMService) GetCurrent(ctx context.Context, _ *v1.GetCurrentRequest) (*v1.CurrentLLM, error) {
	st, err := s.uc.Stats(ctx)
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.CurrentLLM{
		Model:     s.uc.Model(),
		Default:   st.Default,
		Providers: st.Providers,
		Enabled:   st.Enabled,
	}, nil
}

// ListScenes 内置场景清单。
func (s *LLMService) ListScenes(_ context.Context, _ *v1.GetCurrentRequest) (*v1.ScenesReply, error) {
	return &v1.ScenesReply{Scenes: []string{
		llm.SceneGeneral, llm.SceneRAG, llm.SceneGraph, llm.SceneExtract, llm.SceneMemory,
	}}, nil
}

// Chat 调试对话（管理端手动调用）。
func (s *LLMService) Chat(ctx context.Context, req *v1.ChatRequest) (*v1.ChatReply, error) {
	if strings.TrimSpace(req.GetUser()) == "" {
		return nil, badRequest("请求体无效，需要 user 字段")
	}
	reply, err := s.uc.ChatWith(ctx, llm.ChatInput{
		Provider:    req.GetProvider(),
		Scene:       req.GetScene(),
		System:      req.GetSystem(),
		User:        req.GetUser(),
		Temperature: req.GetTemperature(),
	})
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.ChatReply{Reply: reply}, nil
}

// ---------- 领域模型 → 协议 ----------

func toProvider(p llm.Provider) *v1.Provider {
	return &v1.Provider{
		Id:             p.ID,
		Name:           p.Name,
		Kind:           p.Kind,
		BaseUrl:        p.BaseURL,
		HasKey:         p.HasKey,
		Models:         p.Models,
		DefaultModel:   p.DefaultModel,
		ImageModels:    p.ImageModels,
		VideoModels:    p.VideoModels,
		Temperature:    p.Temperature,
		MaxTokens:      int32(p.MaxTokens),
		TimeoutSeconds: int32(p.TimeoutSec),
		Enabled:        p.Enabled,
		IsDefault:      p.IsDefault,
		Priority:       int32(p.Priority),
		Scenes:         p.Scenes,
		Description:    p.Description,
		CreatedAt:      p.CreatedAt.Format(timeLayout),
		UpdatedAt:      p.UpdatedAt.Format(timeLayout),
	}
}
