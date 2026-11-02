package service

// 提示词工程管理面。
//
// 实现 api/prompt/v1/prompt.proto：模板 CRUD、版本发布/回滚、渲染预览。
// 一份实现同时挂到 HTTP 与 gRPC 两个 server 上。

import (
	"context"
	"strings"
	"time"

	v1 "go_agent/api/prompt/v1"
	"go_agent/internal/biz/prompt"
)

// PromptService 提示词工程管理面：模板 CRUD、版本发布/回滚、渲染预览、LLM 打分/生成/优化。
type PromptService struct {
	v1.UnimplementedPromptServiceServer

	uc     *prompt.UseCase
	studio *prompt.Studio
}

var (
	_ v1.PromptServiceHTTPServer = (*PromptService)(nil)
	_ v1.PromptServiceServer     = (*PromptService)(nil)
)

// NewPromptService 创建提示词服务
func NewPromptService(uc *prompt.UseCase, studio *prompt.Studio) *PromptService {
	return &PromptService{uc: uc, studio: studio}
}

// ListPrompts 模板列表。
func (s *PromptService) ListPrompts(ctx context.Context, req *v1.ListPromptsRequest) (*v1.ListPromptsReply, error) {
	list, err := s.uc.List(ctx, prompt.ListFilter{
		Scene:   req.GetScene(),
		Keyword: req.GetQ(),
		Limit:   int(req.GetLimit()),
	})
	if err != nil {
		return nil, mapPromptErr(err)
	}
	out := &v1.ListPromptsReply{Prompts: make([]*v1.Prompt, 0, len(list)), Total: int32(len(list))}
	for _, p := range list {
		out.Prompts = append(out.Prompts, toPrompt(p))
	}
	return out, nil
}

// GetPrompt 模板详情（含当前生效版本）。
func (s *PromptService) GetPrompt(ctx context.Context, req *v1.GetPromptRequest) (*v1.Detail, error) {
	d, err := s.uc.Get(ctx, req.GetKey())
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return toDetail(d), nil
}

// CreatePrompt 新建模板并写入首个版本。
func (s *PromptService) CreatePrompt(ctx context.Context, req *v1.CreatePromptRequest) (*v1.Detail, error) {
	if strings.TrimSpace(req.GetKey()) == "" {
		return nil, badRequest("请求体无效，需要 key 与 content 字段")
	}
	d, err := s.uc.Create(ctx, prompt.CreateInput{
		Key:         req.GetKey(),
		Name:        req.GetName(),
		Scene:       req.GetScene(),
		Description: req.GetDescription(),
		Tags:        req.GetTags(),
		Content:     req.GetContent(),
		Temperature: req.GetTemperature(),
		MaxTokens:   int(req.GetMaxTokens()),
		Note:        req.GetNote(),
		Author:      req.GetAuthor(),
	})
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return toDetail(d), nil
}

// UpdatePrompt 修改模板元信息（内容变更走 PublishVersion）。
//
// proto3 的 repeated 无法区分「未传」与「传空」，因此 tags 为空视为不改；
// 清空标签需要显式发布新版本，避免误删。
func (s *PromptService) UpdatePrompt(ctx context.Context, req *v1.UpdatePromptRequest) (*v1.Detail, error) {
	in := prompt.UpdateInput{
		Name:        req.Name,
		Scene:       req.Scene,
		Description: req.Description,
	}
	if tags := req.GetTags(); len(tags) > 0 {
		in.Tags = &tags
	}
	d, err := s.uc.Update(ctx, req.GetKey(), in)
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return toDetail(d), nil
}

// DeletePrompt 删除模板（内置模板不可删除，用 ResetPrompt 恢复）。
func (s *PromptService) DeletePrompt(ctx context.Context, req *v1.DeletePromptRequest) (*v1.DeletePromptReply, error) {
	if err := s.uc.Delete(ctx, req.GetKey()); err != nil {
		return nil, mapPromptErr(err)
	}
	return &v1.DeletePromptReply{Deleted: true}, nil
}

// ListVersions 版本历史。
func (s *PromptService) ListVersions(ctx context.Context, req *v1.ListVersionsRequest) (*v1.ListVersionsReply, error) {
	list, err := s.uc.Versions(ctx, req.GetKey(), int(req.GetLimit()))
	if err != nil {
		return nil, mapPromptErr(err)
	}
	out := &v1.ListVersionsReply{Versions: make([]*v1.Version, 0, len(list)), Total: int32(len(list))}
	for _, v := range list {
		out.Versions = append(out.Versions, toVersion(v))
	}
	return out, nil
}

// GetVersion 查询指定版本。
func (s *PromptService) GetVersion(ctx context.Context, req *v1.GetVersionRequest) (*v1.Version, error) {
	if req.GetVersion() <= 0 {
		return nil, badRequest("version 必须是正整数")
	}
	v, err := s.uc.Version(ctx, req.GetKey(), int(req.GetVersion()))
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return toVersion(*v), nil
}

// PublishVersion 发布新版本。
func (s *PromptService) PublishVersion(ctx context.Context, req *v1.PublishVersionRequest) (*v1.Version, error) {
	if strings.TrimSpace(req.GetContent()) == "" {
		return nil, badRequest("请求体无效，需要 content 字段")
	}
	v, err := s.uc.Publish(ctx, req.GetKey(), prompt.PublishInput{
		Content:     req.GetContent(),
		Temperature: req.GetTemperature(),
		MaxTokens:   int(req.GetMaxTokens()),
		Note:        req.GetNote(),
		Author:      req.GetAuthor(),
		Activate:    req.Activate,
	})
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return toVersion(*v), nil
}

// ActivatePrompt 激活指定版本。
func (s *PromptService) ActivatePrompt(ctx context.Context, req *v1.ActivatePromptRequest) (*v1.Detail, error) {
	if req.GetVersion() <= 0 {
		return nil, badRequest("请求体无效，需要 version 字段")
	}
	d, err := s.uc.Activate(ctx, req.GetKey(), int(req.GetVersion()))
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return toDetail(d), nil
}

// RollbackPrompt 回滚到上一个版本。
func (s *PromptService) RollbackPrompt(ctx context.Context, req *v1.RollbackPromptRequest) (*v1.Detail, error) {
	d, err := s.uc.Rollback(ctx, req.GetKey())
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return toDetail(d), nil
}

// RenderPrompt 渲染预览（不调用大模型）。
func (s *PromptService) RenderPrompt(ctx context.Context, req *v1.RenderPromptRequest) (*v1.RenderResult, error) {
	res, err := s.uc.Preview(ctx, req.GetKey(), prompt.RenderInput{
		Vars:    req.GetVars(),
		Version: int(req.GetVersion()),
	})
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return &v1.RenderResult{
		Key:       res.Key,
		Version:   int32(res.Version),
		Content:   res.Content,
		Rendered:  res.Rendered,
		Variables: res.Variables,
		Missing:   res.Missing,
	}, nil
}

// ResetPrompt 恢复内置默认内容（追加为新版本）。
func (s *PromptService) ResetPrompt(ctx context.Context, req *v1.ResetPromptRequest) (*v1.Version, error) {
	v, err := s.uc.Reset(ctx, req.GetKey(), req.GetAuthor())
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return toVersion(*v), nil
}

// GetPromptStats 模板与版本数量。
func (s *PromptService) GetPromptStats(ctx context.Context, _ *v1.GetPromptStatsRequest) (*v1.PromptStats, error) {
	st, err := s.uc.Stats(ctx)
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return &v1.PromptStats{Prompts: st.Prompts, Versions: st.Versions}, nil
}

// ListBuiltins 代码内置模板清单（前端「恢复默认」入口）。
func (s *PromptService) ListBuiltins(_ context.Context, _ *v1.ListBuiltinsRequest) (*v1.ListBuiltinsReply, error) {
	out := &v1.ListBuiltinsReply{Builtins: make([]*v1.BuiltinPrompt, 0, len(prompt.Builtins))}
	for _, b := range prompt.Builtins {
		out.Builtins = append(out.Builtins, &v1.BuiltinPrompt{
			Key:         b.Key,
			Name:        b.Name,
			Scene:       b.Scene,
			Description: b.Description,
			Tags:        b.Tags,
			Content:     b.Content,
			Variables:   prompt.Placeholders(b.Content),
			Temperature: b.Temperature,
			MaxTokens:   int32(b.MaxTokens),
		})
	}
	out.Total = int32(len(out.Builtins))
	return out, nil
}

// ---------- 领域模型 → 协议 ----------

func toPrompt(p prompt.Prompt) *v1.Prompt {
	return &v1.Prompt{
		Key:           p.Key,
		Name:          p.Name,
		Scene:         p.Scene,
		Description:   p.Description,
		Tags:          p.Tags,
		ActiveVersion: int32(p.ActiveVersion),
		Builtin:       p.Builtin,
		VersionCount:  int32(p.VersionCount),
		CreatedAt:     p.CreatedAt.Format(time.RFC3339),
		UpdatedAt:     p.UpdatedAt.Format(time.RFC3339),
	}
}

func toVersion(v prompt.Version) *v1.Version {
	return &v1.Version{
		PromptKey:   v.PromptKey,
		Version:     int32(v.Version),
		Content:     v.Content,
		Variables:   v.Variables,
		Temperature: v.Temperature,
		MaxTokens:   int32(v.MaxTokens),
		Note:        v.Note,
		Author:      v.Author,
		Active:      v.Active,
		CreatedAt:   v.CreatedAt.Format(time.RFC3339),
	}
}

func toDetail(d *prompt.Detail) *v1.Detail {
	if d == nil {
		return nil
	}
	meta := toPrompt(d.Prompt)
	out := &v1.Detail{
		Key:           meta.Key,
		Name:          meta.Name,
		Scene:         meta.Scene,
		Description:   meta.Description,
		Tags:          meta.Tags,
		ActiveVersion: meta.ActiveVersion,
		Builtin:       meta.Builtin,
		VersionCount:  meta.VersionCount,
		CreatedAt:     meta.CreatedAt,
		UpdatedAt:     meta.UpdatedAt,
	}
	if d.Active != nil {
		out.Active = toVersion(*d.Active)
	}
	return out
}

// ===== 提示词工程（LLM 驱动：打分 / 生成 / 优化）=====

// ScorePrompt 提示词打分：多维评分 + 问题清单 + 改进建议。
func (s *PromptService) ScorePrompt(ctx context.Context, req *v1.StudioScoreRequest) (*v1.PromptScoreReport, error) {
	if strings.TrimSpace(req.GetPrompt()) == "" {
		return nil, badRequest("prompt 不能为空")
	}
	rep, err := s.studio.Score(ctx, prompt.ScoreInput{
		Prompt:    req.GetPrompt(),
		Task:      req.GetTask(),
		LLMSource: req.GetLlmSource(),
	})
	if err != nil {
		return nil, mapPromptErr(err)
	}
	out := &v1.PromptScoreReport{
		Total:       int32(rep.Total),
		Grade:       rep.Grade,
		Summary:     rep.Summary,
		Model:       rep.Model,
		Issues:      rep.Issues,
		Suggestions: rep.Suggestions,
		Dimensions:  make([]*v1.ScoreDimension, 0, len(rep.Dimensions)),
	}
	for _, d := range rep.Dimensions {
		out.Dimensions = append(out.Dimensions, &v1.ScoreDimension{
			Name:    d.Name,
			Score:   int32(d.Score),
			Comment: d.Comment,
		})
	}
	return out, nil
}

// GeneratePrompt 按需求描述生成角色提示词。
func (s *PromptService) GeneratePrompt(ctx context.Context, req *v1.StudioGenerateRequest) (*v1.StudioPromptDraft, error) {
	if strings.TrimSpace(req.GetTask()) == "" {
		return nil, badRequest("task（需求描述）不能为空")
	}
	d, err := s.studio.Generate(ctx, prompt.GenerateInput{
		Task:      req.GetTask(),
		RoleName:  req.GetRoleName(),
		Extra:     req.GetExtraRequirements(),
		LLMSource: req.GetLlmSource(),
	})
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return &v1.StudioPromptDraft{
		Prompt:     d.Prompt,
		Name:       d.Name,
		Highlights: d.Highlights,
		Model:      d.Model,
	}, nil
}

// OptimizePrompt 优化已有提示词，返回改后版本与改动说明。
func (s *PromptService) OptimizePrompt(ctx context.Context, req *v1.StudioOptimizeRequest) (*v1.StudioOptimizeResult, error) {
	if strings.TrimSpace(req.GetPrompt()) == "" {
		return nil, badRequest("prompt 不能为空")
	}
	r, err := s.studio.Optimize(ctx, prompt.OptimizeInput{
		Prompt:    req.GetPrompt(),
		Task:      req.GetTask(),
		Goal:      req.GetGoal(),
		LLMSource: req.GetLlmSource(),
	})
	if err != nil {
		return nil, mapPromptErr(err)
	}
	return &v1.StudioOptimizeResult{
		Prompt:      r.Prompt,
		Changes:     r.Changes,
		ScoreBefore: int32(r.ScoreBefore),
		ScoreAfter:  int32(r.ScoreAfter),
		Model:       r.Model,
	}, nil
}

// ListPromptScores 提示词版本的评测得分列表（harness 回写，时间倒序）。
func (s *PromptService) ListPromptScores(ctx context.Context, req *v1.ListPromptScoresRequest) (*v1.PromptScoresReply, error) {
	list, err := s.uc.ListVersionScores(ctx, req.GetKey(), int(req.GetLimit()))
	if err != nil {
		return nil, mapPromptErr(err)
	}
	out := &v1.PromptScoresReply{Scores: make([]*v1.PromptScoreRecord, 0, len(list))}
	for _, sc := range list {
		out.Scores = append(out.Scores, &v1.PromptScoreRecord{
			Version:   int32(sc.Version),
			RunId:     int64(sc.RunID),
			Score:     sc.Score,
			Cases:     int32(sc.Cases),
			CreatedAt: formatTimeVal(sc.CreatedAt),
		})
	}
	return out, nil
}
