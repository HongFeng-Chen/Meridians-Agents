package rag

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/pkg/egress"
	"go_agent/internal/pkg/identity"
)

// 本文件：文档原文读取（知识空间详情弹层）与在线 URL 导入（网页正文抓取入库）。

// DocContent 取回一篇文档的原文分块（按当前身份可见性校验）。
//
// 原文以 Milvus 里按 doc_id 过滤的分块为准——它与检索命中的是同一份数据，
// 弹层里看到的片段和问答引用的片段保证一致；仅建图谱索引的旧文档这里为空。
func (r *Rag) DocContent(ctx context.Context, docID string, limit int) (*Document, []Hit, error) {
	if strings.TrimSpace(docID) == "" {
		return nil, nil, fmt.Errorf("doc_id 不能为空")
	}
	doc, err := r.repo.Get(ctx, docID)
	if err != nil {
		return nil, nil, err
	}
	if doc == nil {
		return nil, nil, fmt.Errorf("文档不存在: %s", docID)
	}
	// 可见性：必须落在当前身份的文档白名单里（空间授权 / 三层作用域）。
	ids, err := r.visibleDocIDs(ctx, identity.UserID(ctx))
	if err != nil {
		return nil, nil, err
	}
	ok := false
	for _, id := range ids {
		if id == docID {
			ok = true
			break
		}
	}
	if !ok {
		return nil, nil, fmt.Errorf("无权查看文档 %s", docID)
	}
	if limit <= 0 {
		limit = 100
	}
	filter := fmt.Sprintf("doc_id == %q", docID)
	items, err := r.store.GetTexts(ctx, filter, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("读取文档分块: %w", err)
	}
	return doc, items, nil
}

// DocGraph 取某文档贡献的图谱实体与关系（文档详情弹层「图谱」Tag 的数据源）。
// 可见性与 DocContent 同一道闸；仅图谱缺失时返回空集而不是报错。
func (r *Rag) DocGraph(ctx context.Context, docID string, limit int) ([]graphrag.Entity, []graphrag.Relation, error) {
	doc, err := r.repo.Get(ctx, docID)
	if err != nil {
		return nil, nil, err
	}
	if doc == nil {
		return nil, nil, fmt.Errorf("文档不存在: %s", docID)
	}
	ids, err := r.visibleDocIDs(ctx, identity.UserID(ctx))
	if err != nil {
		return nil, nil, err
	}
	ok := false
	for _, id := range ids {
		if id == docID {
			ok = true
			break
		}
	}
	if !ok {
		return nil, nil, fmt.Errorf("无权查看文档 %s", docID)
	}
	if doc.Mode != ModeGraph && doc.Mode != ModeBoth {
		return nil, nil, nil // 未建图谱：空集，由调用方给「未建」提示
	}
	return r.graph.DocGraph(ctx, docID, limit)
}

// IngestUrlInput 在线 URL 导入参数（语义同 IngestInput，来源换成 url）。
type IngestUrlInput struct {
	URL     string
	DocID   string
	Source  string
	Mode    string
	SpaceID string
}

// urlFetchTimeout / urlMaxBytes 抓取上限：20 秒、10MB，防止大页面拖垮入库。
const (
	urlFetchTimeout = 20 * time.Second
	urlMaxBytes     = 10 << 20
)

// IngestUrl 拉取网页正文并入库。
//
// 出站安全与 MCP Swagger 代理同一道闸门：URL 预校验（scheme/port/主机）+
// Dial 阶段 IP 复检 + 重定向校验（GuardedClient），私网地址需在白名单。
func (r *Rag) IngestUrl(ctx context.Context, in IngestUrlInput) (*IngestResult, error) {
	raw := strings.TrimSpace(in.URL)
	if raw == "" {
		return nil, fmt.Errorf("url 不能为空")
	}
	ReportProgress(ctx, "fetch", "校验出站策略")
	if err := egress.ValidateURL(raw); err != nil {
		return nil, err
	}
	ReportProgress(ctx, "fetch", "获取网页内容中")
	text, title, err := fetchURLText(raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("网页没有可提取的正文内容")
	}
	ReportProgress(ctx, "parse", "解析内容中（HTML → 纯文本）")
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = firstNonEmptyStr(title, raw)
	}
	ReportProgress(ctx, "clean", "文档清洗（去脚本样式，规整正文）")
	return r.IngestDocument(ctx, IngestInput{
		DocID:   in.DocID,
		Source:  source,
		Mode:    in.Mode,
		Text:    text,
		SpaceID: in.SpaceID,
	})
}

// fetchURLText 抓取 URL 并转成纯文本。返回 (正文, <title>, error)。
func fetchURLText(raw string) (string, string, error) {
	client := egress.Client(urlFetchTimeout, egress.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), urlFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Meridians-KB/1.0 (+knowledge-ingest)")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("网页拉取失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("网页返回状态码 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, urlMaxBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("读取网页内容失败: %w", err)
	}
	if len(body) > urlMaxBytes {
		return "", "", fmt.Errorf("网页超过大小上限（%d MB）", urlMaxBytes>>20)
	}
	return htmlToText(string(body))
}

var (
	// Go 的 regexp（RE2）不支持反向引用，script/style 等成对标签逐个编译
	reDropBlocks = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`),
		regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`),
		regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`),
		regexp.MustCompile(`(?is)<svg[^>]*>.*?</svg>`),
		regexp.MustCompile(`(?is)<template[^>]*>.*?</template>`),
		regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`),
	}
	reComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reTitle   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reBlock   = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6]|section|article|blockquote|pre|table|ul|ol|dt|dd|hr)[^>]*>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpaces  = regexp.MustCompile(`[ \t\r\f]+`)
	reBlanks  = regexp.MustCompile(`\n{3,}`)
)

// htmlToText 极简 HTML → 纯文本：去 script/style/注释，块级标签转换行，
// 实体解码。目标是喂给分块器的一份干净正文，不追求浏览器级渲染。
func htmlToText(s string) (string, string, error) {
	title := ""
	if m := reTitle.FindStringSubmatch(s); len(m) > 1 {
		title = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	for _, re := range reDropBlocks {
		s = re.ReplaceAllString(s, " ")
	}
	s = reComment.ReplaceAllString(s, " ")
	// 块级/换行类标签转换行，行内标签直接去掉
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(reSpaces.ReplaceAllString(ln, " "))
	}
	out := strings.TrimSpace(reBlanks.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
	return out, title, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

