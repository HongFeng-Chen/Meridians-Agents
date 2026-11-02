package prompt

import (
	"context"
	"errors"
	"strings"
	"time"

	"go_agent/internal/conf"

	"github.com/go-kratos/kratos/v3/log"
)

// UseCase 提示词工程用例：模板管理 + 版本发布/回滚 + 渲染。
type UseCase struct {
	repo  Repo
	cache Cache
	ttl   time.Duration
	limit int
}

// New 构建用例（wire provider）。
//
// 启动时补齐内置模板（已存在则跳过），保证全新环境开箱即用；
// 补齐失败只告警不阻断启动——渲染侧还有内置回落。
func New(repo Repo, cache Cache, c *conf.Data) (*UseCase, error) {
	pc := c.GetPrompt()
	ttl := time.Duration(0)
	limit := 200
	seed := true
	if pc != nil {
		if pc.GetCacheTTL() != nil {
			ttl = pc.GetCacheTTL().AsDuration()
		}
		if pc.GetListLimit() > 0 {
			limit = int(pc.GetListLimit())
		}
		seed = pc.GetSeedBuiltin()
	}

	u := &UseCase{repo: repo, cache: cache, ttl: ttl, limit: limit}
	if seed {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := u.seed(ctx); err != nil {
			log.Error("[prompt] 初始化内置模板失败", "err", err)
		}
	}
	return u, nil
}

// seed 写入缺失的内置模板，已有（哪怕被改过）不覆盖
func (u *UseCase) seed(ctx context.Context) error {
	for _, b := range Builtins {
		if _, err := u.repo.Get(ctx, b.Key); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		p := &Prompt{
			Key:           b.Key,
			Name:          b.Name,
			Scene:         b.Scene,
			Description:   b.Description,
			Tags:          b.Tags,
			Builtin:       true,
			ActiveVersion: 1,
		}
		v := &Version{
			PromptKey:   b.Key,
			Version:     1,
			Content:     b.Content,
			Variables:   Placeholders(b.Content),
			Temperature: b.Temperature,
			MaxTokens:   b.MaxTokens,
			Note:        "内置初始版本",
			Author:      "system",
		}
		if err := u.repo.Create(ctx, p, v); err != nil {
			return err
		}
		log.Info("[prompt] 已写入内置模板", "key", b.Key, "version", 1)
	}
	return nil
}

// Create 新建模板（v1 直接生效）
func (u *UseCase) Create(ctx context.Context, in CreateInput) (*Detail, error) {
	key := strings.TrimSpace(in.Key)
	if !ValidKey(key) {
		return nil, ErrInvalidKey
	}
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return nil, ErrEmptyContent
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = key
	}
	p := &Prompt{
		Key:           key,
		Name:          name,
		Scene:         normalizeScene(in.Scene),
		Description:   strings.TrimSpace(in.Description),
		Tags:          cleanTags(in.Tags),
		ActiveVersion: 1,
	}
	v := &Version{
		PromptKey:   key,
		Version:     1,
		Content:     content,
		Variables:   Placeholders(content),
		Temperature: clampTemp(in.Temperature),
		MaxTokens:   in.MaxTokens,
		Note:        noteOr(in.Note, "初始版本"),
		Author:      strings.TrimSpace(in.Author),
	}
	if err := u.repo.Create(ctx, p, v); err != nil {
		return nil, err
	}
	_ = u.invalidate(ctx, key)
	// 回读一次，把 created_at / updated_at 等由数据库生成的字段补齐
	// （否则接口返回 0001-01-01T00:00:00Z，前端时间列会显示异常）
	if d, err := u.Get(ctx, key); err == nil {
		return d, nil
	}
	return &Detail{Prompt: *p, Active: v}, nil
}

// Get 模板详情（含生效版本）
func (u *UseCase) Get(ctx context.Context, key string) (*Detail, error) {
	p, err := u.repo.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	d := &Detail{Prompt: *p}
	if p.ActiveVersion > 0 {
		if v, err := u.repo.GetVersion(ctx, key, p.ActiveVersion); err == nil {
			v.Active = true
			d.Active = v
		}
	}
	return d, nil
}

// List 模板列表
func (u *UseCase) List(ctx context.Context, f ListFilter) ([]Prompt, error) {
	if f.Limit <= 0 || f.Limit > u.limit {
		f.Limit = u.limit
	}
	f.Scene = strings.ToLower(strings.TrimSpace(f.Scene))
	f.Keyword = strings.TrimSpace(f.Keyword)
	return u.repo.List(ctx, f)
}

// Update 修改元信息（名称/场景/描述/标签），不影响已发布版本
func (u *UseCase) Update(ctx context.Context, key string, in UpdateInput) (*Detail, error) {
	p, err := u.repo.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.Scene != nil {
		p.Scene = normalizeScene(*in.Scene)
	}
	if in.Description != nil {
		p.Description = strings.TrimSpace(*in.Description)
	}
	if in.Tags != nil {
		p.Tags = cleanTags(*in.Tags)
	}
	if err := u.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return u.Get(ctx, key)
}

// Delete 删除模板（内置模板受保护）
func (u *UseCase) Delete(ctx context.Context, key string) error {
	p, err := u.repo.Get(ctx, key)
	if err != nil {
		return err
	}
	if p.Builtin {
		return ErrBuiltinProtected
	}
	if err := u.repo.Delete(ctx, key); err != nil {
		return err
	}
	_ = u.invalidate(ctx, key)
	return nil
}

// Publish 发布新版本；Activate 缺省为 true（发布即生效）
func (u *UseCase) Publish(ctx context.Context, key string, in PublishInput) (*Version, error) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return nil, ErrEmptyContent
	}
	if _, err := u.repo.Get(ctx, key); err != nil {
		return nil, err
	}
	v := &Version{
		PromptKey:   key,
		Content:     content,
		Variables:   Placeholders(content),
		Temperature: clampTemp(in.Temperature),
		MaxTokens:   in.MaxTokens,
		Note:        strings.TrimSpace(in.Note),
		Author:      strings.TrimSpace(in.Author),
	}
	n, err := u.repo.AddVersion(ctx, v)
	if err != nil {
		return nil, err
	}
	v.Version = n

	activate := true
	if in.Activate != nil {
		activate = *in.Activate
	}
	if activate {
		if err := u.repo.Activate(ctx, key, n); err != nil {
			return nil, err
		}
		v.Active = true
	}
	// 发布/激活必须立刻让缓存失效，否则线上继续用旧模板
	_ = u.invalidate(ctx, key)
	// 回读一次拿到 created_at（数据库写入时生成）
	if got, err := u.Version(ctx, key, n); err == nil {
		return got, nil
	}
	return v, nil
}

// Activate 切换生效版本（回滚也走这里）
func (u *UseCase) Activate(ctx context.Context, key string, version int) (*Detail, error) {
	if version <= 0 {
		return nil, ErrVersionNotFound
	}
	if _, err := u.repo.GetVersion(ctx, key, version); err != nil {
		return nil, err
	}
	if err := u.repo.Activate(ctx, key, version); err != nil {
		return nil, err
	}
	_ = u.invalidate(ctx, key)
	return u.Get(ctx, key)
}

// Rollback 回滚到上一个版本
func (u *UseCase) Rollback(ctx context.Context, key string) (*Detail, error) {
	p, err := u.repo.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	versions, err := u.repo.ListVersions(ctx, key, 200)
	if err != nil {
		return nil, err
	}
	target := 0
	for _, v := range versions { // 倒序：取第一个比当前生效版本小的
		if v.Version < p.ActiveVersion {
			target = v.Version
			break
		}
	}
	if target == 0 {
		return nil, ErrNoPrevious
	}
	return u.Activate(ctx, key, target)
}

// Reset 用内置原文发布一个新版本并生效（内置模板被改坏时的兜底）
func (u *UseCase) Reset(ctx context.Context, key, author string) (*Version, error) {
	b, ok := BuiltinOf(key)
	if !ok {
		return nil, ErrNotFound
	}
	if _, err := u.repo.Get(ctx, key); err != nil {
		return nil, err
	}
	v := &Version{
		PromptKey:   key,
		Content:     b.Content,
		Variables:   Placeholders(b.Content),
		Temperature: b.Temperature,
		MaxTokens:   b.MaxTokens,
		Note:        "恢复内置默认内容",
		Author:      strings.TrimSpace(author),
	}
	n, err := u.repo.AddVersion(ctx, v)
	if err != nil {
		return nil, err
	}
	v.Version = n
	if err := u.repo.Activate(ctx, key, n); err != nil {
		return nil, err
	}
	v.Active = true
	_ = u.invalidate(ctx, key)
	return v, nil
}

// Versions 版本列表（倒序）
func (u *UseCase) Versions(ctx context.Context, key string, limit int) ([]Version, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	return u.repo.ListVersions(ctx, key, limit)
}

// Version 单个版本详情
func (u *UseCase) Version(ctx context.Context, key string, version int) (*Version, error) {
	p, err := u.repo.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	v, err := u.repo.GetVersion(ctx, key, version)
	if err != nil {
		return nil, err
	}
	v.Active = p.ActiveVersion == version
	return v, nil
}

// Stats 规模统计
func (u *UseCase) Stats(ctx context.Context) (Stats, error) {
	return u.repo.Stats(ctx)
}

// Render 渲染生效版本，缺变量直接报错（供 RAG/GraphRAG 调用）。
// 实现 rag.Prompter / graphrag.Prompter。
func (u *UseCase) Render(ctx context.Context, key string, vars map[string]string) (string, error) {
	tpl, _, err := u.template(ctx, key)
	if err != nil {
		return "", err
	}
	out, missing := Render(tpl, vars)
	if len(missing) > 0 {
		return "", &MissingVarsError{Key: key, Names: missing}
	}
	return out, nil
}

// Preview 预览渲染：缺变量只列出不报错，方便前端实时调试
func (u *UseCase) Preview(ctx context.Context, key string, in RenderInput) (*RenderResult, error) {
	v, err := u.versionForPreview(ctx, key, in.Version)
	if err != nil {
		return nil, err
	}
	out, missing := Render(v.Content, in.Vars)
	vars := v.Variables
	if len(vars) == 0 {
		vars = Placeholders(v.Content)
	}
	return &RenderResult{
		Key:       key,
		Version:   v.Version,
		Content:   v.Content,
		Rendered:  out,
		Variables: vars,
		Missing:   missing,
	}, nil
}

func (u *UseCase) versionForPreview(ctx context.Context, key string, version int) (*Version, error) {
	if version > 0 {
		return u.repo.GetVersion(ctx, key, version)
	}
	v, err := u.repo.ActiveVersion(ctx, key)
	if err == nil {
		return v, nil
	}
	// 还没入库的内置模板允许直接预览默认内容
	if b, ok := BuiltinOf(key); ok && (errors.Is(err, ErrNotFound) || errors.Is(err, ErrVersionNotFound)) {
		return &Version{
			PromptKey:   key,
			Content:     b.Content,
			Variables:   Placeholders(b.Content),
			Temperature: b.Temperature,
		}, nil
	}
	return nil, err
}

// template 取生效模板内容：缓存 → 仓库 → 内置
func (u *UseCase) template(ctx context.Context, key string) (string, int, error) {
	if u.cache != nil {
		if v, ok := u.cache.GetVersion(ctx, key); ok && strings.TrimSpace(v.Content) != "" {
			return v.Content, v.Version, nil
		}
	}
	v, err := u.repo.ActiveVersion(ctx, key)
	if err != nil {
		// 存储不可用或模板缺失时回落到内置，保证问答不中断
		if b, ok := BuiltinOf(key); ok {
			return b.Content, 0, nil
		}
		return "", 0, err
	}
	if u.cache != nil && u.ttl > 0 {
		u.cache.PutVersion(ctx, key, v, u.ttl)
	}
	return v.Content, v.Version, nil
}

func (u *UseCase) invalidate(ctx context.Context, key string) error {
	if u.cache == nil {
		return nil
	}
	return u.cache.Invalidate(ctx, key)
}

func normalizeScene(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return SceneGeneral
	}
	return s
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

func cleanTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func noteOr(note, def string) string {
	if s := strings.TrimSpace(note); s != "" {
		return s
	}
	return def
}

// SaveAgentPrompt 把 Agent 的角色提示词沉淀到提示词工程。
//
// 模板 key 固定为 agent.<agent_key>.system：已存在则发布新版本（立即生效），
// 不存在则创建（scene=agent，带 agent 标签，便于在提示词工程页按 Agent 过滤）。
// LLM 元编程创建 Agent（create-by-llm / create_agent）成功后自动调用；
// 同步失败只影响沉淀，不影响 Agent 本身（调用方记 Warning 即可）。
func (u *UseCase) SaveAgentPrompt(ctx context.Context, agentKey, agentName, content string) error {
	agentKey = strings.TrimSpace(agentKey)
	content = strings.TrimSpace(content)
	if agentKey == "" || content == "" {
		return ErrEmptyContent
	}
	key := "agent." + agentKey + ".system"
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = agentKey
	}
	name = "Agent·" + name + "·角色提示词"

	_, err := u.Get(ctx, key)
	switch {
	case err == nil:
		// 已有沉淀：Agent 定义更新 → 发布新版本
		_, err = u.Publish(ctx, key, PublishInput{
			Content: content,
			Note:    "Agent 定义更新，自动同步",
			Author:  "agentfactory",
		})
		return err
	case errors.Is(err, ErrNotFound):
		_, err = u.Create(ctx, CreateInput{
			Key:         key,
			Name:        name,
			Scene:       "agent",
			Description: "LLM 创建 Agent 时自动沉淀的角色提示词（可在提示词工程内打分 / 优化）",
			Tags:        []string{"agent", agentKey},
			Content:     content,
			Note:        "LLM 创建 Agent 时自动沉淀",
			Author:      "agentfactory",
		})
		return err
	default:
		return err
	}
}

// RecordPromptScore 记录一次提示词版本评测得分（实现 harness.ScoreRecorder，
// harness 运行完成后回写）。key 为空或 version 非法时静默忽略。
func (u *UseCase) RecordPromptScore(ctx context.Context, key string, version int, runID uint64, score float64, cases int) error {
	key = strings.TrimSpace(key)
	if key == "" || version <= 0 {
		return nil
	}
	return u.repo.InsertVersionScore(ctx, VersionScore{
		PromptKey: key,
		Version:   version,
		RunID:     runID,
		Score:     score,
		Cases:     cases,
	})
}

// ListVersionScores 模板的版本得分列表（提示词工程页版本历史展示用）
func (u *UseCase) ListVersionScores(ctx context.Context, key string, limit int) ([]VersionScore, error) {
	return u.repo.ListVersionScores(ctx, key, limit)
}
