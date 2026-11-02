package service

// 技能管理面。
//
// 实现 api/skill/v1/skill.proto：技能安装/启停/卸载、文件读取、落盘同步，
// 以及技能市场（skillhub）搜索与下载安装。规则全部在 internal/biz/skill。

import (
	"context"
	"strings"

	v1 "go_agent/api/skill/v1"
	"go_agent/internal/biz/skill"
)

// SkillService 技能管理面。
type SkillService struct {
	v1.UnimplementedSkillServiceServer

	uc *skill.UseCase
}

var (
	_ v1.SkillServiceHTTPServer = (*SkillService)(nil)
	_ v1.SkillServiceServer     = (*SkillService)(nil)
)

// NewSkillService 创建技能服务
func NewSkillService(uc *skill.UseCase) *SkillService { return &SkillService{uc: uc} }

// ListSkills 技能列表。
func (s *SkillService) ListSkills(ctx context.Context, req *v1.ListSkillsRequest) (*v1.ListSkillsReply, error) {
	f := skill.Filter{
		Keyword:  req.GetQ(),
		Category: req.GetCategory(),
		Source:   req.GetSource(),
		Limit:    int(req.GetLimit()),
	}
	switch strings.ToLower(req.GetEnabled()) {
	case "1", "true":
		v := true
		f.Enabled = &v
	case "0", "false":
		v := false
		f.Enabled = &v
	}
	list, err := s.uc.List(ctx, f)
	if err != nil {
		return nil, mapSkillErr(err)
	}
	out := &v1.ListSkillsReply{Skills: make([]*v1.Skill, 0, len(list)), Total: int32(len(list))}
	for _, sk := range list {
		out.Skills = append(out.Skills, toSkill(&sk, false))
	}
	return out, nil
}

// GetSkill 技能详情（含 SKILL.md 与文件正文）。
func (s *SkillService) GetSkill(ctx context.Context, req *v1.GetSkillRequest) (*v1.Skill, error) {
	sk, err := s.uc.Get(ctx, req.GetName())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toSkill(sk, true), nil
}

// InstallSkill 手动安装/上传技能（同名视为升级）。
func (s *SkillService) InstallSkill(ctx context.Context, req *v1.InstallSkillRequest) (*v1.Skill, error) {
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, badRequest("请求体无效，需要 name 与 content（或 files）字段")
	}
	in := skill.InstallInput{
		Name:        req.GetName(),
		Version:     req.GetVersion(),
		Title:       req.GetTitle(),
		Description: req.GetDescription(),
		Category:    req.GetCategory(),
		Tags:        req.GetTags(),
		Content:     req.GetContent(),
		Enabled:     req.Enabled,
	}
	for _, f := range req.GetFiles() {
		in.Files = append(in.Files, skill.File{Path: f.GetPath(), Content: f.GetContent(), Size: int(f.GetSize())})
	}
	sk, err := s.uc.Install(ctx, in)
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toSkill(sk, true), nil
}

// UninstallSkill 卸载技能。
func (s *SkillService) UninstallSkill(ctx context.Context, req *v1.UninstallSkillRequest) (*v1.UninstallSkillReply, error) {
	if err := s.uc.Uninstall(ctx, req.GetName()); err != nil {
		return nil, mapSkillErr(err)
	}
	return &v1.UninstallSkillReply{Deleted: true}, nil
}

// EnableSkill 启用技能。
func (s *SkillService) EnableSkill(ctx context.Context, req *v1.ToggleSkillRequest) (*v1.Skill, error) {
	sk, err := s.uc.Enable(ctx, req.GetName())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toSkill(sk, true), nil
}

// DisableSkill 停用技能。
func (s *SkillService) DisableSkill(ctx context.Context, req *v1.ToggleSkillRequest) (*v1.Skill, error) {
	sk, err := s.uc.Disable(ctx, req.GetName())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toSkill(sk, true), nil
}

// UseSkill 使用技能：返回技能内容，同时落一条技能使用历史（op_histories）。
func (s *SkillService) UseSkill(ctx context.Context, req *v1.UseSkillRequest) (*v1.Skill, error) {
	sk, err := s.uc.UseSkill(ctx, req.GetName(), req.GetPurpose())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toSkill(sk, true), nil
}

// ListSkillFiles 技能文件清单（只返回路径与大小）。
func (s *SkillService) ListSkillFiles(ctx context.Context, req *v1.ListSkillFilesRequest) (*v1.SkillFiles, error) {
	sk, err := s.uc.Get(ctx, req.GetName())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	out := &v1.SkillFiles{
		Name:      sk.Name,
		FileCount: int32(len(sk.Files)),
		Size:      int32(sk.Size),
		Files:     make([]*v1.File, 0, len(sk.Files)),
	}
	for _, f := range sk.Files {
		out.Files = append(out.Files, &v1.File{Path: f.Path, Size: int32(f.Size)})
	}
	return out, nil
}

// ReadSkillFile 读取单个文件正文。
func (s *SkillService) ReadSkillFile(ctx context.Context, req *v1.ReadSkillFileRequest) (*v1.File, error) {
	path := strings.TrimSpace(req.GetPath())
	if path == "" {
		return nil, badRequest("需要 path 查询参数")
	}
	sk, err := s.uc.Get(ctx, req.GetName())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	for _, f := range sk.Files {
		if f.Path == path {
			return &v1.File{Path: f.Path, Content: f.Content, Size: int32(f.Size)}, nil
		}
	}
	if strings.EqualFold(path, "SKILL.md") {
		return &v1.File{Path: "SKILL.md", Content: sk.Content, Size: int32(len(sk.Content))}, nil
	}
	return nil, notFound("文件不存在: " + path)
}

// SyncSkills 把启用技能落盘到 sync_dir。
func (s *SkillService) SyncSkills(ctx context.Context, _ *v1.SyncSkillsRequest) (*v1.SyncResult, error) {
	res, err := s.uc.Sync(ctx)
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return &v1.SyncResult{Dir: res.Dir, Synced: res.Synced, Removed: res.Removed}, nil
}

// GetSkillStats 规模统计。
func (s *SkillService) GetSkillStats(ctx context.Context, _ *v1.GetSkillStatsRequest) (*v1.SkillStats, error) {
	st, err := s.uc.Stats(ctx)
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return &v1.SkillStats{Skills: st.Skills, Enabled: st.Enabled, Hubs: st.Hubs}, nil
}

// SearchHubSkills 搜索技能市场。
func (s *SkillService) SearchHubSkills(ctx context.Context, req *v1.SearchHubSkillsRequest) (*v1.SearchHubSkillsReply, error) {
	list, err := s.uc.SearchHub(ctx, req.GetQ(), req.GetHub(), int(req.GetLimit()))
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toHubSkills(list), nil
}

// SearchSkillhub 搜索技能市场（与 SearchHubSkills 等价的语义入口）。
func (s *SkillService) SearchSkillhub(ctx context.Context, req *v1.SearchHubSkillsRequest) (*v1.SearchHubSkillsReply, error) {
	return s.SearchHubSkills(ctx, req)
}

// InstallFromHub 从技能市场安装。
func (s *SkillService) InstallFromHub(ctx context.Context, req *v1.InstallFromHubRequest) (*v1.Skill, error) {
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, badRequest("请求体无效，需要 name 字段")
	}
	sk, err := s.uc.InstallFromHub(ctx, skill.InstallFromHubInput{
		Name:    req.GetName(),
		Version: req.GetVersion(),
		Hub:     req.GetHub(),
		Enable:  req.Enable,
	})
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toSkill(sk, true), nil
}

// ImportSkillFromURL 从技能包 URL 安装（zip / SKILL.md / JSON 包）。
func (s *SkillService) ImportSkillFromURL(ctx context.Context, req *v1.ImportSkillFromURLRequest) (*v1.Skill, error) {
	if strings.TrimSpace(req.GetUrl()) == "" {
		return nil, badRequest("请求体无效，需要 url 字段")
	}
	sk, err := s.uc.ImportFromURL(ctx, skill.ImportFromURLInput{
		URL:     req.GetUrl(),
		Name:    req.GetName(),
		Version: req.GetVersion(),
		Token:   req.GetToken(),
		Enable:  req.Enable,
	})
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toSkill(sk, true), nil
}

// ListHubs 技能市场列表。
func (s *SkillService) ListHubs(ctx context.Context, _ *v1.ListHubsRequest) (*v1.ListHubsReply, error) {
	list, err := s.uc.ListHubs(ctx)
	if err != nil {
		return nil, mapSkillErr(err)
	}
	out := &v1.ListHubsReply{Hubs: make([]*v1.Hub, 0, len(list)), Total: int32(len(list))}
	for _, h := range list {
		out.Hubs = append(out.Hubs, toHub(h))
	}
	return out, nil
}

// ListSkillhubHubs 技能市场列表（语义入口）。
func (s *SkillService) ListSkillhubHubs(ctx context.Context, req *v1.ListHubsRequest) (*v1.ListHubsReply, error) {
	return s.ListHubs(ctx, req)
}

// CreateHub 新增技能市场。
func (s *SkillService) CreateHub(ctx context.Context, req *v1.CreateHubRequest) (*v1.Hub, error) {
	if strings.TrimSpace(req.GetName()) == "" || strings.TrimSpace(req.GetBaseUrl()) == "" {
		return nil, badRequest("请求体无效，需要 name 与 base_url 字段")
	}
	h, err := s.uc.CreateHub(ctx, skill.HubInput{
		Name:        req.GetName(),
		BaseURL:     req.GetBaseUrl(),
		Token:       req.GetToken(),
		Enabled:     req.Enabled,
		IsDefault:   req.GetIsDefault(),
		Description: req.GetDescription(),
	})
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toHub(*h), nil
}

// UpdateHub 修改技能市场。
func (s *SkillService) UpdateHub(ctx context.Context, req *v1.UpdateHubRequest) (*v1.Hub, error) {
	h, err := s.uc.UpdateHub(ctx, req.GetName(), skill.HubPatch{
		BaseURL:     req.BaseUrl,
		Token:       req.Token,
		Enabled:     req.Enabled,
		IsDefault:   req.IsDefault,
		Description: req.Description,
	})
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toHub(*h), nil
}

// DeleteHub 删除技能市场。
func (s *SkillService) DeleteHub(ctx context.Context, req *v1.DeleteHubRequest) (*v1.DeleteHubReply, error) {
	if err := s.uc.DeleteHub(ctx, req.GetName()); err != nil {
		return nil, mapSkillErr(err)
	}
	return &v1.DeleteHubReply{Deleted: true}, nil
}

// SetDefaultHub 设为默认市场。
func (s *SkillService) SetDefaultHub(ctx context.Context, req *v1.SetDefaultHubRequest) (*v1.Hub, error) {
	h, err := s.uc.SetDefaultHub(ctx, req.GetName())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toHub(*h), nil
}

// ---------- 领域模型 → 协议 ----------

// toSkill 技能详情。列表接口不返回 SKILL.md 与文件正文（withBody=false），
// 避免一次拉几百个技能的全文。
func toSkill(sk *skill.Skill, withBody bool) *v1.Skill {
	if sk == nil {
		return nil
	}
	out := &v1.Skill{
		Name:        sk.Name,
		Version:     sk.Version,
		Title:       sk.Title,
		Description: sk.Description,
		Category:    sk.Category,
		Tags:        sk.Tags,
		Source:      sk.Source,
		SourceUrl:   sk.SourceURL,
		Enabled:     sk.Enabled,
		Checksum:    sk.Checksum,
		FileCount:   int32(sk.FileCount),
		Size:        int32(sk.Size),
		InstalledAt: sk.InstalledAt.Format(timeLayout),
		UpdatedAt:   sk.UpdatedAt.Format(timeLayout),
	}
	if withBody {
		out.Content = sk.Content
		if len(sk.Files) > 0 {
			out.Files = make([]*v1.File, 0, len(sk.Files))
			for _, f := range sk.Files {
				out.Files = append(out.Files, &v1.File{Path: f.Path, Content: f.Content, Size: int32(f.Size)})
			}
		}
	}
	return out
}

func toHub(h skill.Hub) *v1.Hub {
	return &v1.Hub{
		Name:        h.Name,
		BaseUrl:     h.BaseURL,
		HasToken:    h.HasToken,
		Enabled:     h.Enabled,
		IsDefault:   h.IsDefault,
		Description: h.Description,
		CreatedAt:   h.CreatedAt.Format(timeLayout),
		UpdatedAt:   h.UpdatedAt.Format(timeLayout),
	}
}

func toHubSkills(list []skill.HubSkill) *v1.SearchHubSkillsReply {
	out := &v1.SearchHubSkillsReply{Skills: make([]*v1.HubSkill, 0, len(list)), Total: int32(len(list))}
	for _, s := range list {
		out.Skills = append(out.Skills, &v1.HubSkill{
			Name:        s.Name,
			Version:     s.Version,
			Title:       s.Title,
			Description: s.Description,
			Category:    s.Category,
			Tags:        s.Tags,
			Downloads:   int32(s.Downloads),
			Installed:   s.Installed,
			Hub:         s.Hub,
		})
	}
	return out
}
