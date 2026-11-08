package mcp

// UseCase MCP 管理用例：服务 CRUD、连接/断开、工具同步、语义召回、工具调用。
//
// 同步链路：tools/list → MySQL upsert → embedding → Milvus → Redis。
// 任何一步失败都保留已完成的部分，并在返回值里给出可读原因：
// 数据库写成功但向量化失败，工具照样能在列表里看到（vectorized=false）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go_agent/internal/biz/history"

	"github.com/go-kratos/kratos/v3/log"
)

const (
	// defaultTopK 语义召回默认条数
	defaultTopK = 8
	// maxTopK 召回上限（避免前端传大数字打爆向量库）
	maxTopK = 50
	// defaultListLimit 列表默认条数
	defaultListLimit = 200
	// maxToolNameLen unique_name 字段上限
	maxToolNameLen = 256
)

// UseCase MCP 用例
type UseCase struct {
	repo  Repo
	vec   VectorStore
	cache Cache
	emb   Embedder
	mgr   *Manager
	// proxy Swagger/OpenAPI 代理引擎（transport=proxy）
	proxy *proxyEngine
	// hist 统一操作历史（MCP 调用记录，可为 nil：不记录）
	hist history.Recorder

	// clientOpts 运行时 http.Client 覆盖项（内存态，不落库）
	optMu      sync.RWMutex
	clientOpts map[uint64]ClientOptions

	vecMu   sync.Mutex
	vecInit bool
}

// New 创建用例
func New(repo Repo, vec VectorStore, cache Cache, dialer Dialer, clients ClientProvider, emb Embedder, hist history.Recorder) (*UseCase, error) {
	if repo == nil || vec == nil || cache == nil || dialer == nil || emb == nil {
		return nil, fmt.Errorf("MCP 用例初始化失败: 依赖缺失")
	}
	return &UseCase{
		repo:       repo,
		vec:        vec,
		cache:      cache,
		emb:        emb,
		mgr:        NewManager(dialer, clients),
		proxy:      newProxyEngine(),
		hist:       hist,
		clientOpts: make(map[uint64]ClientOptions),
	}, nil
}

// Manager 暴露管理器（进程退出时由 main 调用 CloseAll）
func (u *UseCase) Manager() *Manager { return u.mgr }

// —— 服务 ——

// ListServers 服务列表（带运行状态与工具数）
func (u *UseCase) ListServers(ctx context.Context, f ServerFilter) ([]Server, error) {
	list, err := u.repo.ListServers(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uint64, 0, len(list))
	for i := range list {
		ids = append(ids, list[i].ID)
	}
	statuses := u.cache.GetStatuses(ctx, ids)
	for i := range list {
		list[i].Status = u.mergeStatus(list[i], statuses[list[i].ID])
	}
	return list, nil
}

// GetServer 服务详情（含工具明细）
func (u *UseCase) GetServer(ctx context.Context, id uint64) (*Server, error) {
	s, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return nil, err
	}
	tools, err := u.repo.ListTools(ctx, ToolFilter{ServerID: id, Limit: 500})
	if err == nil {
		s.Tools = tools
		s.ToolCount = len(tools)
	}
	st, _ := u.cache.GetStatus(ctx, id)
	s.Status = u.mergeStatus(*s, st)
	return s, nil
}

// CreateServer 创建服务
func (u *UseCase) CreateServer(ctx context.Context, in ServerInput) (*Server, error) {
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	s := &Server{
		Name:                in.Name,
		Namespace:           in.Namespace,
		Transport:           in.Transport,
		Enabled:             enabled,
		Description:         in.Description,
		Endpoint:            in.Endpoint,
		Headers:             in.Headers,
		Command:             in.Command,
		Args:                in.Args,
		Env:                 in.Env,
		HealthCheckInterval: in.HealthCheckInterval,
		ProxyConfig:         in.ProxyConfig,
	}
	if err := u.repo.CreateServer(ctx, s); err != nil {
		return nil, err
	}
	return u.GetServer(ctx, s.ID)
}

// UpdateServer 更新服务；配置可能变了，先断开旧实例
func (u *UseCase) UpdateServer(ctx context.Context, id uint64, in ServerInput) (*Server, error) {
	cur, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	cur.Name = in.Name
	cur.Namespace = in.Namespace
	cur.Transport = in.Transport
	cur.Enabled = enabled
	cur.Description = in.Description
	cur.Endpoint = in.Endpoint
	cur.Headers = in.Headers
	cur.Command = in.Command
	cur.Args = in.Args
	cur.Env = in.Env
	cur.HealthCheckInterval = in.HealthCheckInterval
	cur.ProxyConfig = in.ProxyConfig

	if err := u.repo.UpdateServer(ctx, cur); err != nil {
		return nil, err
	}
	_ = u.mgr.Disconnect(ctx, id)
	u.saveStatus(ctx, *cur, func(st *Status) {
		st.Running = false
		st.ErrorMsg = ""
		st.HTTPClient = ""
		st.ProtocolVersion = ""
	})
	return u.GetServer(ctx, id)
}

// DeleteServer 删除服务：断开实例、清理工具、向量与缓存
func (u *UseCase) DeleteServer(ctx context.Context, id uint64) error {
	s, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return err
	}
	tools, err := u.repo.ListTools(ctx, ToolFilter{ServerID: id, Limit: 1000})
	if err != nil {
		return err
	}
	ids := make([]uint64, 0, len(tools))
	for _, t := range tools {
		ids = append(ids, t.ID)
	}

	_ = u.mgr.Disconnect(ctx, id)
	for _, t := range tools {
		if err := u.repo.DeleteTool(ctx, t.ID); err != nil {
			return err
		}
	}
	if len(ids) > 0 {
		if err := u.vec.Delete(ctx, ids); err != nil {
			log.Warn("删除工具向量失败", "server", s.Name, "err", err)
		}
		u.cache.DeleteTools(ctx, ids)
		u.cache.RemoveFromNamespace(ctx, s.EffectiveNamespace(), ids)
	}
	if err := u.repo.DeleteServer(ctx, id); err != nil {
		return err
	}
	u.cache.DeleteStatus(ctx, id)
	u.optMu.Lock()
	delete(u.clientOpts, id)
	u.optMu.Unlock()
	return nil
}

// SetServerEnabled 启停服务；停用时断开实例
func (u *UseCase) SetServerEnabled(ctx context.Context, id uint64, enabled bool) (*Server, error) {
	s, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := u.repo.SetServerEnabled(ctx, id, enabled); err != nil {
		return nil, err
	}
	if !enabled {
		_ = u.mgr.Disconnect(ctx, id)
		u.saveStatus(ctx, *s, func(st *Status) { st.Running = false; st.ErrorMsg = "" })
	}
	return u.GetServer(ctx, id)
}

// ConnectServer 建立连接（HTTP 模式使用运行时注入的 client）
func (u *UseCase) ConnectServer(ctx context.Context, id uint64) (Status, error) {
	s, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return Status{}, err
	}
	if !s.Enabled {
		return Status{}, fmt.Errorf("%w: %s", ErrServerDisabled, s.Name)
	}
	// 代理没有长连接：连上 = 文档可解析 + 服务可达
	if s.Transport == TransportProxy {
		if err := u.proxy.Ping(ctx, *s); err != nil {
			u.markError(ctx, *s, err)
			return u.statusOf(ctx, *s), fmt.Errorf("代理服务 %s 不可用: %w", s.Name, err)
		}
		return u.saveStatus(ctx, *s, func(st *Status) {
			st.Running = true
			st.LastPingAt = time.Now()
			st.ErrorMsg = ""
			st.HTTPClient = describeClient(*s, u.options(id))
		}), nil
	}
	info, err := u.mgr.Connect(ctx, *s, u.options(id))
	if err != nil {
		u.markError(ctx, *s, err)
		return u.statusOf(ctx, *s), fmt.Errorf("连接 MCP 服务 %s 失败: %w", s.Name, err)
	}
	st := u.saveStatus(ctx, *s, func(st *Status) {
		st.Running = true
		st.LastPingAt = time.Now()
		st.ErrorMsg = ""
		st.ProtocolVersion = info.ProtocolVersion
		st.ServerVersion = info.Version
		st.HTTPClient = describeClient(*s, u.options(id))
	})
	return st, nil
}

// DisconnectServer 断开连接
func (u *UseCase) DisconnectServer(ctx context.Context, id uint64) (Status, error) {
	s, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return Status{}, err
	}
	if err := u.mgr.Disconnect(ctx, id); err != nil {
		return Status{}, err
	}
	st := u.saveStatus(ctx, *s, func(st *Status) {
		st.Running = false
		st.HTTPClient = ""
		st.ProtocolVersion = ""
	})
	return st, nil
}

// PingServer 探活：未连接时先连接（用户点「探活」的预期是「让我知道现在通不通」）
func (u *UseCase) PingServer(ctx context.Context, id uint64) (Status, error) {
	s, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return Status{}, err
	}
	if !s.Enabled {
		return Status{}, fmt.Errorf("%w: %s", ErrServerDisabled, s.Name)
	}
	if s.Transport == TransportProxy {
		if err := u.proxy.Ping(ctx, *s); err != nil {
			u.markError(ctx, *s, err)
			return u.statusOf(ctx, *s), err
		}
		return u.saveStatus(ctx, *s, func(st *Status) {
			st.Running = true
			st.LastPingAt = time.Now()
			st.ErrorMsg = ""
			st.HTTPClient = describeClient(*s, u.options(id))
		}), nil
	}
	cli, err := u.mgr.Ensure(ctx, *s, u.options(id))
	if err != nil {
		u.markError(ctx, *s, err)
		return u.statusOf(ctx, *s), err
	}
	if err := cli.Ping(ctx); err != nil {
		u.markError(ctx, *s, err)
		return u.statusOf(ctx, *s), err
	}
	st := u.saveStatus(ctx, *s, func(st *Status) {
		st.Running = true
		st.LastPingAt = time.Now()
		st.ErrorMsg = ""
		st.HTTPClient = describeClient(*s, u.options(id))
	})
	return st, nil
}

// SetClientOptions 运行时替换 http.Client 参数并重连（不落库）
func (u *UseCase) SetClientOptions(ctx context.Context, id uint64, opts ClientOptions) (Status, error) {
	s, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return Status{}, err
	}
	if opts.TimeoutSec < 0 {
		opts.TimeoutSec = 0
	}
	u.optMu.Lock()
	u.clientOpts[id] = opts
	u.optMu.Unlock()

	cur := u.statusOf(ctx, *s)
	if !cur.Running {
		cur.HTTPClient = describeClient(*s, opts)
		return cur, nil
	}
	if _, err := u.mgr.Connect(ctx, *s, opts); err != nil {
		u.markError(ctx, *s, err)
		return u.statusOf(ctx, *s), fmt.Errorf("按新参数重连失败: %w", err)
	}
	st := u.saveStatus(ctx, *s, func(st *Status) {
		st.Running = true
		st.ErrorMsg = ""
		st.HTTPClient = describeClient(*s, opts)
	})
	return st, nil
}

// Namespaces 全部命名空间（前端过滤下拉）
func (u *UseCase) Namespaces(ctx context.Context) ([]string, error) {
	list, err := u.repo.ListServers(ctx, ServerFilter{Limit: 500})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, s := range list {
		ns := s.EffectiveNamespace()
		if seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, ns)
	}
	return out, nil
}

// —— 工具同步 ——

// SyncTools 拉取远端工具清单并同步到 MySQL / Milvus / Redis
func (u *UseCase) SyncTools(ctx context.Context, id uint64) (*SyncResult, error) {
	s, err := u.repo.GetServer(ctx, id)
	if err != nil {
		return nil, err
	}
	res := &SyncResult{ServerID: id, ServerName: s.Name}
	if !s.Enabled {
		return res, fmt.Errorf("%w: %s", ErrServerDisabled, s.Name)
	}
	var tools []Tool
	if s.Transport == TransportProxy {
		// 代理：解析文档生成工具（不走 MCP 握手），并把解析快照回写配置
		built, cfg, base, perr := u.proxy.BuildTools(ctx, *s)
		if perr != nil {
			u.markError(ctx, *s, perr)
			return res, fmt.Errorf("解析代理文档失败: %w", perr)
		}
		tools = built
		if cfg != nil {
			cfg.BaseURL = base
			if encoded, eerr := EncodeProxySpec(cfg); eerr == nil {
				s.ProxyConfig = encoded
			}
			if strings.TrimSpace(s.Endpoint) == "" {
				s.Endpoint = base
			}
			if uerr := u.repo.UpdateServer(ctx, s); uerr != nil {
				log.Warn("回写代理配置失败", "server", s.Name, "err", uerr)
			}
		}
	} else {
		cli, err := u.mgr.Ensure(ctx, *s, u.options(id))
		if err != nil {
			u.markError(ctx, *s, err)
			return res, fmt.Errorf("连接 MCP 服务 %s 失败: %w", s.Name, err)
		}
		remote, err := cli.ListTools(ctx)
		if err != nil {
			u.markError(ctx, *s, err)
			return res, fmt.Errorf("拉取工具列表失败: %w", err)
		}
		tools = buildTools(*s, remote)
	}
	res.Fetched = len(tools)

	stat, saved, err := u.repo.UpsertTools(ctx, id, tools)
	if err != nil {
		return res, fmt.Errorf("写入工具表失败: %w", err)
	}
	res.Added, res.Updated, res.Removed = stat.Added, stat.Updated, stat.Removed

	// 新增/内容变化的工具需要重新向量化；失败只给警告，不影响元数据落库
	targets := make([]Tool, 0, len(saved))
	for _, t := range saved {
		if t.Enabled && !t.Vectorized {
			targets = append(targets, t)
		}
	}
	if err := u.pushVectors(ctx, targets); err != nil {
		res.Warning = err.Error()
	} else {
		res.Vectorized = len(targets)
	}

	// 缓存与命名空间索引
	_ = u.cache.PutTools(ctx, saved)
	if len(stat.RemovedIDs) > 0 {
		u.cache.DeleteTools(ctx, stat.RemovedIDs)
		u.cache.RemoveFromNamespace(ctx, s.EffectiveNamespace(), stat.RemovedIDs)
		if err := u.vec.Delete(ctx, stat.RemovedIDs); err != nil {
			log.Warn("删除失效工具向量失败", "server", s.Name, "err", err)
		}
	}
	u.cache.AddToNamespace(ctx, s.EffectiveNamespace(), idsOf(saved))

	u.saveStatus(ctx, *s, func(st *Status) {
		st.Running = true
		st.LastSyncAt = time.Now()
		st.LastPingAt = time.Now()
		st.ToolCount = len(saved)
		st.ErrorMsg = ""
		st.HTTPClient = describeClient(*s, u.options(id))
	})
	return res, nil
}

// PreviewProxy 解析 Swagger/OpenAPI 文档，返回接口清单（不落库，保存前预览用）。
func (u *UseCase) PreviewProxy(ctx context.Context, proxyConfig string) (*ProxyPreview, error) {
	cfg, err := DecodeProxySpec(proxyConfig)
	if err != nil {
		return nil, err
	}
	doc, base, err := u.proxy.Preview(ctx, Server{Transport: TransportProxy, ProxyConfig: proxyConfig}, cfg)
	if err != nil {
		return nil, err
	}
	return &ProxyPreview{
		BaseURL:  base,
		Title:    doc.title,
		Version:  doc.version,
		Ops:      doc.ops,
		Warnings: doc.warnings,
	}, nil
}

// SyncAll 同步全部启用的服务（单个失败不影响其它）
func (u *UseCase) SyncAll(ctx context.Context) ([]SyncResult, error) {
	servers, err := u.repo.ListServers(ctx, ServerFilter{Limit: 500})
	if err != nil {
		return nil, err
	}
	out := make([]SyncResult, 0, len(servers))
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		res, err := u.SyncTools(ctx, s.ID)
		if err != nil {
			out = append(out, SyncResult{ServerID: s.ID, ServerName: s.Name, Warning: err.Error()})
			continue
		}
		out = append(out, *res)
	}
	return out, nil
}

// ListTools 工具列表
func (u *UseCase) ListTools(ctx context.Context, f ToolFilter) ([]Tool, error) {
	if f.Limit <= 0 {
		f.Limit = defaultListLimit
	}
	return u.repo.ListTools(ctx, f)
}

// GetTool 工具详情
func (u *UseCase) GetTool(ctx context.Context, id uint64) (*Tool, error) {
	return u.repo.GetTool(ctx, id)
}

// ResolveTool 按「id 或唯一名」定位工具（CallTool 的入参口径）。
//
// 单独抽出来是给调用前的闸门用：审批要拿工具的风险等级与唯一名做绑定，
// 不能等到真正发请求时才知道调的是哪个工具。
func (u *UseCase) ResolveTool(ctx context.Context, toolID uint64, uniqueName string) (*Tool, error) {
	if toolID > 0 {
		return u.repo.GetTool(ctx, toolID)
	}
	if strings.TrimSpace(uniqueName) == "" {
		return nil, ErrToolNotFound
	}
	return u.repo.GetToolByUniqueName(ctx, uniqueName)
}

// SetToolEnabled 启停工具，并把状态同步进向量库（保证 is_enabled 标量可信）
func (u *UseCase) SetToolEnabled(ctx context.Context, id uint64, enabled bool) (*Tool, error) {
	if err := u.repo.SetToolEnabled(ctx, id, enabled); err != nil {
		return nil, err
	}
	t, err := u.repo.GetTool(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := u.pushVectors(ctx, []Tool{*t}); err != nil {
		// 向量同步失败不影响启停：召回结果还会按数据库的 enabled 过滤一次
		log.Warn("工具向量状态同步失败", "tool", t.UniqueName, "err", err)
		if enabled {
			_ = u.repo.MarkVectorized(ctx, []uint64{t.ID}, false)
			t.Vectorized = false
		}
	}
	_ = u.cache.PutTools(ctx, []Tool{*t})
	return t, nil
}

// DeleteTool 删除工具（元数据、向量、缓存）
func (u *UseCase) DeleteTool(ctx context.Context, id uint64) error {
	t, err := u.repo.GetTool(ctx, id)
	if err != nil {
		return err
	}
	if err := u.repo.DeleteTool(ctx, id); err != nil {
		return err
	}
	if err := u.vec.Delete(ctx, []uint64{id}); err != nil {
		log.Warn("删除工具向量失败", "tool", t.UniqueName, "err", err)
	}
	u.cache.DeleteTools(ctx, []uint64{id})
	u.cache.RemoveFromNamespace(ctx, t.Namespace, []uint64{id})
	return nil
}

// —— 召回与调用 ——

// SearchTools 语义召回工具：Milvus 取 id → Redis 取详情 → MySQL 兜底
func (u *UseCase) SearchTools(ctx context.Context, query string, limit int, namespace string) ([]SearchHit, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, ErrEmptyQuery
	}
	if limit <= 0 {
		limit = defaultTopK
	}
	if limit > maxTopK {
		limit = maxTopK
	}
	vecs, err := u.emb.Embed(ctx, []string{q})
	if err != nil {
		return nil, fmt.Errorf("查询向量化失败: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("查询向量化返回空向量")
	}
	// 多取一些：禁用工具会在下面被过滤掉
	hits, err := u.vec.Search(ctx, vecs[0], limit*2, VectorFilter{
		Namespace:   strings.TrimSpace(namespace),
		EnabledOnly: true,
	})
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}

	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ToolID)
	}
	cached, err := u.cache.GetTools(ctx, ids)
	if err != nil {
		cached = map[uint64]Tool{}
	}
	missing := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if _, ok := cached[id]; !ok {
			missing = append(missing, id)
		}
	}
	fromDB := make(map[uint64]Tool, len(missing))
	if len(missing) > 0 {
		list, err := u.repo.GetTools(ctx, missing)
		if err != nil {
			return nil, err
		}
		backfill := make([]Tool, 0, len(list))
		for _, t := range list {
			fromDB[t.ID] = t
			backfill = append(backfill, t)
		}
		_ = u.cache.PutTools(ctx, backfill) // 回填缓存，下次直接命中
	}

	out := make([]SearchHit, 0, limit)
	for _, h := range hits {
		t, ok := cached[h.ToolID]
		source := "redis"
		if !ok {
			t, ok = fromDB[h.ToolID]
			source = "mysql"
		}
		if !ok || !t.Enabled {
			continue
		}
		out = append(out, SearchHit{Tool: t, Score: h.Score, Source: source})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CallTool 调用工具：toolID 与 uniqueName 二选一。
// 每次调用异步落一条 MCP 调用历史（op_histories），任务内调用经 WithTask 自动关联 task。
func (u *UseCase) CallTool(ctx context.Context, toolID uint64, uniqueName string, args map[string]any) (res *CallResult, err error) {
	start := time.Now()
	refName := uniqueName
	defer func() {
		if u.hist == nil {
			return
		}
		rec := &history.Record{
			Kind:      history.KindMCPCall,
			Ref:       refName,
			OK:        err == nil,
			LatencyMS: time.Since(start).Milliseconds(),
		}
		if argsJSON, e := json.Marshal(args); e == nil {
			rec.Query = string(argsJSON)
		}
		if err != nil {
			rec.Summary = err.Error()
		} else if res != nil {
			rec.Summary = callResultSummary(res)
		}
		u.hist.Record(ctx, rec)
	}()

	var t *Tool
	if toolID > 0 {
		t, err = u.repo.GetTool(ctx, toolID)
	} else {
		t, err = u.repo.GetToolByUniqueName(ctx, uniqueName)
	}
	if err != nil {
		return nil, err
	}
	refName = t.UniqueName
	if !t.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrToolDisabled, t.UniqueName)
	}
	s, err := u.repo.GetServer(ctx, t.ServerID)
	if err != nil {
		return nil, err
	}
	if !s.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrServerDisabled, s.Name)
	}
	// 代理：按 route 映射拼 HTTP 请求并注入鉴权
	if s.Transport == TransportProxy {
		r, cerr := u.proxy.Call(ctx, *s, *t, args)
		if cerr != nil {
			u.markError(ctx, *s, cerr)
			return nil, fmt.Errorf("调用接口 %s 失败: %w", t.UniqueName, cerr)
		}
		u.saveStatus(ctx, *s, func(st *Status) {
			st.Running = true
			st.LastPingAt = time.Now()
			st.ErrorMsg = ""
		})
		return r, nil
	}
	cli, err := u.mgr.Ensure(ctx, *s, u.options(s.ID))
	if err != nil {
		u.markError(ctx, *s, err)
		return nil, fmt.Errorf("连接 MCP 服务 %s 失败: %w", s.Name, err)
	}
	r, err := cli.CallTool(ctx, t.ToolName, args)
	if err != nil {
		u.markError(ctx, *s, err)
		return nil, fmt.Errorf("调用工具 %s 失败: %w", t.UniqueName, err)
	}
	u.saveStatus(ctx, *s, func(st *Status) {
		st.Running = true
		st.LastPingAt = time.Now()
		st.ErrorMsg = ""
	})
	return &r, nil
}

// callResultSummary 调用结果摘要（文本内容拼接，截断到 500 字符内）
func callResultSummary(r *CallResult) string {
	var b strings.Builder
	if r.Structured != "" {
		b.WriteString(r.Structured)
	}
	for _, p := range r.Content {
		if b.Len() > 0 {
			b.WriteString(" | ")
		}
		if p.Text != "" {
			b.WriteString(p.Text)
		} else if p.Data != "" {
			b.WriteString(fmt.Sprintf("[%s %d 字]", p.Type, len(p.Data)))
		}
	}
	out := strings.TrimSpace(b.String())
	if r.IsError {
		out = "[isError] " + out
	}
	rs := []rune(out)
	if len(rs) > 500 {
		out = string(rs[:500])
	}
	return out
}

// Stats 规模统计
func (u *UseCase) Stats(ctx context.Context) (Stats, error) {
	st, err := u.repo.Stats(ctx)
	if err != nil {
		return st, err
	}
	st.Running = u.mgr.Running()
	return st, nil
}

// —— 内部工具 ——

// pushVectors 重新计算并写入给定工具的向量（含 is_enabled 标量）。
// 空列表直接返回：调用方不必判断。
func (u *UseCase) pushVectors(ctx context.Context, tools []Tool) error {
	if len(tools) == 0 {
		return nil
	}
	if err := u.ensureVectorStore(ctx); err != nil {
		return fmt.Errorf("工具向量集合不可用: %w", err)
	}
	texts := make([]string, 0, len(tools))
	for _, t := range tools {
		texts = append(texts, t.EmbeddingText())
	}
	vecs, err := u.emb.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("%d 个工具向量化失败（元数据已保存，向量未更新）: %w", len(tools), err)
	}
	if len(vecs) != len(tools) {
		return fmt.Errorf("向量数量不匹配: %d 个工具 / %d 条向量", len(tools), len(vecs))
	}
	if err := u.vec.Upsert(ctx, tools, vecs); err != nil {
		return fmt.Errorf("写入工具向量失败: %w", err)
	}
	_ = u.repo.MarkVectorized(ctx, idsOf(tools), true)
	for i := range tools {
		tools[i].Vectorized = true
	}
	return nil
}

func (u *UseCase) ensureVectorStore(ctx context.Context) error {
	u.vecMu.Lock()
	defer u.vecMu.Unlock()
	if u.vecInit {
		return nil
	}
	if err := u.vec.EnsureCollection(ctx); err != nil {
		return err
	}
	u.vecInit = true
	return nil
}

// buildTools 远端工具 → DO（唯一名 = 命名空间__工具名）
func buildTools(s Server, remote []RemoteTool) []Tool {
	ns := s.EffectiveNamespace()
	out := make([]Tool, 0, len(remote))
	seen := make(map[string]bool, len(remote))
	for _, rt := range remote {
		unique := uniqueToolName(ns, rt.Name)
		if seen[unique] {
			continue
		}
		seen[unique] = true
		out = append(out, Tool{
			ServerID:     s.ID,
			ServerName:   s.Name,
			Namespace:    ns,
			ToolName:     rt.Name,
			UniqueName:   unique,
			DisplayName:  firstNonEmpty(rt.Title, rt.Name),
			Description:  rt.Description,
			InputSchema:  compactJSON(rt.InputSchema),
			OutputSchema: compactJSON(rt.OutputSchema),
			Tags:         annotationTags(rt.Annotations),
			Enabled:      true,
		})
	}
	return out
}

// uniqueToolName 拼接全局唯一名，超长截断（优先保留工具名）
func uniqueToolName(namespace, tool string) string {
	ns := sanitizeName(namespace)
	name := sanitizeName(tool)
	if ns == "" {
		ns = DefaultNamespace
	}
	full := ns + "__" + name
	r := []rune(full)
	if len(r) <= maxToolNameLen {
		return full
	}
	// 截断命名空间，保留完整的工具名
	keep := maxToolNameLen - len([]rune(name)) - 2
	if keep < 1 {
		return string(r[:maxToolNameLen])
	}
	return string([]rune(ns)[:keep]) + "__" + name
}

// sanitizeName 去掉空白与分隔歧义字符
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.String()
	}
	return string(raw)
}

// annotationTags 把 MCP 工具注解（hints）转成可检索标签
func annotationTags(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	tags := make([]string, 0, 4)
	add := func(key, tag string) {
		if v, ok := m[key].(bool); ok && v {
			tags = append(tags, tag)
		}
	}
	add("readOnlyHint", "read-only")
	add("destructiveHint", "destructive")
	add("idempotentHint", "idempotent")
	add("openWorldHint", "open-world")
	return tags
}

func idsOf(tools []Tool) []uint64 {
	out := make([]uint64, 0, len(tools))
	for _, t := range tools {
		if t.ID > 0 {
			out = append(out, t.ID)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// options 取运行时覆盖项
func (u *UseCase) options(id uint64) ClientOptions {
	u.optMu.RLock()
	defer u.optMu.RUnlock()
	return u.clientOpts[id]
}

// statusOf 合并「Redis 里的持久状态」与「内存里的实时实例」
func (u *UseCase) statusOf(ctx context.Context, s Server) Status {
	st, _ := u.cache.GetStatus(ctx, s.ID)
	return u.mergeStatus(s, st)
}

func (u *UseCase) mergeStatus(s Server, st Status) Status {
	st.ServerID = s.ID
	st.ServerName = s.Name
	running := false
	if _, ok := u.mgr.Client(s.ID); ok {
		running = true
	}
	st.Running = running
	if running {
		if info, ok := u.mgr.Info(s.ID); ok {
			st.ProtocolVersion = info.ProtocolVersion
			st.ServerVersion = info.Version
		}
		if at, ok := u.mgr.ConnectedAt(s.ID); ok && st.LastPingAt.IsZero() {
			st.LastPingAt = at
		}
		if opts, ok := u.mgr.Options(s.ID); ok {
			st.HTTPClient = describeClient(s, opts)
		}
	} else {
		st.HTTPClient = ""
	}
	return st
}

// saveStatus 读旧状态 → 改字段 → 写回
func (u *UseCase) saveStatus(ctx context.Context, s Server, fn func(*Status)) Status {
	st, _ := u.cache.GetStatus(ctx, s.ID)
	st.ServerID = s.ID
	st.ServerName = s.Name
	fn(&st)
	u.cache.PutStatus(ctx, st)
	return st
}

// markError 记录失败状态（实例不可用）
func (u *UseCase) markError(ctx context.Context, s Server, err error) {
	u.saveStatus(ctx, s, func(st *Status) {
		st.Running = false
		st.ErrorMsg = err.Error()
		st.HTTPClient = ""
		st.ProtocolVersion = ""
	})
}

// describeClient 运行时客户端摘要（stdio 不涉及 http client）
func describeClient(s Server, opts ClientOptions) string {
	if s.Transport == TransportProxy {
		cfg, err := DecodeProxySpec(s.ProxyConfig)
		if err != nil {
			return "mode=proxy"
		}
		return fmt.Sprintf("mode=proxy auth=%s timeout=%dms ops=%d", cfg.Auth.Type, cfg.Request.TimeoutMS, len(cfg.Ops))
	}
	if s.Transport != TransportHTTP {
		return "mode=stdio"
	}
	timeout := opts.TimeoutSec
	if timeout <= 0 {
		timeout = 60
	}
	parts := []string{fmt.Sprintf("timeout=%ds", timeout)}
	if opts.ProxyURL != "" {
		parts = append(parts, "proxy="+opts.ProxyURL)
	}
	if opts.InsecureTLS {
		parts = append(parts, "insecure_tls")
	}
	return strings.Join(parts, " ")
}
