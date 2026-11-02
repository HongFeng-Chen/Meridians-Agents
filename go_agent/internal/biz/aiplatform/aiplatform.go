package aiplatform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/skill"
)

// UseCase 外部 AI 平台接入用例。
//
// 依赖全部是接口：Repo（MySQL）、Catalog（HTTP 目录）、MCPRegistry（*mcp.UseCase）、
// SkillMarkets（*skill.UseCase）。所以这里只做编排：解析地址、拼鉴权头、判断是否已接入、
// 调用顺序与回滚。
type UseCase struct {
	repo Repo
	cat  Catalog
	mcp  MCPRegistry
	hubs SkillMarkets
}

// New 构造
func New(repo Repo, cat Catalog, registry MCPRegistry, hubs SkillMarkets) *UseCase {
	return &UseCase{repo: repo, cat: cat, mcp: registry, hubs: hubs}
}

// —— 平台 CRUD ——

// List 平台列表（API Key 脱敏）
func (u *UseCase) List(ctx context.Context, f Filter) ([]Platform, error) {
	list, err := u.repo.List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]Platform, 0, len(list))
	for _, p := range list {
		out = append(out, p.Masked())
	}
	return out, nil
}

// Get 平台详情（API Key 脱敏）
func (u *UseCase) Get(ctx context.Context, name string) (*Platform, error) {
	p, err := u.resolvePlatform(ctx, name)
	if err != nil {
		return nil, err
	}
	m := p.Masked()
	return &m, nil
}

// Create 新增平台。warning 非空表示平台已保存但「技能市场登记」没成功（不阻断）。
func (u *UseCase) Create(ctx context.Context, in Input) (*Platform, string, error) {
	if err := in.Normalize(true); err != nil {
		return nil, "", err
	}
	if cur, err := u.repo.Get(ctx, in.Name); err == nil && cur != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrPlatformExists, in.Name)
	} else if err != nil && !errors.Is(err, ErrPlatformNotFound) {
		return nil, "", err
	}

	p := &Platform{Name: in.Name}
	applyInput(p, &in)
	if p.IsDefault {
		// 先把其它平台降级，Repo.SetDefault 内部统一处理
		defer func() { _ = u.repo.SetDefault(ctx, p.Name) }()
	}
	if err := u.repo.Save(ctx, p); err != nil {
		return nil, "", err
	}
	warn := u.syncSkillHub(ctx, p)
	if warn != "" {
		// hub 没登记成功时不要留下一个指向不存在 hub 的名字
		p.SkillHub = ""
		_ = u.repo.Save(ctx, p)
	}
	m := p.Masked()
	return &m, warn, nil
}

// Update 修改平台（指针字段为空表示不改）。返回保存后的平台与技能市场告警。
func (u *UseCase) Update(ctx context.Context, name string, in Input) (*Platform, string, error) {
	cur, err := u.repo.Get(ctx, name)
	if err != nil {
		return nil, "", err
	}
	in.Name = name
	if strings.TrimSpace(in.Kind) == "" { // 未传 kind 表示不改，避免把 qwen 重置成 generic
		in.Kind = cur.Kind
	}
	inheritInput(cur, &in) // 未传的字段先用现值填上，避免 Normalize 的默认值把自定义配置覆盖回官方默认
	if err := in.Normalize(false); err != nil {
		return nil, "", err
	}
	applyInput(cur, &in)
	if err := u.repo.Save(ctx, cur); err != nil {
		return nil, "", err
	}
	if cur.IsDefault {
		if err := u.repo.SetDefault(ctx, cur.Name); err != nil {
			return nil, "", err
		}
	}
	warn := u.syncSkillHub(ctx, cur)
	if warn != "" && strings.TrimSpace(cur.SkillBaseURL) == "" {
		cur.SkillHub = ""
		_ = u.repo.Save(ctx, cur)
	}
	m := cur.Masked()
	return &m, warn, nil
}

// Delete 删除平台（不会删除已接入的 MCP 服务，避免误删线上能力）
func (u *UseCase) Delete(ctx context.Context, name string) error {
	if _, err := u.repo.Get(ctx, name); err != nil {
		return err
	}
	return u.repo.Delete(ctx, name)
}

// SetDefault 设为默认平台
func (u *UseCase) SetDefault(ctx context.Context, name string) (*Platform, error) {
	if _, err := u.repo.Get(ctx, name); err != nil {
		return nil, err
	}
	if err := u.repo.SetDefault(ctx, name); err != nil {
		return nil, err
	}
	return u.Get(ctx, name)
}

// —— MCP 广场：搜索 / 接入 / 测试 ——

// SearchMCP 从平台目录搜索 MCP 服务；目录不可用时返回 ErrCatalogUnsupported（前端据此提示按编码接入）。
func (u *UseCase) SearchMCP(ctx context.Context, platformName, keyword string, limit int) ([]RemoteMCP, error) {
	p, err := u.resolvePlatform(ctx, platformName)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	items, err := u.cat.Search(ctx, *p, strings.TrimSpace(keyword), limit)
	if err != nil {
		return nil, err
	}
	installed, err := u.installedSet(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteMCP, 0, len(items))
	for _, it := range items {
		code := strings.TrimSpace(it.Code)
		if code == "" {
			continue
		}
		it.Code = code
		it.Provider = p.Name
		it.ServerName = p.LocalServerName(code)
		if strings.TrimSpace(it.Endpoint) == "" {
			if ep, rerr := p.ResolveEndpoint(code, mcp.TransportHTTP); rerr == nil {
				it.Endpoint = ep
			}
		}
		if it.Transport == "" {
			it.Transport = mcp.TransportHTTP
		}
		it.Installed = installed[it.ServerName]
		out = append(out, it)
	}
	return out, nil
}

// ImportMCP 把一个平台服务接入成本地 MCP 服务（建服务 → 连接 → 同步工具）。
//
// 幂等：同名服务已存在时不重复创建，直接重新连接并同步工具（Reused=true）。
// 回滚：连接失败会删掉刚建的服务，不留下「配置在但连不上」的脏数据。
func (u *UseCase) ImportMCP(ctx context.Context, in ImportInput) (*ImportResult, error) {
	p, err := u.resolvePlatform(ctx, in.Platform)
	if err != nil {
		return nil, err
	}
	code := strings.TrimSpace(in.Code)
	if code == "" {
		return nil, fmt.Errorf("%w: mcpCode 不能为空", ErrInvalidPlatform)
	}

	warn := ""
	transport := strings.ToLower(strings.TrimSpace(in.Transport))
	if transport == "" {
		transport = mcp.TransportHTTP
	}
	if transport == TransportSSE {
		// 本地 MCP 客户端只实现 Streamable HTTP（POST JSON-RPC），不实现 SSE 长连接
		transport = mcp.TransportHTTP
		warn = "本地 MCP 客户端仅支持 Streamable HTTP，已自动改用 /mcp 地址（千问同一服务同时提供 /mcp 与 /sse）"
	}
	endpoint, err := p.ResolveEndpoint(code, transport)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = p.LocalServerName(code)
	}
	ns := p.EffectiveNamespace(in.Namespace)

	res := &ImportResult{ServerName: name, Namespace: ns, Endpoint: endpoint, Message: warn}

	// 已接入过：复用并重新同步工具
	if existing, ferr := u.findServer(ctx, name); ferr == nil && existing != nil {
		res.ServerID = existing.ID
		res.Reused = true
		res.Namespace = existing.EffectiveNamespace()
		res.Endpoint = existing.Endpoint
		if st, cerr := u.mcp.ConnectServer(ctx, existing.ID); cerr != nil || !st.Running {
			msg := "已存在同名服务，但重新连接失败"
			if cerr != nil {
				msg += "：" + cerr.Error()
			} else if st.ErrorMsg != "" {
				msg += "：" + st.ErrorMsg
			}
			res.Message = joinMessage(res.Message, msg)
			return res, nil
		}
		res.ToolCount = u.syncTools(ctx, existing.ID, in.SyncTools, res)
		return res, nil
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = fmt.Sprintf("来自 %s 的 MCP 服务：%s", p.Name, code)
	}
	created, err := u.mcp.CreateServer(ctx, mcp.ServerInput{
		Name:                name,
		Namespace:           ns,
		Transport:           mcp.TransportHTTP,
		Description:         desc,
		Endpoint:            endpoint,
		Headers:             p.AuthHeaders(),
		HealthCheckInterval: 60,
		Enabled:             &enabled,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: 创建本地 MCP 服务失败：%v", ErrImportFailed, err)
	}
	res.ServerID = created.ID

	if !enabled {
		res.Message = joinMessage(res.Message, "服务已创建但未启用，启用后才会连接")
		return res, nil
	}
	if _, cerr := u.mcp.ConnectServer(ctx, created.ID); cerr != nil {
		_ = u.mcp.DeleteServer(ctx, created.ID) // 回滚
		return nil, fmt.Errorf("%w: 连接失败（确认 API Key 有效、该 MCP 服务已在平台开通）：%v", ErrImportFailed, cerr)
	}
	res.ToolCount = u.syncTools(ctx, created.ID, in.SyncTools, res)
	return res, nil
}

// Test 连通性测试：优先打目录接口；目录不可用但接入模板可用时仍视为「可接入」。
// 返回的 TestResult.OK 表示「能不能用这个平台拉服务」，而不是「HTTP 一定通」。
func (u *UseCase) Test(ctx context.Context, name string) (*TestResult, error) {
	p, err := u.resolvePlatform(ctx, name)
	if err != nil {
		return nil, err
	}
	res := &TestResult{AuthConfigured: strings.TrimSpace(p.APIKey) != "" || p.AuthScheme == AuthNone}
	start := time.Now()
	items, serr := u.cat.Search(ctx, *p, "", 1)
	res.LatencyMS = time.Since(start).Milliseconds()
	res.SampleCount = len(items)
	res.CatalogOK = serr == nil

	if serr == nil {
		res.OK = true
		if len(items) == 0 {
			res.Message = "目录接口可用，但没有返回条目（可能是关键词为空或平台侧未上架服务）"
		} else {
			res.Message = fmt.Sprintf("目录接口可用，返回 %d 条", len(items))
		}
		return res, nil
	}

	// 目录不行，再看接入模板是否可用（千问的目录接口未公开，但按编码接入是稳的）
	if ep, rerr := p.ResolveEndpoint("probe", mcp.TransportHTTP); rerr == nil {
		res.OK = true
		res.Message = fmt.Sprintf("目录接口不可用（%v）；接入模板可用，可直接按编码接入，如 %s", serr, ep)
		return res, nil
	}
	res.OK = false
	res.Message = serr.Error()
	return res, nil
}

// —— 内部 ——

// resolvePlatform 空名时取默认平台，其次取第一个启用的。
func (u *UseCase) resolvePlatform(ctx context.Context, name string) (*Platform, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		return u.repo.Get(ctx, name)
	}
	list, err := u.repo.List(ctx, Filter{Limit: 100})
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Enabled && list[i].IsDefault {
			return &list[i], nil
		}
	}
	for i := range list {
		if list[i].Enabled {
			return &list[i], nil
		}
	}
	return nil, ErrNoPlatform
}

func (u *UseCase) installedSet(ctx context.Context) (map[string]bool, error) {
	servers, err := u.mcp.ListServers(ctx, mcp.ServerFilter{Limit: 1000})
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(servers))
	for _, s := range servers {
		set[s.Name] = true
	}
	return set, nil
}

func (u *UseCase) findServer(ctx context.Context, name string) (*mcp.Server, error) {
	servers, err := u.mcp.ListServers(ctx, mcp.ServerFilter{Keyword: name, Limit: 100})
	if err != nil {
		return nil, err
	}
	for i := range servers {
		if servers[i].Name == name {
			return &servers[i], nil
		}
	}
	return nil, nil
}

// syncTools 同步工具清单，返回工具数；失败不致命，写进 Message。
func (u *UseCase) syncTools(ctx context.Context, serverID uint64, want *bool, res *ImportResult) int {
	if want != nil && !*want {
		return 0
	}
	out, err := u.mcp.SyncTools(ctx, serverID)
	if err != nil {
		res.Message = joinMessage(res.Message, "工具同步失败（可稍后在 MCP 页手动同步）："+err.Error())
		return 0
	}
	n := 0
	if out != nil {
		n = out.Fetched
	}
	if out != nil && out.Warning != "" {
		res.Message = joinMessage(res.Message, out.Warning)
	}
	if n == 0 {
		res.Message = joinMessage(res.Message, "连接正常但该服务没有暴露工具")
	}
	return n
}

// syncSkillHub 平台登记技能市场：skill_base_url 非空时幂等 upsert 一个 hub。
// 返回非空字符串表示告警（平台本身仍然保存成功）。
func (u *UseCase) syncSkillHub(ctx context.Context, p *Platform) string {
	base := strings.TrimSpace(p.SkillBaseURL)
	if base == "" || u.hubs == nil {
		p.SkillHub = ""
		return ""
	}
	hubName := strings.TrimSpace(p.SkillHub)
	if hubName == "" {
		hubName = p.Name + "-skills"
	}
	desc := fmt.Sprintf("由 AI 平台「%s」自动登记（技能源）", p.Name)
	enabled := true

	hubs, err := u.hubs.ListHubs(ctx)
	if err != nil {
		return "查询技能市场失败：" + err.Error()
	}
	for _, h := range hubs {
		if h.Name != hubName {
			continue
		}
		baseCopy := base
		tokenCopy := p.APIKey
		descCopy := desc
		if _, uerr := u.hubs.UpdateHub(ctx, hubName, skill.HubPatch{
			BaseURL:     &baseCopy,
			Token:       &tokenCopy,
			Enabled:     &enabled,
			Description: &descCopy,
		}); uerr != nil {
			return "更新技能市场失败：" + uerr.Error()
		}
		p.SkillHub = hubName
		return ""
	}
	if _, cerr := u.hubs.CreateHub(ctx, skill.HubInput{
		Name:        hubName,
		BaseURL:     base,
		Token:       p.APIKey,
		Enabled:     &enabled,
		Description: desc,
	}); cerr != nil {
		return "登记技能市场失败：" + cerr.Error()
	}
	p.SkillHub = hubName
	return ""
}

// inheritInput 把库里现有的值填进「本次没传」的字段。
//
// 目的：Normalize 会按 kind 补默认值（如 qwen 的官方地址），若不先继承现值，
// 一次只改描述的更新就会把用户自定义的 base_url / 路径模板重置回官方默认。
func inheritInput(cur *Platform, in *Input) {
	str := func(v string) *string { return &v }
	if in.BaseURL == nil {
		in.BaseURL = str(cur.BaseURL)
	}
	if in.AuthScheme == nil {
		in.AuthScheme = str(cur.AuthScheme)
	}
	if in.AuthHeader == nil {
		in.AuthHeader = str(cur.AuthHeader)
	}
	if in.CatalogPath == nil {
		in.CatalogPath = str(cur.CatalogPath)
	}
	if in.StreamablePath == nil {
		in.StreamablePath = str(cur.StreamablePath)
	}
	if in.SSEPath == nil {
		in.SSEPath = str(cur.SSEPath)
	}
	if in.SkillBaseURL == nil {
		in.SkillBaseURL = str(cur.SkillBaseURL)
	}
	if in.Description == nil {
		in.Description = str(cur.Description)
	}
	if in.Enabled == nil {
		v := cur.Enabled
		in.Enabled = &v
	}
}

// applyInput 把入参合并到 DO（未传的字段保持原值）。
func applyInput(p *Platform, in *Input) {
	p.Kind = in.Kind
	if in.BaseURL != nil {
		p.BaseURL = strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
	}
	if in.APIKey != nil {
		key := strings.TrimSpace(*in.APIKey)
		if key != "" { // 空串表示「不改」，避免前端回显脱敏值把 key 清掉
			p.APIKey = key
		}
	}
	if in.AuthScheme != nil {
		p.AuthScheme = *in.AuthScheme
	}
	if in.AuthHeader != nil {
		p.AuthHeader = strings.TrimSpace(*in.AuthHeader)
	}
	if in.Headers != nil {
		p.Headers = in.Headers
	}
	if in.CatalogPath != nil {
		p.CatalogPath = strings.TrimSpace(*in.CatalogPath)
	}
	if in.StreamablePath != nil {
		p.StreamablePath = strings.TrimSpace(*in.StreamablePath)
	}
	if in.SSEPath != nil {
		p.SSEPath = strings.TrimSpace(*in.SSEPath)
	}
	if in.SkillBaseURL != nil {
		p.SkillBaseURL = strings.TrimRight(strings.TrimSpace(*in.SkillBaseURL), "/")
	}
	if in.Description != nil {
		p.Description = strings.TrimSpace(*in.Description)
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.IsDefault != nil {
		p.IsDefault = *in.IsDefault
	}
}

func joinMessage(cur, add string) string {
	if strings.TrimSpace(add) == "" {
		return cur
	}
	if strings.TrimSpace(cur) == "" {
		return add
	}
	return cur + "；" + add
}
