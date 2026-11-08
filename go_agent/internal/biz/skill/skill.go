package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go_agent/internal/biz/history"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/egress"

	"github.com/go-kratos/kratos/v3/log"
)

// defaultMaxFile 单文件内容上限（8MB）。
// 技能包常把运行时资源切成 1MB+ 的分片（如 project-runtime.partNN.txt），
// 1MB 的旧默认值会让这类正常技能直接装不上；上限受 skill_files.content
// 的 mediumtext（16MB）约束，8MB 留足余量。
const defaultMaxFile = 8 << 20

// manifestName 落盘时额外写一份元信息，便于运行时/排查
const manifestName = "manifest.json"

// UseCase 技能用例：安装、启停、卸载、市场搜索下载、落盘同步。
type UseCase struct {
	repo     Repo
	hub      HubClient
	syncDir  string
	maxFile  int64
	confHub  Hub
	hubLimit int
	// hist 统一操作历史（技能使用记录，可为 nil：不记录）
	hist history.Recorder
}

// New 创建技能用例；skill_hubs 为空时用 conf 播种默认市场。
func New(repo Repo, hub HubClient, hist history.Recorder, c *conf.Data) (*UseCase, error) {
	sc := c.GetSkill()
	maxFile := sc.GetMaxFileBytes()
	if maxFile <= 0 {
		maxFile = defaultMaxFile
	}
	u := &UseCase{
		repo:     repo,
		hub:      hub,
		syncDir:  strings.TrimSpace(sc.GetSyncDir()),
		maxFile:  maxFile,
		hubLimit: 100,
		hist:     hist,
		confHub: Hub{
			Name:      "default",
			BaseURL:   strings.TrimRight(strings.TrimSpace(sc.GetHubBaseURL()), "/"),
			Token:     strings.TrimSpace(sc.GetHubToken()),
			Enabled:   true,
			IsDefault: true,
		},
	}
	u.seedHub(context.Background())
	return u, nil
}

// seedHub 表为空且配置了默认地址时播种，避免升级后必须手工先建市场
func (u *UseCase) seedHub(ctx context.Context) {
	if u.repo == nil || u.confHub.BaseURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	hubs, err := u.repo.ListHubs(ctx)
	if err != nil {
		log.Error("[skill] 读取技能市场失败", "err", err)
		return
	}
	if len(hubs) > 0 {
		return
	}
	h := u.confHub
	h.Description = "由 config.yaml 的 data.skill 块自动播种"
	if err := u.repo.SaveHub(ctx, &h); err != nil {
		log.Error("[skill] 播种默认技能市场失败", "err", err)
		return
	}
	log.Info("[skill] 已播种默认技能市场", "name", h.Name, "base_url", h.BaseURL)
}

// —— 技能管理 ——

// List 技能列表（不含文件正文，按更新时间倒序）
func (u *UseCase) List(ctx context.Context, f Filter) ([]Skill, error) {
	if f.Limit <= 0 || f.Limit > u.hubLimit {
		f.Limit = u.hubLimit
	}
	return u.repo.List(ctx, f)
}

// Get 技能详情（含 SKILL.md 与文件列表）
func (u *UseCase) Get(ctx context.Context, name string) (*Skill, error) {
	return u.repo.Get(ctx, strings.TrimSpace(name))
}

// UseSkill 技能使用：返回技能内容（SKILL.md + 文件清单），同时落一条技能使用历史
// （op_histories）。任务内调用（WithTask）自动关联 task；用途 purpose 记入 ref 辅助列。
func (u *UseCase) UseSkill(ctx context.Context, name, purpose string) (*Skill, error) {
	start := time.Now()
	name = strings.TrimSpace(name)
	s, err := u.repo.Get(ctx, name)
	if err != nil {
		if u.hist != nil {
			u.hist.Record(ctx, &history.Record{
				Kind:      history.KindSkillUse,
				Ref:       name,
				Query:     purpose,
				Summary:   err.Error(),
				OK:        false,
				LatencyMS: time.Since(start).Milliseconds(),
			})
		}
		return nil, err
	}
	if !s.Enabled {
		err = fmt.Errorf("skill %s 已停用，启用后才能使用", name)
		if u.hist != nil {
			u.hist.Record(ctx, &history.Record{
				Kind: history.KindSkillUse, Ref: name, Query: purpose,
				Summary: err.Error(), OK: false, LatencyMS: time.Since(start).Milliseconds(),
			})
		}
		return nil, err
	}
	if u.hist != nil {
		summary := fmt.Sprintf("SKILL.md %d 字符，文件 %d 个", len([]rune(s.Content)), len(s.Files))
		u.hist.Record(ctx, &history.Record{
			Kind: history.KindSkillUse, Ref: name, Query: purpose,
			Summary: summary, OK: true, LatencyMS: time.Since(start).Milliseconds(),
		})
	}
	return s, nil
}

// Install 手动安装/上传技能；同名视为升级
func (u *UseCase) Install(ctx context.Context, in InstallInput) (*Skill, error) {
	name := strings.TrimSpace(in.Name)
	if !validName(name) {
		return nil, ErrInvalidName
	}
	s, err := u.build(ctx, name, in)
	if err != nil {
		return nil, err
	}
	s.Source = SourceUpload
	if err := u.repo.Save(ctx, s); err != nil {
		return nil, err
	}
	if s.Enabled {
		if err := u.materialize(s); err != nil {
			return nil, err
		}
	} else {
		u.removeDir(s.Name)
	}
	return u.repo.Get(ctx, name)
}

// build 归一化并校验技能内容
func (u *UseCase) build(ctx context.Context, name string, in InstallInput) (*Skill, error) {
	content := strings.TrimSpace(in.Content)
	files := make([]File, 0, len(in.Files))
	for _, f := range in.Files {
		p := strings.TrimSpace(f.Path)
		if !validFilePath(p) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidFile, p)
		}
		if int64(len(f.Content)) > u.maxFile {
			return nil, fmt.Errorf("%w: %s 超过 %d 字节（可调 data.skill.maxFileBytes）",
				ErrInvalidFile, p, u.maxFile)
		}
		files = append(files, File{Path: path.Clean(p), Content: f.Content, Size: len(f.Content)})
	}
	if content == "" {
		for _, f := range files {
			if strings.EqualFold(f.Path, "SKILL.md") {
				content = strings.TrimSpace(f.Content)
				break
			}
		}
	}
	if content == "" && len(files) == 0 {
		return nil, ErrEmptyContent
	}
	if int64(len(content)) > u.maxFile {
		return nil, fmt.Errorf("%w: SKILL.md 超过 %d 字节", ErrInvalidFile, u.maxFile)
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	} else if existing, err := u.repo.Get(ctx, name); err == nil {
		// 升级时未显式指定则沿用原状态
		enabled = existing.Enabled
	}
	size := len(content)
	for _, f := range files {
		size += f.Size
	}
	version := strings.TrimSpace(in.Version)
	if version == "" {
		version = "1.0.0"
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = name
	}
	return &Skill{
		Name:        name,
		Version:     version,
		Title:       title,
		Description: strings.TrimSpace(in.Description),
		Category:    strings.ToLower(strings.TrimSpace(in.Category)),
		Tags:        cleanTags(in.Tags),
		Enabled:     enabled,
		Checksum:    checksum(content, files),
		Content:     content,
		Files:       files,
		FileCount:   len(files),
		Size:        size,
	}, nil
}

// Enable 启用并落盘
func (u *UseCase) Enable(ctx context.Context, name string) (*Skill, error) {
	s, err := u.repo.Get(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if err := u.repo.SetEnabled(ctx, s.Name, true); err != nil {
		return nil, err
	}
	s.Enabled = true
	if err := u.materialize(s); err != nil {
		return nil, err
	}
	return u.repo.Get(ctx, s.Name)
}

// Disable 停用并从落盘目录移除
func (u *UseCase) Disable(ctx context.Context, name string) (*Skill, error) {
	s, err := u.repo.Get(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if err := u.repo.SetEnabled(ctx, s.Name, false); err != nil {
		return nil, err
	}
	u.removeDir(s.Name)
	s.Enabled = false
	return s, nil
}

// Uninstall 卸载技能：删记录 + 删落盘目录
func (u *UseCase) Uninstall(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if err := u.repo.Delete(ctx, name); err != nil {
		return err
	}
	u.removeDir(name)
	return nil
}

// Sync 把全部启用技能重新落盘，并清理不再启用的目录（幂等，可定时调用）
func (u *UseCase) Sync(ctx context.Context) (*SyncResult, error) {
	res := &SyncResult{Dir: u.syncDir, Synced: []string{}, Removed: []string{}}
	if u.syncDir == "" {
		return res, nil
	}
	enabled := true
	list, err := u.repo.List(ctx, Filter{Enabled: &enabled, Limit: 10000})
	if err != nil {
		return nil, err
	}
	keep := make(map[string]struct{}, len(list))
	for i := range list {
		full, err := u.repo.Get(ctx, list[i].Name)
		if err != nil {
			continue
		}
		if err := u.materialize(full); err != nil {
			return nil, err
		}
		keep[full.Name] = struct{}{}
		res.Synced = append(res.Synced, full.Name)
	}
	// 清理落盘目录里已不再启用的技能
	entries, err := os.ReadDir(u.syncDir)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := keep[e.Name()]; !ok {
			u.removeDir(e.Name())
			res.Removed = append(res.Removed, e.Name())
		}
	}
	return res, nil
}

// —— 技能市场 ——

// SearchHub 搜索技能市场；hubName 为空用默认市场
func (u *UseCase) SearchHub(ctx context.Context, keyword, hubName string, limit int) ([]HubSkill, error) {
	hub, err := u.resolveHub(ctx, hubName)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	list, err := u.hub.Search(ctx, *hub, strings.TrimSpace(keyword), limit)
	if err != nil {
		return nil, err
	}
	// 标记已安装，便于前端展示「已安装 / 可安装」
	installed := make(map[string]struct{})
	if all, lerr := u.repo.List(ctx, Filter{Limit: 10000}); lerr == nil {
		for _, s := range all {
			installed[s.Name] = struct{}{}
		}
	}
	for i := range list {
		if _, ok := installed[list[i].Name]; ok {
			list[i].Installed = true
		}
		list[i].Hub = hub.Name
	}
	// 保持市场返回的顺序（相关度/热度），不要按名字重排，否则搜索结果看起来是乱的
	return list, nil
}

// InstallFromHub 从技能市场下载并安装
func (u *UseCase) InstallFromHub(ctx context.Context, in InstallFromHubInput) (*Skill, error) {
	name := strings.TrimSpace(in.Name)
	if !validName(name) {
		return nil, ErrInvalidName
	}
	hub, err := u.resolveHub(ctx, in.Hub)
	if err != nil {
		return nil, err
	}
	pkg, err := u.hub.Fetch(ctx, *hub, name, strings.TrimSpace(in.Version))
	if err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, ErrHubResponse
	}
	pkg.Name = name
	if strings.TrimSpace(in.Version) != "" {
		pkg.Version = strings.TrimSpace(in.Version)
	}
	pkg.Source = SourceHub
	pkg.SourceURL = hub.BaseURL
	if in.Enable != nil {
		pkg.Enabled = *in.Enable
	} else {
		pkg.Enabled = true
	}
	normalized, err := u.build(ctx, name, InstallInput{
		Name:        name,
		Version:     pkg.Version,
		Title:       pkg.Title,
		Description: pkg.Description,
		Category:    pkg.Category,
		Tags:        pkg.Tags,
		Content:     pkg.Content,
		Files:       pkg.Files,
		Enabled:     &pkg.Enabled,
	})
	if err != nil {
		return nil, err
	}
	normalized.Source = SourceHub
	normalized.SourceURL = hub.BaseURL
	if err := u.repo.Save(ctx, normalized); err != nil {
		return nil, err
	}
	if normalized.Enabled {
		if err := u.materialize(normalized); err != nil {
			return nil, err
		}
	}
	return u.repo.Get(ctx, name)
}

// ImportFromURL 从技能包 URL 安装。
//
// 覆盖「没有市场 API、只给一个下载链接」的分发方式（如千问技能包、内部 GitLab 归档）：
// zip 包按包内目录结构入库；单文件按 SKILL.md 正文处理（此时必须传 name）；
// JSON 包按 {name,version,content,files} 解析。私有链接可传 token（Bearer）。
func (u *UseCase) ImportFromURL(ctx context.Context, in ImportFromURLInput) (*Skill, error) {
	raw := strings.TrimSpace(in.URL)
	// 出站闸门：技能包地址同样是「服务端替你去下载」，只校验前缀就能被当成
	// 内网探测器（http://169.254.169.254/… 会被当技能包拉回来）。
	if err := egress.ValidateURL(raw); err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidURL, raw)
	}
	pkg, err := u.hub.FetchURL(ctx, raw, strings.TrimSpace(in.Token))
	if err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, ErrHubResponse
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.TrimSpace(pkg.Name)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: 该链接里没有技能名，请显式指定 name", ErrInvalidName)
	}
	if !validName(name) {
		return nil, ErrInvalidName
	}
	if v := strings.TrimSpace(in.Version); v != "" {
		pkg.Version = v
	}
	enabled := true
	if in.Enable != nil {
		enabled = *in.Enable
	}
	normalized, err := u.build(ctx, name, InstallInput{
		Name:        name,
		Version:     pkg.Version,
		Title:       pkg.Title,
		Description: pkg.Description,
		Category:    pkg.Category,
		Tags:        pkg.Tags,
		Content:     pkg.Content,
		Files:       pkg.Files,
		Enabled:     &enabled,
	})
	if err != nil {
		return nil, err
	}
	normalized.Source = SourceURL
	normalized.SourceURL = raw
	if err := u.repo.Save(ctx, normalized); err != nil {
		return nil, err
	}
	if normalized.Enabled {
		if err := u.materialize(normalized); err != nil {
			return nil, err
		}
	}
	return u.repo.Get(ctx, name)
}

// ListHubs 技能市场列表（token 脱敏）
func (u *UseCase) ListHubs(ctx context.Context) ([]Hub, error) {
	list, err := u.repo.ListHubs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Hub, 0, len(list))
	for _, h := range list {
		out = append(out, h.Masked())
	}
	return out, nil
}

// CreateHub 新增技能市场
func (u *UseCase) CreateHub(ctx context.Context, in HubInput) (*Hub, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	h := &Hub{
		Name:        strings.TrimSpace(in.Name),
		BaseURL:     strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"),
		Token:       strings.TrimSpace(in.Token),
		Enabled:     enabled,
		IsDefault:   in.IsDefault,
		Description: strings.TrimSpace(in.Description),
	}
	if err := validateHub(h); err != nil {
		return nil, err
	}
	if err := u.repo.SaveHub(ctx, h); err != nil {
		return nil, err
	}
	if h.IsDefault {
		if err := u.repo.SetDefaultHub(ctx, h.Name); err != nil {
			return nil, err
		}
	}
	out := h.Masked()
	return &out, nil
}

// UpdateHub 修改技能市场（token 传空表示保持原值）
func (u *UseCase) UpdateHub(ctx context.Context, name string, in HubPatch) (*Hub, error) {
	h, err := u.repo.GetHub(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if in.BaseURL != nil {
		h.BaseURL = strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
	}
	if in.Token != nil && strings.TrimSpace(*in.Token) != "" {
		h.Token = strings.TrimSpace(*in.Token)
	}
	if in.Enabled != nil {
		h.Enabled = *in.Enabled
	}
	if in.IsDefault != nil {
		h.IsDefault = *in.IsDefault
	}
	if in.Description != nil {
		h.Description = strings.TrimSpace(*in.Description)
	}
	if err := validateHub(h); err != nil {
		return nil, err
	}
	if err := u.repo.SaveHub(ctx, h); err != nil {
		return nil, err
	}
	if h.IsDefault {
		if err := u.repo.SetDefaultHub(ctx, h.Name); err != nil {
			return nil, err
		}
	}
	out := h.Masked()
	return &out, nil
}

// DeleteHub 删除技能市场
func (u *UseCase) DeleteHub(ctx context.Context, name string) error {
	return u.repo.DeleteHub(ctx, strings.TrimSpace(name))
}

// SetDefaultHub 切换默认技能市场
func (u *UseCase) SetDefaultHub(ctx context.Context, name string) (*Hub, error) {
	if err := u.repo.SetDefaultHub(ctx, strings.TrimSpace(name)); err != nil {
		return nil, err
	}
	h, err := u.repo.GetHub(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	out := h.Masked()
	return &out, nil
}

// Stats 规模统计
func (u *UseCase) Stats(ctx context.Context) (Stats, error) {
	return u.repo.Stats(ctx)
}

// resolveHub 解析市场：指定名 → 默认 → conf 兜底
func (u *UseCase) resolveHub(ctx context.Context, name string) (*Hub, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		return u.repo.GetHub(ctx, name)
	}
	hubs, err := u.repo.ListHubs(ctx)
	if err == nil {
		for i := range hubs {
			if hubs[i].IsDefault && hubs[i].Enabled {
				return &hubs[i], nil
			}
		}
		for i := range hubs {
			if hubs[i].Enabled {
				return &hubs[i], nil
			}
		}
	}
	if u.confHub.BaseURL != "" {
		h := u.confHub
		return &h, nil
	}
	return nil, ErrNoHub
}

// —— 落盘 ——

// materialize 把技能写到 syncDir/<name>/（SKILL.md + 文件 + manifest.json）
func (u *UseCase) materialize(s *Skill) error {
	if u.syncDir == "" || s == nil || s.Name == "" {
		return nil
	}
	root, err := filepath.Abs(u.syncDir)
	if err != nil {
		return fmt.Errorf("解析技能目录: %w", err)
	}
	target := filepath.Join(root, s.Name)
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("清理技能目录 %s: %w", target, err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("创建技能目录 %s: %w", target, err)
	}
	if s.Content != "" {
		if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte(s.Content), 0o644); err != nil {
			return err
		}
	}
	for _, f := range s.Files {
		dest := filepath.Join(target, filepath.FromSlash(path.Clean(f.Path)))
		if !strings.HasPrefix(dest, target+string(os.PathSeparator)) {
			return fmt.Errorf("%w: %s", ErrInvalidFile, f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	meta := map[string]any{
		"name":         s.Name,
		"version":      s.Version,
		"title":        s.Title,
		"description":  s.Description,
		"category":     s.Category,
		"tags":         s.Tags,
		"source":       s.Source,
		"source_url":   s.SourceURL,
		"checksum":     s.Checksum,
		"enabled":      s.Enabled,
		"installed_at": s.InstalledAt,
	}
	raw, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(filepath.Join(target, manifestName), raw, 0o644)
}

// removeDir 删除技能落盘目录（不存在视为成功）
func (u *UseCase) removeDir(name string) {
	if u.syncDir == "" || name == "" || !validName(name) {
		return
	}
	root, err := filepath.Abs(u.syncDir)
	if err != nil {
		return
	}
	if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
		log.Warn("[skill] 删除技能目录失败", "name", name, "err", err)
	}
}

// —— 校验工具 ——

func validateHub(h *Hub) error {
	if !validName(h.Name) {
		return fmt.Errorf("%w: 市场名称需为小写字母数字开头，可含 . _ -", ErrInvalidName)
	}
	if err := egress.ValidateURL(h.BaseURL); err != nil {
		return fmt.Errorf("%w: base_url 不合法（%v）", ErrHubResponse, err)
	}
	return nil
}

func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 && !ok {
			return false
		}
		if !ok && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// validFilePath 只允许技能目录内的相对路径
func validFilePath(p string) bool {
	if p == "" || len(p) > 512 || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return false
	}
	if strings.Contains(p, "..") || strings.Contains(p, `\`) {
		return false
	}
	clean := path.Clean(p)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func checksum(content string, files []File) string {
	h := sha256.New()
	h.Write([]byte(content))
	cp := make([]File, len(files))
	copy(cp, files)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Path < cp[j].Path })
	for _, f := range cp {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(f.Content))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cleanTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
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
