package llm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go_agent/internal/conf"

	"github.com/go-kratos/kratos/v3/log"
)

// Manager 多数据源管理器：实现 rag.Chatter / graphrag.Chatter，
// 同时提供数据源的在线管理能力与 LLM 调用历史记录。
type Manager struct {
	repo     ProviderRepo
	fallback *Client
	calls    CallRepo

	ttl time.Duration

	mu       sync.RWMutex
	cache    []Provider
	cachedAt time.Time
	loaded   bool
}

// NewManager 构建数据源管理器并在 MySQL 为空时用 conf 播种一个 default 数据源。
//
// 启动阶段任何存储异常都只告警：解析侧有 conf 兜底，业务不因管理面不可用而中断。
// calls 为调用历史仓库（可为 nil：不记录历史）。
func NewManager(c *conf.Data, repo ProviderRepo, calls CallRepo) *Manager {
	lc := c.GetLlm()
	ttl := lc.GetReloadTTL().AsDuration()
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	timeout := 120 * time.Second
	if lc.GetHttpTimeout() != nil && lc.GetHttpTimeout().AsDuration() > 0 {
		timeout = lc.GetHttpTimeout().AsDuration()
	}
	m := &Manager{
		repo:     repo,
		fallback: NewClient(lc),
		calls:    calls,
		ttl:      ttl,
	}
	m.seed(context.Background(), lc, timeout)
	return m
}

// seed 表为空时，把 conf 里的 llm 配置落成一个名为 default 的数据源，
// 保证「配置迁到 MySQL」不需要人工先插数据。
func (m *Manager) seed(ctx context.Context, lc *conf.Data_LLM, timeout time.Duration) {
	if m.repo == nil || strings.TrimSpace(lc.GetBaseURL()) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := m.repo.List(ctx)
	if err != nil {
		log.Error("[llm] 读取数据源失败，暂时使用配置文件兜底", "err", err)
		return
	}
	if len(list) > 0 {
		return
	}
	p := &Provider{
		Name:         "default",
		Kind:         KindCustom,
		BaseURL:      strings.TrimRight(strings.TrimSpace(lc.GetBaseURL()), "/"),
		APIKey:       strings.TrimSpace(lc.GetApiKey()),
		Models:       nonEmpty([]string{strings.TrimSpace(lc.GetModel())}),
		DefaultModel: strings.TrimSpace(lc.GetModel()),
		Temperature:  lc.GetTemperature(),
		TimeoutSec:   int(timeout.Seconds()),
		Enabled:      true,
		IsDefault:    true,
		Priority:     100,
		Scenes:       []string{SceneGeneral},
		Description:  "由 config.yaml 的 data.llm 块自动播种",
	}
	if err := m.repo.Create(ctx, p); err != nil {
		log.Error("[llm] 播种默认数据源失败", "err", err)
		return
	}
	log.Info("[llm] 已用配置播种默认数据源", "name", p.Name, "model", p.Model())
}

// —— Chatter 实现 ——

// Chat 使用默认数据源对话
func (m *Manager) Chat(ctx context.Context, system, user string) (string, error) {
	return m.chat(ctx, "", system, user, -1)
}

// ChatTemp 指定温度对话
func (m *Manager) ChatTemp(ctx context.Context, system, user string, temperature float64) (string, error) {
	return m.chat(ctx, "", system, user, temperature)
}

// ChatScene 按场景路由（rag / graph / extract / memory），失败自动切下一个数据源
func (m *Manager) ChatScene(ctx context.Context, scene, system, user string) (string, error) {
	return m.chat(ctx, scene, system, user, -1)
}

// ChatSource 指定数据源对话（Agent 默认模型数据源用）：
// name 命中启用数据源则优先调用；未命中或调用失败时回退默认链路
// （default 优先 → 其他启用源按优先级 → conf 兜底），保证业务不因单源故障中断。
func (m *Manager) ChatSource(ctx context.Context, name, system, user string, temperature float64) (string, error) {
	return m.ChatOpts(ctx, ChatOptions{Source: name, Temperature: temperature, System: system, User: user})
}

// ChatOpts 统一调用通道：数据源/场景路由 + 温度 + max_tokens + JSON 模式。
// JSONMode 开启时若数据源不支持 response_format（400 且报文提到该字段），
// 自动去掉该参数重试一次，保证老网关不被卡死。
func (m *Manager) ChatOpts(ctx context.Context, o ChatOptions) (string, error) {
	out, err := m.chatOpts(ctx, o, true)
	if err != nil && o.JSONMode && looksUnsupportedOption(err) {
		log.Warn("[llm] 数据源不支持 response_format，降级为普通文本输出重试",
			"source", o.Source, "scene", o.Scene, "err", err)
		noJSON := o
		noJSON.JSONMode = false
		out, err = m.chatOpts(ctx, noJSON, false)
	}
	return out, err
}

func (m *Manager) chatOpts(ctx context.Context, o ChatOptions, allowJSON bool) (string, error) {
	opt := CallOptions{MaxTokens: o.MaxTokens}
	if o.JSONMode && allowJSON {
		opt.JSONMode = true
	}

	// 指定数据源：命中启用源则直接调用（失败落回默认链路）
	if name := strings.TrimSpace(o.Source); name != "" {
		for _, p := range m.snapshot(ctx) {
			if p.Name != name || !p.Enabled || p.Model() == "" || strings.TrimSpace(p.BaseURL) == "" {
				continue
			}
			temp := o.Temperature
			if temp < 0 {
				temp = p.Temperature
			}
			start := time.Now()
			out, usage, err := m.clientFor(p).ChatDetailedOpts(ctx, o.System, o.User, temp, opt)
			m.record(ctx, p.Name, p.Model(), sceneOr(o.Scene), temp, o.System, o.User, out, usage, time.Since(start), err)
			if err == nil {
				return out, nil
			}
			log.Warn("[llm] 指定数据源调用失败，回退默认数据源", "provider", p.Name, "model", p.Model(), "err", err)
			break
		}
	}
	return m.chatWithOpt(ctx, o.Scene, o.System, o.User, o.Temperature, opt)
}

// chatWithOpt 候选数据源顺序调用 + conf 兜底（chat 的带参版本）
func (m *Manager) chatWithOpt(ctx context.Context, scene, system, user string, temperature float64, opt CallOptions) (string, error) {
	var lastErr error
	for _, p := range m.candidates(scene) {
		temp := temperature
		if temp < 0 {
			temp = p.Temperature
		}
		start := time.Now()
		out, usage, err := m.clientFor(p).ChatDetailedOpts(ctx, system, user, temp, opt)
		m.record(ctx, p.Name, p.Model(), sceneOr(scene), temp, system, user, out, usage, time.Since(start), err)
		if err == nil {
			return out, nil
		}
		lastErr = err
		log.Warn("[llm] 数据源调用失败，尝试下一个", "provider", p.Name, "model", p.Model(), "err", err)
	}
	if m.fallback != nil && strings.TrimSpace(m.fallback.Model()) != "" {
		temp := temperature
		if temp < 0 {
			temp = m.fallback.temp
		}
		start := time.Now()
		out, usage, err := m.fallback.ChatDetailedOpts(ctx, system, user, temp, opt)
		m.record(ctx, "fallback", m.fallback.Model(), sceneOr(scene), temp, system, user, out, usage, time.Since(start), err)
		if err == nil {
			return out, nil
		} else if lastErr == nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", ErrNoProvider
}

// looksUnsupportedOption 判断错误是否为「不支持 response_format / max_tokens」类参数错误。
// 只有这类错误才值得降级重试，网络错误/鉴权错误重试也一样失败。
func looksUnsupportedOption(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "response_format") && !strings.Contains(msg, "json_object") {
		return false
	}
	return strings.Contains(msg, "400") || strings.Contains(msg, "invalid") ||
		strings.Contains(msg, "unsupported") || strings.Contains(msg, "not support")
}

// Model 当前生效模型名（管理接口与状态展示用）
func (m *Manager) Model() string {
	cands := m.candidates(SceneGeneral)
	if len(cands) > 0 {
		return cands[0].Model()
	}
	return m.fallback.Model()
}

func (m *Manager) chat(ctx context.Context, scene, system, user string, temperature float64) (string, error) {
	var lastErr error
	for _, p := range m.candidates(scene) {
		temp := temperature
		if temp < 0 {
			temp = p.Temperature
		}
		start := time.Now()
		out, usage, err := m.clientFor(p).ChatTempDetailed(ctx, system, user, temp)
		m.record(ctx, p.Name, p.Model(), scene, temp, system, user, out, usage, time.Since(start), err)
		if err == nil {
			return out, nil
		}
		lastErr = err
		log.Warn("[llm] 数据源调用失败，尝试下一个", "provider", p.Name, "model", p.Model(), "err", err)
	}
	// 全部数据源不可用时回落到 conf 兜底（K8s/本地单机场景常见）
	if m.fallback != nil && strings.TrimSpace(m.fallback.Model()) != "" {
		temp := temperature
		if temp < 0 {
			temp = m.fallback.temp
		}
		start := time.Now()
		out, usage, err := m.fallback.ChatTempDetailed(ctx, system, user, temp)
		m.record(ctx, "fallback", m.fallback.Model(), scene, temp, system, user, out, usage, time.Since(start), err)
		if err == nil {
			return out, nil
		} else if lastErr == nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", ErrNoProvider
}

// record 异步落一条调用历史：写失败只告警，绝不拖慢/阻断 LLM 调用本身。
// 输入/输出内容截断落库（详情弹层展示用），字符数记截断前的全量。
func (m *Manager) record(ctx context.Context, provider, model, scene string, temp float64, system, user, out string, usage Usage, cost time.Duration, err error) {
	if m.calls == nil {
		return
	}
	meta := MetaFrom(ctx)
	fullPrompt := PromptFull(system, user)
	rec := CallRecord{
		Provider:    provider,
		Model:       model,
		Scene:       sceneOr(scene),
		Purpose:     meta.Purpose,
		TaskID:      meta.TaskID,
		AgentKey:    meta.AgentKey,
		Temperature: temp,
		DurationMS:  cost.Milliseconds(),
		OK:          err == nil,
		// 字符数按 rune 计（中文 1 字 = 1 字符），与详情弹层「原文 X 字符」口径一致
		PromptChars: utf8.RuneCountInString(fullPrompt),
		ReplyChars:  utf8.RuneCountInString(out),
		PromptText:  truncateText(fullPrompt, maxPromptText),
		ReplyText:   truncateText(out, maxReplyText),
		Usage:       usage,
	}
	if err != nil {
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		rec.ErrorMsg = msg
	}
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := m.calls.Insert(c, rec); e != nil {
			log.Warn("[llm] 调用历史写入失败", "err", e)
		}
	}()
}

func sceneOr(scene string) string {
	if strings.TrimSpace(scene) == "" {
		return SceneGeneral
	}
	return scene
}

// candidates 解析可用数据源顺序：场景命中 → 默认 → 优先级
func (m *Manager) candidates(scene string) []Provider {
	list := m.snapshot(context.Background())
	enabled := make([]Provider, 0, len(list))
	for _, p := range list {
		if p.Enabled && p.Model() != "" && strings.TrimSpace(p.BaseURL) != "" {
			enabled = append(enabled, p)
		}
	}
	sort.SliceStable(enabled, func(i, j int) bool {
		if enabled[i].IsDefault != enabled[j].IsDefault {
			return enabled[i].IsDefault
		}
		if enabled[i].Priority != enabled[j].Priority {
			return enabled[i].Priority > enabled[j].Priority
		}
		return enabled[i].Name < enabled[j].Name
	})
	if scene == "" {
		return enabled
	}
	out := make([]Provider, 0, len(enabled))
	for _, p := range enabled {
		if p.ServesScene(scene) {
			out = append(out, p)
		}
	}
	return append(out, filterScene(enabled, scene)...)
}

// filterScene 把不服务该场景的数据源也追加在后面，做最终兜底
func filterScene(list []Provider, scene string) []Provider {
	out := make([]Provider, 0, len(list))
	for _, p := range list {
		if !p.ServesScene(scene) {
			out = append(out, p)
		}
	}
	return out
}

func (m *Manager) clientFor(p Provider) *Client {
	timeout := time.Duration(p.TimeoutSec) * time.Second
	return NewTarget(Target{
		BaseURL:     p.BaseURL,
		APIKey:      p.APIKey,
		Model:       p.Model(),
		Temperature: p.Temperature,
		MaxTokens:   p.MaxTokens,
		Timeout:     timeout,
	})
}

// snapshot 带 TTL 的数据源快照；存储异常时返回空列表，由 fallback 兜底
func (m *Manager) snapshot(ctx context.Context) []Provider {
	m.mu.RLock()
	if m.loaded && time.Since(m.cachedAt) < m.ttl {
		out := m.cache
		m.mu.RUnlock()
		return out
	}
	m.mu.RUnlock()

	if m.repo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := m.repo.List(ctx)
	if err != nil {
		log.Warn("[llm] 读取数据源失败，沿用上次快照", "err", err)
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.cache
	}
	m.mu.Lock()
	m.cache, m.cachedAt, m.loaded = list, time.Now(), true
	m.mu.Unlock()
	return list
}

// Invalidate 丢弃快照（配置变更后立即生效）
func (m *Manager) Invalidate() {
	m.mu.Lock()
	m.loaded = false
	m.cache = nil
	m.mu.Unlock()
}

// —— 数据源管理 ——

// ListProviders 数据源列表（密钥脱敏）
func (m *Manager) ListProviders(ctx context.Context, includeDisabled bool) ([]Provider, error) {
	if m.repo == nil {
		return nil, nil
	}
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(list))
	for _, p := range list {
		if !includeDisabled && !p.Enabled {
			continue
		}
		out = append(out, p.Masked())
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// GetProvider 单个数据源（脱敏）
func (m *Manager) GetProvider(ctx context.Context, name string) (*Provider, error) {
	p, err := m.repo.Get(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	out := p.Masked()
	return &out, nil
}

// CreateProvider 新增数据源
func (m *Manager) CreateProvider(ctx context.Context, in CreateProviderInput) (*Provider, error) {
	p, err := buildProvider(in)
	if err != nil {
		return nil, err
	}
	if err := m.repo.Create(ctx, p); err != nil {
		return nil, err
	}
	if p.IsDefault {
		if err := m.repo.SetDefault(ctx, p.Name); err != nil {
			return nil, err
		}
	}
	m.Invalidate()
	out := p.Masked()
	return &out, nil
}

// UpdateProvider 修改数据源（api_key 传空表示保持不变）
func (m *Manager) UpdateProvider(ctx context.Context, name string, in UpdateProviderInput) (*Provider, error) {
	p, err := m.repo.Get(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	applyPatch(p, in)
	if err := validateProvider(p); err != nil {
		return nil, err
	}
	if err := m.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	if in.IsDefault != nil && *in.IsDefault {
		if err := m.repo.SetDefault(ctx, p.Name); err != nil {
			return nil, err
		}
		p.IsDefault = true
	}
	m.Invalidate()
	out := p.Masked()
	return &out, nil
}

// DeleteProvider 删除数据源；默认源删除后自动把优先级最高的启用源设为默认
func (m *Manager) DeleteProvider(ctx context.Context, name string) error {
	p, err := m.repo.Get(ctx, name)
	if err != nil {
		return err
	}
	list, err := m.repo.List(ctx)
	if err != nil {
		return err
	}
	enabled := 0
	for _, it := range list {
		if it.Enabled {
			enabled++
		}
	}
	if p.Enabled && enabled <= 1 {
		return ErrLastProvider
	}
	if err := m.repo.Delete(ctx, name); err != nil {
		return err
	}
	if p.IsDefault {
		for _, it := range list {
			if it.Name != p.Name && it.Enabled {
				if err := m.repo.SetDefault(ctx, it.Name); err != nil {
					log.Error("[llm] 迁移默认数据源失败", "err", err)
				}
				break
			}
		}
	}
	m.Invalidate()
	return nil
}

// SetDefault 切换默认数据源
func (m *Manager) SetDefault(ctx context.Context, name string) (*Provider, error) {
	if err := m.repo.SetDefault(ctx, strings.TrimSpace(name)); err != nil {
		return nil, err
	}
	m.Invalidate()
	return m.GetProvider(ctx, name)
}

// SetEnabled 启用/停用数据源
func (m *Manager) SetEnabled(ctx context.Context, name string, enabled bool) (*Provider, error) {
	p, err := m.repo.Get(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if !enabled && p.Enabled {
		list, lerr := m.repo.List(ctx)
		if lerr != nil {
			return nil, lerr
		}
		enabledCount := 0
		for _, it := range list {
			if it.Enabled {
				enabledCount++
			}
		}
		if enabledCount <= 1 {
			return nil, ErrLastProvider
		}
	}
	p.Enabled = enabled
	if err := m.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	m.Invalidate()
	out := p.Masked()
	return &out, nil
}

// TestProvider 连通性测试：发一句最小对话，返回耗时与回复
func (m *Manager) TestProvider(ctx context.Context, name string) (*TestResult, error) {
	p, err := m.repo.Get(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	start := time.Now()
	reply, usage, err := m.clientFor(*p).ChatTempDetailed(ctx, "你是连通性测试助手，只回复 OK。", "ping", 0)
	m.record(ctx, p.Name, p.Model(), SceneGeneral, 0, "连通性测试", "ping", reply, usage, time.Since(start), err)
	res := &TestResult{
		Provider:  p.Name,
		Model:     p.Model(),
		LatencyMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	res.OK = true
	res.Reply = strings.TrimSpace(reply)
	return res, nil
}

// RemoteModels 拉取数据源可用模型列表（不落库，供前端选择）
func (m *Manager) RemoteModels(ctx context.Context, name string) ([]string, error) {
	p, err := m.repo.Get(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	return m.clientFor(*p).ListModels(ctx)
}

// Chat 管理端调试对话：provider 为空走场景路由
func (m *Manager) ChatWith(ctx context.Context, in ChatInput) (string, error) {
	user := strings.TrimSpace(in.User)
	if user == "" {
		return "", fmt.Errorf("%w: user 不能为空", ErrInvalidProvider)
	}
	system := in.System
	if strings.TrimSpace(system) == "" {
		system = "你是一个乐于助人的助手。"
	}
	if strings.TrimSpace(in.Provider) != "" {
		p, err := m.repo.Get(ctx, strings.TrimSpace(in.Provider))
		if err != nil {
			return "", err
		}
		temp := p.Temperature
		if in.Temperature > 0 {
			temp = in.Temperature
		}
		start := time.Now()
		out, usage, err := m.clientFor(*p).ChatTempDetailed(ctx, system, user, temp)
		m.record(ctx, p.Name, p.Model(), in.Scene, temp, system, user, out, usage, time.Since(start), err)
		return out, err
	}
	if in.Temperature > 0 {
		return m.chat(ctx, in.Scene, system, user, in.Temperature)
	}
	return m.chat(ctx, in.Scene, system, user, -1)
}

// Stats 数据源规模
func (m *Manager) Stats(ctx context.Context) (Stats, error) {
	if m.repo == nil {
		return Stats{}, nil
	}
	return m.repo.Stats(ctx)
}

// —— 调用历史查询 ——

// ListCalls 调用历史（时间倒序 + 总数）
func (m *Manager) ListCalls(ctx context.Context, f CallFilter) ([]CallRecord, int64, error) {
	if m.calls == nil {
		return nil, 0, nil
	}
	return m.calls.List(ctx, f)
}

// CallStats 调用历史统计（含按模型聚合）
func (m *Manager) CallStats(ctx context.Context) (CallStats, error) {
	if m.calls == nil {
		return CallStats{}, nil
	}
	return m.calls.Stats(ctx)
}

// UsageBreakdown token 用量下钻（按 agent/scene/purpose/model 维度排行）
func (m *Manager) UsageBreakdown(ctx context.Context, dim string, days int) ([]UsageRow, error) {
	if m.calls == nil {
		return nil, nil
	}
	return m.calls.Breakdown(ctx, dim, days)
}

// —— 构造与校验 ——

func buildProvider(in CreateProviderInput) (*Provider, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	p := &Provider{
		Name:         strings.TrimSpace(in.Name),
		Kind:         normalizeKind(in.Kind),
		BaseURL:      strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"),
		APIKey:       strings.TrimSpace(in.APIKey),
		Models:       cleanStrings(in.Models),
		DefaultModel: strings.TrimSpace(in.DefaultModel),
		Temperature:  clampTemp(in.Temperature),
		MaxTokens:    in.MaxTokens,
		TimeoutSec:   in.TimeoutSec,
		Enabled:      enabled,
		IsDefault:    in.IsDefault,
		Priority:     in.Priority,
		Scenes:       cleanScenes(in.Scenes),
		Description:  strings.TrimSpace(in.Description),
	}
	if p.DefaultModel == "" && len(p.Models) > 0 {
		p.DefaultModel = p.Models[0]
	}
	if p.TimeoutSec <= 0 {
		p.TimeoutSec = 120
	}
	if p.Temperature == 0 {
		p.Temperature = 0.3
	}
	if err := validateProvider(p); err != nil {
		return nil, err
	}
	return p, nil
}

func applyPatch(p *Provider, in UpdateProviderInput) {
	if in.Kind != nil {
		p.Kind = normalizeKind(*in.Kind)
	}
	if in.BaseURL != nil {
		p.BaseURL = strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
	}
	if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
		p.APIKey = strings.TrimSpace(*in.APIKey)
	}
	if in.Models != nil {
		p.Models = cleanStrings(*in.Models)
	}
	if in.DefaultModel != nil {
		p.DefaultModel = strings.TrimSpace(*in.DefaultModel)
	}
	if in.Temperature != nil {
		p.Temperature = clampTemp(*in.Temperature)
	}
	if in.MaxTokens != nil {
		p.MaxTokens = *in.MaxTokens
	}
	if in.TimeoutSec != nil {
		p.TimeoutSec = *in.TimeoutSec
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.IsDefault != nil {
		p.IsDefault = *in.IsDefault
	}
	if in.Priority != nil {
		p.Priority = *in.Priority
	}
	if in.Scenes != nil {
		p.Scenes = cleanScenes(*in.Scenes)
	}
	if in.Description != nil {
		p.Description = strings.TrimSpace(*in.Description)
	}
	if p.DefaultModel == "" && len(p.Models) > 0 {
		p.DefaultModel = p.Models[0]
	}
	if p.TimeoutSec <= 0 {
		p.TimeoutSec = 120
	}
}

func validateProvider(p *Provider) error {
	if !validName(p.Name) {
		return fmt.Errorf("%w: 名称需以字母数字开头，可含 . _ -，最长 64 字符", ErrInvalidProvider)
	}
	if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
		return fmt.Errorf("%w: base_url 必须以 http:// 或 https:// 开头", ErrInvalidProvider)
	}
	if p.Model() == "" {
		return fmt.Errorf("%w: 需要配置 default_model 或 models", ErrInvalidProvider)
	}
	if p.TimeoutSec < 1 || p.TimeoutSec > 600 {
		return fmt.Errorf("%w: timeout_seconds 需在 1-600 之间", ErrInvalidProvider)
	}
	return nil
}

func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if i == 0 && !ok {
			return false
		}
		if !ok && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func normalizeKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	switch k {
	case "":
		return KindCustom
	case KindOpenAI, KindDeepSeek, KindOllama, KindQwen, KindSiliconFlow, KindMoonshot, KindCustom:
		return k
	default:
		return KindCustom
	}
}

func clampTemp(t float64) float64 {
	switch {
	case t < 0:
		return 0
	case t > 2:
		return 2
	default:
		return t
	}
}

func cleanStrings(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func cleanScenes(list []string) []string {
	out := cleanStrings(list)
	if len(out) == 0 {
		return []string{SceneGeneral}
	}
	return out
}

func nonEmpty(list []string) []string { return cleanStrings(list) }
