package service

// 技能管理面。
//
// 实现 api/skill/v1/skill.proto：技能安装/启停/卸载、文件读取、落盘同步，
// 以及技能市场（skillhub）搜索与下载安装。规则全部在 internal/biz/skill。

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	v1 "go_agent/api/skill/v1"
	"go_agent/internal/biz/skill"
	"go_agent/internal/pkg/identity"
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
	// 安装技能 = 往磁盘写文件 + 让 Agent 获得新能力（技能正文会被拼进提示词），
	// 属于平台级变更：登录用户 ≠ 管理员，走统一闸门。
	if err := requireAdmin(ctx, "skill.install"); err != nil {
		return nil, err
	}
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
		Execution:   fromSpecPB(req.GetExecution()),
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
	if err := requireAdmin(ctx, "skill.uninstall"); err != nil {
		return nil, err
	}
	if err := s.uc.Uninstall(ctx, req.GetName()); err != nil {
		return nil, mapSkillErr(err)
	}
	return &v1.UninstallSkillReply{Deleted: true}, nil
}

// EnableSkill 启用技能。
//
// 启停是「这份能力对当前工作区是否可见」的偏好设置，不改变磁盘内容，
// 因此不接管理闸门——否则普通用户连自己装过的技能都关不掉。
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
	// 同步会把启用技能整体落盘到 sync_dir（覆盖宿主机的技能目录），平台级变更
	if err := requireAdmin(ctx, "skill.sync"); err != nil {
		return nil, err
	}
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
	// 从市场下载并安装 = 引入外部代码与提示词，等同于 skill.install
	if err := requireAdmin(ctx, "skill.install.hub"); err != nil {
		return nil, err
	}
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
	// 服务端主动去拉取外部地址，既是安装动作也是出站请求，双重敏感
	if err := requireAdmin(ctx, "skill.install.url"); err != nil {
		return nil, err
	}
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
	// Hub 配置里带访问令牌，且决定后续所有技能包的下载来源 —— 平台级配置
	if err := requireAdmin(ctx, "skill.hub.create"); err != nil {
		return nil, err
	}
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
	if err := requireAdmin(ctx, "skill.hub.update"); err != nil {
		return nil, err
	}
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
	if err := requireAdmin(ctx, "skill.hub.delete"); err != nil {
		return nil, err
	}
	if err := s.uc.DeleteHub(ctx, req.GetName()); err != nil {
		return nil, mapSkillErr(err)
	}
	return &v1.DeleteHubReply{Deleted: true}, nil
}

// SetDefaultHub 设为默认市场。
func (s *SkillService) SetDefaultHub(ctx context.Context, req *v1.SetDefaultHubRequest) (*v1.Hub, error) {
	if err := requireAdmin(ctx, "skill.hub.set_default"); err != nil {
		return nil, err
	}
	h, err := s.uc.SetDefaultHub(ctx, req.GetName())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toHub(*h), nil
}

// ---------- V2 运行时：执行 / 实例 / 执行记录 ----------

// ExecuteSkill 执行技能：local 返回 SKILL.md，remote 同步等结果，async 返回执行 ID。
//
// 不做管理员闸门：执行技能是「用能力」，不是「改平台配置」。能不能用由技能是否启用
// 与 Agent 白名单决定；租户隔离靠运行时写入的执行记录（tenant_id / user_id）。
func (s *SkillService) ExecuteSkill(ctx context.Context, req *v1.ExecuteSkillRequest) (*v1.ExecuteSkillReply, error) {
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, badRequest("需要 name 字段")
	}
	input := map[string]any{}
	if raw := strings.TrimSpace(req.GetInput()); raw != "" {
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return nil, badRequest("input 必须是 JSON 对象字符串：" + err.Error())
		}
	}
	info := identity.From(ctx)
	res, err := s.uc.Execute(ctx, skill.RuntimeRequest{
		SkillName:      name,
		TaskID:         strings.TrimSpace(req.GetTaskId()),
		Input:          input,
		TenantID:       info.TenantID,
		UserID:         info.UserID,
		Purpose:        req.GetPurpose(),
		IdempotencyKey: req.GetIdempotencyKey(),
		ForceAsync:     req.GetAsync(),
		Timeout:        time.Duration(req.GetTimeoutSec()) * time.Second,
	})
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return &v1.ExecuteSkillReply{
		ExecutionId: res.ExecutionID,
		SkillName:   res.SkillName,
		Mode:        res.Mode,
		Status:      res.Status,
		Output:      marshalMap(res.Output),
		Text:        res.Text,
		WorkerId:    res.WorkerID,
		Attempts:    int32(res.Attempts),
		LatencyMs:   res.LatencyMS,
		Replayed:    res.Replayed,
		Queued:      res.Queued,
	}, nil
}

// SetSkillExecution 修改执行配置（local ⇄ remote ⇄ async），不动技能内容。
func (s *SkillService) SetSkillExecution(ctx context.Context, req *v1.SetSkillExecutionRequest) (*v1.Skill, error) {
	// 改执行配置 = 改流量去向（可能指向外部 Worker），平台级变更
	if err := requireAdmin(ctx, "skill.execution.update"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, badRequest("需要 name 字段")
	}
	if req.GetExecution() == nil {
		return nil, badRequest("需要 execution 字段（mode=local/remote/async）")
	}
	spec := *fromSpecPB(req.GetExecution())
	sk, err := s.uc.SetExecution(ctx, req.GetName(), spec)
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toSkill(sk, true), nil
}

// ListSkillExecutions 执行记录列表（非管理员只看自己的）。
func (s *SkillService) ListSkillExecutions(ctx context.Context, req *v1.ListSkillExecutionsRequest) (*v1.ListSkillExecutionsReply, error) {
	info := identity.From(ctx)
	f := skill.ExecutionFilter{
		SkillName: strings.TrimSpace(req.GetSkill()),
		Status:    strings.TrimSpace(req.GetStatus()),
		TaskID:    strings.TrimSpace(req.GetTaskId()),
		// 跨租户硬隔离：即使管理员也不跨租户查（跨租户排障走审计）
		TenantID: info.TenantID,
		Limit:    int(req.GetLimit()),
	}
	if !info.Admin {
		f.UserID = info.UserID
	}
	list, err := s.uc.ListExecutions(ctx, f)
	if err != nil {
		return nil, mapSkillErr(err)
	}
	out := &v1.ListSkillExecutionsReply{Executions: make([]*v1.SkillExecution, 0, len(list)), Total: int32(len(list))}
	for i := range list {
		out.Executions = append(out.Executions, toExecution(&list[i]))
	}
	return out, nil
}

// GetSkillExecution 单条执行记录（异步结果轮询）。
func (s *SkillService) GetSkillExecution(ctx context.Context, req *v1.GetSkillExecutionRequest) (*v1.SkillExecution, error) {
	exec, err := s.uc.GetExecution(ctx, req.GetId())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	// 拿到记录还要过一遍归属：ID 可枚举，不能靠「不知道别人的 ID」当隔离
	if err := guardExecution(ctx, exec); err != nil {
		return nil, err
	}
	return toExecution(exec), nil
}

// CancelSkillExecution 取消排队中的执行。
func (s *SkillService) CancelSkillExecution(ctx context.Context, req *v1.CancelSkillExecutionRequest) (*v1.SkillExecution, error) {
	exec, err := s.uc.GetExecution(ctx, req.GetId())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	if err := guardExecution(ctx, exec); err != nil {
		return nil, err
	}
	out, err := s.uc.CancelExecution(ctx, req.GetId())
	if err != nil {
		return nil, badRequest(err.Error())
	}
	return toExecution(out), nil
}

// ListSkillInstances 实例列表。
func (s *SkillService) ListSkillInstances(ctx context.Context, req *v1.ListSkillInstancesRequest) (*v1.ListSkillInstancesReply, error) {
	list, err := s.uc.ListInstances(ctx, req.GetName())
	if err != nil {
		return nil, mapSkillErr(err)
	}
	out := &v1.ListSkillInstancesReply{Instances: make([]*v1.SkillInstance, 0, len(list)), Total: int32(len(list))}
	for i := range list {
		out.Instances = append(out.Instances, toInstance(&list[i]))
	}
	return out, nil
}

// RegisterSkillInstance Worker 自注册。
//
// 注册决定「流量打到哪」，是平台级变更；Worker 侧用平台令牌（或认证关闭时的本机回环）注册。
func (s *SkillService) RegisterSkillInstance(ctx context.Context, req *v1.RegisterSkillInstanceRequest) (*v1.SkillInstance, error) {
	if err := requireAdmin(ctx, "skill.instance.register"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetEndpoint()) == "" {
		return nil, badRequest("需要 endpoint 字段")
	}
	inst, err := s.uc.RegisterInstance(ctx, skill.RegisterInstanceInput{
		SkillName:      req.GetName(),
		ID:             req.GetId(),
		Endpoint:       req.GetEndpoint(),
		Version:        req.GetVersion(),
		Region:         req.GetRegion(),
		Labels:         req.GetLabels(),
		Weight:         int(req.GetWeight()),
		MaxConcurrency: int(req.GetMaxConcurrency()),
	})
	if err != nil {
		return nil, mapSkillErr(err)
	}
	return toInstance(inst), nil
}

// HeartbeatSkillInstance 实例心跳。
//
// 心跳只更新自身的在途数与状态，不改平台配置，因此不接管理员闸门——
// 否则 Worker 必须持有管理员令牌才能保活，权限过大。
func (s *SkillService) HeartbeatSkillInstance(ctx context.Context, req *v1.HeartbeatSkillInstanceRequest) (*v1.HeartbeatSkillInstanceReply, error) {
	err := s.uc.Heartbeat(ctx, skill.InstanceHeartbeat{
		SkillName: req.GetName(),
		ID:        req.GetId(),
		Inflight:  int(req.GetInflight()),
		LatencyMS: req.GetLatencyMs(),
		Status:    req.GetStatus(),
	})
	if err != nil {
		// 心跳打到未注册实例：404 让 Worker 知道要重新注册
		return nil, mapSkillErr(err)
	}
	return &v1.HeartbeatSkillInstanceReply{Ok: true}, nil
}

// DeleteSkillInstance 摘除实例。
func (s *SkillService) DeleteSkillInstance(ctx context.Context, req *v1.DeleteSkillInstanceRequest) (*v1.DeleteSkillInstanceReply, error) {
	if err := requireAdmin(ctx, "skill.instance.delete"); err != nil {
		return nil, err
	}
	if err := s.uc.DeleteInstance(ctx, req.GetName(), req.GetId()); err != nil {
		return nil, mapSkillErr(err)
	}
	return &v1.DeleteSkillInstanceReply{Deleted: true}, nil
}

// guardExecution 执行记录归属校验：同租户 + （管理员或本人）
func guardExecution(ctx context.Context, e *skill.Execution) error {
	info := identity.From(ctx)
	if e.TenantID != "" && info.TenantID != "" && e.TenantID != info.TenantID {
		return notFound("执行记录不存在: " + e.ID)
	}
	if !info.Admin && e.UserID != "" && e.UserID != info.UserID {
		return notFound("执行记录不存在: " + e.ID)
	}
	return nil
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
		Execution:   toSpecPB(sk.Execution),
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

// —— 运行时：领域模型 → 协议 ——

func toSpecPB(spec skill.ExecutionSpec) *v1.SkillExecutionSpec {
	n := spec.Normalized()
	return &v1.SkillExecutionSpec{
		Mode:           n.Mode,
		Protocol:       n.Protocol,
		Endpoint:       n.Endpoint,
		InputSchema:    n.InputSchema,
		OutputSchema:   n.OutputSchema,
		TimeoutSec:     int32(n.TimeoutSec),
		Retry:          int32(n.Retry),
		Concurrency:    int32(n.Concurrency),
		Idempotent:     n.Idempotent,
		RequiredScopes: n.RequiredScopes,
		Handler:        n.Handler,
	}
}

// fromSpecPB 协议 → 领域；nil 表示「沿用已安装的执行配置」
func fromSpecPB(in *v1.SkillExecutionSpec) *skill.ExecutionSpec {
	if in == nil {
		return nil
	}
	return &skill.ExecutionSpec{
		Mode:           in.GetMode(),
		Protocol:       in.GetProtocol(),
		Endpoint:       in.GetEndpoint(),
		InputSchema:    in.GetInputSchema(),
		OutputSchema:   in.GetOutputSchema(),
		TimeoutSec:     int(in.GetTimeoutSec()),
		Retry:          int(in.GetRetry()),
		Concurrency:    int(in.GetConcurrency()),
		Idempotent:     in.GetIdempotent(),
		RequiredScopes: in.GetRequiredScopes(),
		Handler:        in.GetHandler(),
	}
}

func toInstance(inst *skill.Instance) *v1.SkillInstance {
	if inst == nil {
		return nil
	}
	return &v1.SkillInstance{
		Id:             inst.ID,
		SkillName:      inst.SkillName,
		Endpoint:       inst.Endpoint,
		Version:        inst.Version,
		Region:         inst.Region,
		Labels:         inst.Labels,
		Weight:         int32(inst.Weight),
		MaxConcurrency: int32(inst.MaxConcurrency),
		Inflight:       int32(inst.Inflight),
		Status:         inst.Status,
		LastHeartbeat:  formatTime(inst.LastHeartbeat),
		LatencyMs:      inst.LatencyMS,
		Failures:       int32(inst.Failures),
		Successes:      inst.Successes,
		LastError:      inst.LastError,
	}
}

func toExecution(e *skill.Execution) *v1.SkillExecution {
	if e == nil {
		return nil
	}
	return &v1.SkillExecution{
		Id:             e.ID,
		TaskId:         e.TaskID,
		SkillName:      e.SkillName,
		SkillVersion:   e.SkillVersion,
		Mode:           e.Mode,
		Status:         e.Status,
		TenantId:       e.TenantID,
		UserId:         e.UserID,
		AgentKey:       e.AgentKey,
		SessionId:      e.SessionID,
		IdempotencyKey: e.IdempotencyKey,
		Input:          e.Input,
		Output:         e.Output,
		Error:          e.Error,
		WorkerId:       e.WorkerID,
		Attempts:       int32(e.Attempts),
		LatencyMs:      e.LatencyMS,
		TraceId:        e.TraceID,
		QueuedAt:       formatTimePtr(e.QueuedAt),
		StartedAt:      formatTimePtr(e.StartedAt),
		FinishedAt:     formatTimePtr(e.FinishedAt),
		CreatedAt:      formatTime(e.CreatedAt),
	}
}

// formatTimePtr 指针时间（零值/空指针都返回空串）
func formatTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// marshalMap 输出 JSON（nil/序列化失败都退化为空串：这是展示字段，不该影响主流程）
func marshalMap(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(raw)
}
