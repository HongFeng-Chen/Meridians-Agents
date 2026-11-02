// Package skillhub 技能市场（skillhub）HTTP 客户端。
//
// 支持两类市场，按 base_url 自动识别：
//
// 1) SkillHub（skillhub.cn）——国内技能市场，站点是 SPA，真正的接口在 api.skillhub.cn：
//
//	GET {api}/api/skills?page=1&pageSize=<n>&keyword=<kw>
//	     → {"code":0,"data":{"skills":[{slug,name,version,description,description_zh,
//	        category,downloads,installs,subCategories,namespace}],"total":N}}
//	GET {api}/api/v1/skills/{slug}
//	     → {"skill":{displayName,summary,summary_zh,category,subCategories},
//	        "latestVersion":{version}}
//	GET {api}/api/v1/download?slug={slug}[&version=<v>]
//	     → 302 → application/zip 技能包
//
//	把 https://www.skillhub.cn 这类页面地址填进 skill_hubs 也能用：会自动换成 api 主机。
//
// 2) 通用 registry（OpenAI 风格，base_url 由 skill_hubs 表决定）：
//
//	GET  {base}/api/v1/skills?q=<keyword>&limit=<n>
//	     → {"skills":[{name,version,title,description,category,tags,downloads}]}
//	       或直接返回数组、或 {"data":{"skills":[…]}}
//	GET  {base}/api/v1/skills/{name}                                （最新版）
//	GET  {base}/api/v1/skills/{name}/versions/{version}/download     （指定版本）
//	     → application/zip 技能包，或 JSON：
//	       {name,version,title,description,category,tags,content,
//	        files:[{path,content}]}
//
// 鉴权：hub 配置了 token 时带 Authorization: Bearer <token>。
package skillhub

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"go_agent/internal/biz/skill"
	"go_agent/internal/conf"
)

const (
	maxPackageSize = 20 << 20 // 技能包上限 20MB
	maxSnippetLen  = 200      // 报错时附带的响应片段长度
)

// archiveEntry zip 内的文本条目
type archiveEntry struct {
	name    string
	content string
}

// Client 实现 skill.HubClient
type Client struct {
	http *http.Client
}

var _ skill.HubClient = (*Client)(nil)

// New 创建技能市场客户端；超时取 data.skill.httpTimeout
func New(c *conf.Data) *Client {
	timeout := 30 * time.Second
	if t := c.GetSkill().GetHttpTimeout().AsDuration(); t > 0 {
		timeout = t
	}
	return &Client{http: &http.Client{Timeout: timeout}}
}

// —— 地址归一化 ——

// normalizeBase 去掉末尾 / 与 /api、/api/v1 后缀；SkillHub 的页面主机会替换成 api 主机。
// 返回的 bool 表示是否识别为 SkillHub 市场。
func normalizeBase(baseURL string) (string, bool) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "", false
	}
	for _, suffix := range []string{"/api/v1", "/api"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base, false
	}
	host := strings.ToLower(u.Host)
	if host == "skillhub.cn" || host == "www.skillhub.cn" || host == "api.skillhub.cn" {
		return "https://api.skillhub.cn", true
	}
	// 自建/镜像市场沿用原地址
	return base, false
}

// —— 搜索 ——

// Search 搜索技能市场
func (c *Client) Search(ctx context.Context, hub skill.Hub, keyword string, limit int) ([]skill.HubSkill, error) {
	if limit <= 0 {
		limit = 20
	}
	base, isHub := normalizeBase(hub.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("%w: 市场地址为空", skill.ErrHubResponse)
	}
	if isHub {
		return c.skillHubSearch(ctx, hub, base, keyword, limit)
	}
	return c.genericSearch(ctx, hub, base, keyword, limit)
}

// skillHubSearch 走 SkillHub 的 v0 列表接口（支持 keyword 过滤）
func (c *Client) skillHubSearch(ctx context.Context, hub skill.Hub, base, keyword string, limit int) ([]skill.HubSkill, error) {
	if limit > 100 {
		limit = 100 // 接口单页上限
	}
	q := url.Values{}
	q.Set("page", "1")
	q.Set("pageSize", fmt.Sprint(limit))
	if kw := strings.TrimSpace(keyword); kw != "" {
		q.Set("keyword", kw)
	}
	endpoint := base + "/api/skills?" + q.Encode()

	raw, contentType, err := c.getWithType(ctx, hub, endpoint)
	if err != nil {
		return nil, err
	}
	out, err := parseSkillHubList(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: 搜索结果无法解析（%s，%v）", skill.ErrHubResponse, describeBody(contentType, raw), err)
	}
	return out, nil
}

// parseSkillHubList 解析 {code,data:{skills,total}} 形态的列表
func parseSkillHubList(raw []byte) ([]skill.HubSkill, error) {
	var resp struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Total  int          `json:"total"`
			Skills []shSkillDTO `json:"skills"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Code != nil && *resp.Code != 0 {
		return nil, fmt.Errorf("市场返回错误 code=%d %s", *resp.Code, strings.TrimSpace(resp.Message))
	}
	// 无结果时 skills 为 []；连 data 字段都没有说明接口形态不对
	if resp.Data.Skills == nil && resp.Data.Total == 0 {
		var probe map[string]any
		if json.Unmarshal(raw, &probe) == nil {
			if _, ok := probe["data"]; !ok {
				return nil, fmt.Errorf("响应缺少 data.skills 字段")
			}
		}
	}
	out := make([]skill.HubSkill, 0, len(resp.Data.Skills))
	seen := make(map[string]struct{}, len(resp.Data.Skills))
	for _, d := range resp.Data.Skills {
		name := strings.TrimSpace(d.Slug)
		if name == "" {
			name = strings.TrimSpace(d.Namespace.PublicSlug)
		}
		if name == "" {
			continue // 没有可安装标识的条目直接跳过
		}
		// 不同命名空间可能有同名 slug，而安装只按 slug 定位，保留首个避免重复展示
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, skill.HubSkill{
			Name:        name,
			Version:     strings.TrimSpace(d.Version),
			Title:       firstNonEmpty(d.Name, name),
			Description: firstNonEmpty(d.DescriptionZh, d.Description),
			Category:    strings.ToLower(strings.TrimSpace(d.Category)),
			Tags:        d.subCategoryNames(),
			Downloads:   d.Downloads,
		})
	}
	return out, nil
}

// genericSearch 兼容多种 registry 返回形态
func (c *Client) genericSearch(ctx context.Context, hub skill.Hub, base, keyword string, limit int) ([]skill.HubSkill, error) {
	endpoint := fmt.Sprintf("%s/api/v1/skills?q=%s&limit=%d", base,
		url.QueryEscape(strings.TrimSpace(keyword)), limit)
	raw, contentType, err := c.getWithType(ctx, hub, endpoint)
	if err != nil {
		return nil, err
	}
	list, ok := decodeSkillList(raw)
	if !ok {
		return nil, fmt.Errorf("%w: 搜索结果无法解析（%s）；请确认市场地址指向 API 而不是网页",
			skill.ErrHubResponse, describeBody(contentType, raw))
	}
	out := make([]skill.HubSkill, 0, len(list))
	for _, d := range list {
		name := firstNonEmpty(d.Name, d.SkillName, d.Slug)
		if strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, skill.HubSkill{
			Name:        strings.TrimSpace(name),
			Version:     strings.TrimSpace(d.Version),
			Title:       firstNonEmpty(d.Title, d.DisplayName, d.Name, d.Slug),
			Description: strings.TrimSpace(d.Description),
			Category:    strings.ToLower(strings.TrimSpace(d.Category)),
			Tags:        d.Tags,
			Downloads:   d.Downloads,
		})
	}
	return out, nil
}

// decodeSkillList 依次尝试 {"skills":[…]}、{"data":{"skills":[…]}}、
// {"data":[…]}、{"items":[…]}、[…]，任一命中即返回。
func decodeSkillList(raw []byte) ([]hubSkillDTO, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	if trimmed[0] == '[' {
		var arr []hubSkillDTO
		if json.Unmarshal(trimmed, &arr) == nil {
			return arr, true
		}
		return nil, false
	}
	var obj struct {
		Skills []hubSkillDTO   `json:"skills"`
		Items  []hubSkillDTO   `json:"items"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(trimmed, &obj) != nil {
		return nil, false
	}
	if obj.Skills != nil {
		return obj.Skills, true
	}
	if obj.Items != nil {
		return obj.Items, true
	}
	if len(obj.Data) > 0 {
		var inner struct {
			Skills []hubSkillDTO `json:"skills"`
			Items  []hubSkillDTO `json:"items"`
		}
		if json.Unmarshal(obj.Data, &inner) == nil {
			if inner.Skills != nil {
				return inner.Skills, true
			}
			if inner.Items != nil {
				return inner.Items, true
			}
		}
		var arr []hubSkillDTO
		if json.Unmarshal(obj.Data, &arr) == nil && arr != nil {
			return arr, true
		}
	}
	return nil, false
}

// FetchURL 直接从 URL 拉技能包。
//
// 支持的形态（按内容自动识别，不看后缀）：
//   - zip：包内 SKILL.md + 其它文本文件，顶层目录会自动去掉；
//   - JSON：{name,version,title,description,content,files:[{path,content}]}（可包一层 skill）；
//   - 纯文本 / Markdown：当作 SKILL.md 正文，名称由调用方指定。
//
// 私有链接可传 token（Authorization: Bearer）；返回 HTML 页面时直接报错，
// 避免把控制台页面当成技能装进去。
func (c *Client) FetchURL(ctx context.Context, rawURL, token string) (*skill.Skill, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", skill.ErrInvalidURL, err)
	}
	req.Header.Set("Accept", "application/json, application/zip, text/markdown, text/plain")
	req.Header.Set("User-Agent", "go-agent-skillhub/1.0")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载技能包失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxPackageSize))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载技能包返回 %d（%s）: %s",
			resp.StatusCode, truncateStr(rawURL, 120), truncate(raw, 300))
	}
	if isZip(raw) {
		return parseZip(raw)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && (trimmed[0] == '{') {
		if s, jerr := parseJSON(raw); jerr == nil {
			return s, nil
		}
		// JSON 解析失败时继续按文本处理：有些 SKILL.md 本身以 { 开头（例如含 JSON 示例）
	}
	if !utf8.Valid(trimmed) || bytes.IndexByte(trimmed, 0) >= 0 {
		return nil, fmt.Errorf("%w: 下载内容不是文本（可能是不支持的二进制包）", skill.ErrHubResponse)
	}
	if looksHTMLBody(trimmed) {
		return nil, fmt.Errorf("%w: 该地址返回的是 HTML 页面，不是技能包（检查链接是否指向下载地址）", skill.ErrHubResponse)
	}
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: 下载内容为空", skill.ErrHubResponse)
	}
	return &skill.Skill{Content: string(trimmed), Files: nil}, nil
}

// looksHTMLBody 判断响应体是否是一个网页（而非技能包）
func looksHTMLBody(raw []byte) bool {
	lower := bytes.ToLower(raw)
	return bytes.HasPrefix(lower, []byte("<!doctype")) || bytes.HasPrefix(lower, []byte("<html"))
}

// —— 下载 ——

// Fetch 拉取技能包
func (c *Client) Fetch(ctx context.Context, hub skill.Hub, name, version string) (*skill.Skill, error) {
	base, isHub := normalizeBase(hub.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("%w: 市场地址为空", skill.ErrHubResponse)
	}
	if isHub {
		return c.skillHubFetch(ctx, hub, base, name, version)
	}
	return c.genericFetch(ctx, hub, base, name, version)
}

// skillHubFetch 下载 zip 包，并用详情接口补齐标题/描述/分类
func (c *Client) skillHubFetch(ctx context.Context, hub skill.Hub, base, name, version string) (*skill.Skill, error) {
	q := url.Values{}
	q.Set("slug", name)
	if v := strings.TrimSpace(version); v != "" && v != "latest" {
		q.Set("version", v)
	}
	raw, contentType, err := c.getWithType(ctx, hub, base+"/api/v1/download?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var s *skill.Skill
	if isZip(raw) {
		s, err = parseZip(raw)
		if err != nil {
			return nil, err
		}
	} else if js, jerr := parseJSON(raw); jerr == nil {
		// 少数镜像直接返回 JSON 包
		s = js
	} else {
		return nil, fmt.Errorf("%w: 下载技能包失败（%s）", skill.ErrHubResponse, describeBody(contentType, raw))
	}

	// 详情接口补元数据：列表接口没有 summary，zip 里也不一定有 manifest
	if detail, derr := c.skillHubDetail(ctx, hub, base, name); derr == nil && detail != nil {
		if s.Title == "" {
			s.Title = detail.Title
		}
		if s.Description == "" {
			s.Description = detail.Description
		}
		if s.Category == "" {
			s.Category = detail.Category
		}
		if len(s.Tags) == 0 {
			s.Tags = detail.Tags
		}
		if s.Version == "" {
			s.Version = detail.Version
		}
	}
	if s.Name == "" {
		s.Name = name
	}
	if s.Version == "" {
		s.Version = strings.TrimSpace(version)
	}
	return s, nil
}

// skillHubDetail 取技能详情（失败不影响安装）
func (c *Client) skillHubDetail(ctx context.Context, hub skill.Hub, base, name string) (*skill.Skill, error) {
	raw, _, err := c.getWithType(ctx, hub, base+"/api/v1/skills/"+url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	var dto struct {
		LatestVersion struct {
			Version string `json:"version"`
		} `json:"latestVersion"`
		Skill struct {
			DisplayName   string  `json:"displayName"`
			Summary       string  `json:"summary"`
			SummaryZh     string  `json:"summary_zh"`
			Category      string  `json:"category"`
			SubCategories []shCat `json:"subCategories"`
		} `json:"skill"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		return nil, err
	}
	out := &skill.Skill{
		Title:       strings.TrimSpace(dto.Skill.DisplayName),
		Description: firstNonEmpty(dto.Skill.SummaryZh, dto.Skill.Summary),
		Category:    strings.ToLower(strings.TrimSpace(dto.Skill.Category)),
		Version:     strings.TrimSpace(dto.LatestVersion.Version),
	}
	for _, c := range dto.Skill.SubCategories {
		if n := strings.TrimSpace(c.Name); n != "" {
			out.Tags = append(out.Tags, n)
		}
	}
	return out, nil
}

// genericFetch 按通用 registry 约定拉包：优先指定版本的 download 端点，其次 latest 端点。
func (c *Client) genericFetch(ctx context.Context, hub skill.Hub, base, name, version string) (*skill.Skill, error) {
	escaped := url.PathEscape(name)
	var endpoint string
	if v := strings.TrimSpace(version); v != "" && v != "latest" {
		endpoint = fmt.Sprintf("%s/api/v1/skills/%s/versions/%s/download", base, escaped, url.PathEscape(v))
	} else {
		endpoint = fmt.Sprintf("%s/api/v1/skills/%s", base, escaped)
	}
	raw, contentType, err := c.getWithType(ctx, hub, endpoint)
	if err != nil {
		return nil, err
	}
	var s *skill.Skill
	if strings.Contains(contentType, "zip") || isZip(raw) {
		s, err = parseZip(raw)
	} else {
		s, err = parseJSON(raw)
	}
	if err != nil {
		return nil, err
	}
	if s.Name == "" {
		s.Name = name
	}
	if s.Version == "" {
		s.Version = strings.TrimSpace(version)
	}
	return s, nil
}

// —— DTO ——

type shCat struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// shSkillDTO SkillHub v0 列表条目
type shSkillDTO struct {
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	Version       string  `json:"version"`
	Description   string  `json:"description"`
	DescriptionZh string  `json:"description_zh"`
	Category      string  `json:"category"`
	Downloads     int     `json:"downloads"`
	Installs      int     `json:"installs"`
	Stars         int     `json:"stars"`
	SubCategories []shCat `json:"subCategories"`
	Namespace     struct {
		CanonicalName string `json:"canonicalName"`
		Handle        string `json:"handle"`
		PublicSlug    string `json:"publicSlug"`
	} `json:"namespace"`
}

func (d shSkillDTO) subCategoryNames() []string {
	if len(d.SubCategories) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.SubCategories))
	for _, c := range d.SubCategories {
		if n := strings.TrimSpace(c.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// hubSkillDTO 通用 registry 列表条目（字段名兼容多种写法）
type hubSkillDTO struct {
	Name        string   `json:"name"`
	SkillName   string   `json:"skill_name"`
	Slug        string   `json:"slug"`
	DisplayName string   `json:"display_name"`
	Version     string   `json:"version"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Downloads   int      `json:"downloads"`
}

type packageDTO struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Tags        []string
	Content     string `json:"content"`
	Files       []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
	// 兼容 {skill: {...}} 包装
	Skill *packageDTO `json:"skill"`
}

func parseJSON(raw []byte) (*skill.Skill, error) {
	var dto packageDTO
	if err := json.Unmarshal(raw, &dto); err != nil {
		return nil, fmt.Errorf("%w: 技能包 JSON 无法解析: %v", skill.ErrHubResponse, err)
	}
	if dto.Skill != nil {
		dto = *dto.Skill
	}
	s := &skill.Skill{
		Name:        strings.TrimSpace(dto.Name),
		Version:     strings.TrimSpace(dto.Version),
		Title:       strings.TrimSpace(dto.Title),
		Description: strings.TrimSpace(dto.Description),
		Category:    strings.ToLower(strings.TrimSpace(dto.Category)),
		Tags:        dto.Tags,
		Content:     dto.Content,
	}
	for _, f := range dto.Files {
		s.Files = append(s.Files, skill.File{Path: path.Clean(f.Path), Content: f.Content, Size: len(f.Content)})
	}
	if s.Content == "" {
		for _, f := range s.Files {
			if strings.EqualFold(f.Path, "SKILL.md") {
				s.Content = f.Content
			}
		}
	}
	if s.Content == "" && len(s.Files) == 0 {
		return nil, fmt.Errorf("%w: 技能包为空", skill.ErrHubResponse)
	}
	return s, nil
}

// parseZip 解析 zip 技能包：只收文本文件，跳过目录与二进制；
// 若全部条目共享同一个顶层目录则去掉该前缀。
func parseZip(raw []byte) (*skill.Skill, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: zip 解析失败: %v", skill.ErrHubResponse, err)
	}
	type entry = archiveEntry
	var entries []entry
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if isMetaFile(f.Name) {
			continue // _meta.json 之类是市场写入的元信息，不进技能目录
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: 读取 %s 失败", skill.ErrHubResponse, f.Name)
		}
		content, err := io.ReadAll(io.LimitReader(rc, maxPackageSize))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: 读取 %s 失败", skill.ErrHubResponse, f.Name)
		}
		if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			continue // 二进制资源（图片/二进制脚本）暂不入库
		}
		entries = append(entries, entry{name: f.Name, content: string(content)})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: zip 内没有文本文件", skill.ErrHubResponse)
	}
	prefix := commonPrefix(entries)
	s := &skill.Skill{}
	if prefix != "" {
		s.Name = strings.TrimSuffix(prefix, "/")
	}
	for _, e := range entries {
		name := strings.TrimPrefix(e.name, prefix)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, "SKILL.md") {
			s.Content = e.content
			continue // 正文单独存 content，避免与文件表重复
		}
		if strings.EqualFold(name, "manifest.json") {
			var meta struct {
				Name        string   `json:"name"`
				Version     string   `json:"version"`
				Title       string   `json:"title"`
				Description string   `json:"description"`
				Category    string   `json:"category"`
				Tags        []string `json:"tags"`
			}
			if json.Unmarshal([]byte(e.content), &meta) == nil {
				if meta.Name != "" {
					s.Name = meta.Name
				}
				s.Version, s.Title = meta.Version, meta.Title
				s.Description, s.Category, s.Tags = meta.Description, meta.Category, meta.Tags
			}
			continue
		}
		s.Files = append(s.Files, skill.File{Path: path.Clean(name), Content: e.content, Size: len(e.content)})
	}
	if s.Content == "" && len(s.Files) == 0 {
		return nil, fmt.Errorf("%w: zip 内没有技能内容", skill.ErrHubResponse)
	}
	return s, nil
}

func isMetaFile(name string) bool {
	base := strings.ToLower(path.Base(name))
	return base == "_meta.json" || base == ".ds_store"
}

// commonPrefix 若所有条目都在同一个顶层目录下，返回该目录前缀（含 /）
func commonPrefix(entries []archiveEntry) string {
	if len(entries) == 0 {
		return ""
	}
	first := entries[0].name
	idx := strings.Index(first, "/")
	if idx <= 0 {
		return ""
	}
	prefix := first[:idx+1]
	for _, e := range entries {
		if !strings.HasPrefix(e.name, prefix) {
			return ""
		}
	}
	return prefix
}

// —— HTTP ——

func (c *Client) getWithType(ctx context.Context, hub skill.Hub, endpoint string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json, application/zip")
	req.Header.Set("User-Agent", "go-agent-skillhub/1.0")
	if token := strings.TrimSpace(hub.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("访问技能市场失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxPackageSize))
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("技能市场返回 %d（%s）: %s",
			resp.StatusCode, truncateStr(endpoint, 120), truncate(raw, 300))
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

func isZip(raw []byte) bool {
	return len(raw) > 4 && raw[0] == 'P' && raw[1] == 'K'
}

// describeBody 把响应特征压缩成一句可读的诊断信息（用于「无法解析」类报错）
func describeBody(contentType string, raw []byte) string {
	kind := "内容为空"
	if len(raw) > 0 {
		trimmed := bytes.TrimSpace(raw)
		switch {
		case isZip(trimmed):
			kind = "返回的是 zip 包"
		case len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '['):
			kind = "返回 JSON 但字段不匹配"
		case bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<!doctype")) ||
			bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<html")):
			kind = "返回 HTML 页面（该地址可能是网页而不是 API，可尝试 api.skillhub.cn）"
		default:
			kind = "返回内容不是 JSON"
		}
	}
	if ct := strings.TrimSpace(contentType); ct != "" {
		return kind + "；Content-Type=" + ct
	}
	return kind
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
