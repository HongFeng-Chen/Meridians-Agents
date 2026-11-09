// service.go GraphRAG 业务编排：图谱构建（摄取）与图谱问答。
package graphrag

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go_agent/internal/biz/prompt"

	"github.com/go-kratos/kratos/v3/log"
)

// Service GraphRAG 服务
type Service struct {
	extractor *Extractor
	embed     Embedder
	llm       Chatter
	prompts   Prompter
	store     Store
	evidence  Evidence
	splitter  Splitter
	opts      Options
}

// Deps 构造依赖
type Deps struct {
	Store    Store
	Embedder Embedder
	LLM      Chatter
	Prompts  Prompter
	Evidence Evidence
	Splitter Splitter
	Options  Options
}

// New 创建 GraphRAG 服务并确保图集合存在
func New(ctx context.Context, d Deps) (*Service, error) {
	if d.Options.TopK <= 0 {
		d.Options.TopK = 8
	}
	if d.Options.Hops <= 0 {
		d.Options.Hops = 2
	}
	if d.Options.MaxEdges <= 0 {
		d.Options.MaxEdges = 200
	}
	if d.Options.MaxEntities <= 0 {
		d.Options.MaxEntities = 10
	}
	if d.Options.MaxRelations <= 0 {
		d.Options.MaxRelations = 8
	}
	if err := d.Store.EnsureCollections(ctx); err != nil {
		return nil, err
	}
	return &Service{
		extractor: NewExtractor(d.LLM, d.Prompts, d.Options.MaxEntities, d.Options.MaxRelations),
		embed:     d.Embedder,
		llm:       d.LLM,
		prompts:   d.Prompts,
		store:     d.Store,
		evidence:  d.Evidence,
		splitter:  d.Splitter,
		opts:      d.Options,
	}, nil
}

// graph 查询期内存子图（实体按 name 归一化去重合并）
type graph struct {
	entities  map[string]*Entity
	relations []Relation
}

func newGraph() *graph {
	return &graph{entities: make(map[string]*Entity)}
}

func (g *graph) addEntity(e Entity) {
	key := normName(e.Name)
	if exist, ok := g.entities[key]; ok {
		exist.Freq += e.Freq
		if len(e.Description) > len(exist.Description) {
			exist.Description = e.Description
		}
		exist.ChunkRefs = mergeRefs(exist.ChunkRefs, e.ChunkRefs)
		return
	}
	cp := e
	cp.ChunkRefs = append([]string(nil), e.ChunkRefs...)
	g.entities[key] = &cp
}

func (g *graph) addRelation(r Relation) {
	g.relations = append(g.relations, r)
}

func (g *graph) entityNames() []string {
	out := make([]string, 0, len(g.entities))
	for _, e := range g.entities {
		out = append(out, e.Name)
	}
	return out
}

// Ingest 构建图谱：分块 -> 并发抽取 -> 合并去重 -> 向量化入库
func (s *Service) Ingest(ctx context.Context, docID, source, text string) (entities int, relations int, err error) {
	chunks := s.splitter.Chunk(text)
	if len(chunks) == 0 {
		return 0, 0, fmt.Errorf("no chunks generated (empty text?)")
	}

	// 并发抽取（信号量限流）
	type result struct {
		idx int
		ex  *Extraction
		err error
	}
	results := make([]result, len(chunks))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, ch := range chunks {
		wg.Add(1)
		go func(i int, ch string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ex, err := s.extractor.Extract(ctx, ch)
			results[i] = result{idx: i, ex: ex, err: err}
		}(i, ch)
	}
	wg.Wait()

	// 合并：实体按 name 归一化去重，关系按 source|target|relation 去重
	entMap := make(map[string]*Entity)
	relMap := make(map[string]*Relation)
	for _, r := range results {
		if r.err != nil {
			return 0, 0, fmt.Errorf("extract chunk %d: %w", r.idx, r.err)
		}
		for _, e := range r.ex.Entities {
			key := normName(e.Name)
			if exist, ok := entMap[key]; ok {
				exist.Freq++
				if len(e.Description) > len(exist.Description) {
					exist.Description = e.Description
				}
				exist.ChunkRefs = mergeRefs(exist.ChunkRefs, []string{refOf(docID, r.idx)})
			} else {
				cp := e
				cp.Freq = 1
				cp.ChunkRefs = []string{refOf(docID, r.idx)}
				entMap[key] = &cp
			}
		}
		for _, rel := range r.ex.Relations {
			key := normName(rel.Source) + "|" + normName(rel.Target) + "|" + normName(rel.Relation)
			if exist, ok := relMap[key]; ok {
				exist.Weight++
				exist.ChunkRefs = mergeRefs(exist.ChunkRefs, []string{refOf(docID, r.idx)})
			} else {
				cp := rel
				cp.Weight = 1
				cp.ChunkRefs = []string{refOf(docID, r.idx)}
				relMap[key] = &cp
			}
		}
	}

	// 与库中已有实体合并（跨文档去重）。
	// 合并前先摘掉「本文档自己的旧引用」：否则重新入库时旧版本引用会被原样
	// 合并进新节点，等于把过期内容永久固化在图里。
	docPrefix := docID + "#"
	names := make([]string, 0, len(entMap))
	for _, e := range entMap {
		names = append(names, e.Name)
	}
	existing, err := s.store.ExistingEntities(ctx, names)
	if err != nil {
		return 0, 0, err
	}
	for k, e := range entMap {
		if old, ok := existing[normName(e.Name)]; ok {
			others := dropPrefixRefs(old.ChunkRefs, docPrefix)
			e.Freq += len(others)
			if len(old.Description) > len(e.Description) {
				e.Description = old.Description
			}
			e.ChunkRefs = mergeRefs(e.ChunkRefs, others)
			entMap[k] = e
		}
	}

	// 实体向量化入库
	ents := sortedEntities(entMap)
	contents := make([]string, len(ents))
	for i, e := range ents {
		contents[i] = entityContent(e)
	}
	entVecs, err := s.embed.Embed(ctx, contents)
	if err != nil {
		return 0, 0, fmt.Errorf("embed entities: %w", err)
	}

	// 关系向量化（与实体一起先算完，失败时旧版本图谱仍然可用）
	rels := sortedRelations(relMap)
	relDescs := make([]string, len(rels))
	for i, r := range rels {
		relDescs[i] = relationContent(r)
	}
	relVecs, err := s.embed.Embed(ctx, relDescs)
	if err != nil {
		return 0, 0, fmt.Errorf("embed relations: %w", err)
	}

	// 旧版本清理：摘掉本文档的全部旧引用，只删「没有别人支撑」的节点/边，
	// 共享节点保留并改写 refs。放在所有 embedding 之后，前面任何一步失败
	// 都不会动到已有图谱。
	if _, _, derr := s.store.DetachDocRefs(ctx, docPrefix); derr != nil {
		return 0, 0, fmt.Errorf("清理旧图谱引用: %w", derr)
	}

	if err := s.store.InsertEntities(ctx, ents, contents, entVecs); err != nil {
		return 0, 0, err
	}
	if err := s.store.InsertRelations(ctx, rels, relVecs); err != nil {
		return 0, 0, err
	}
	return len(ents), len(rels), nil
}

// Stats 图谱规模统计
type Stats struct {
	Entities  int64 `json:"entities"`
	Relations int64 `json:"relations"`
}

// Stats 返回实体与关系数量
func (s *Service) Stats(ctx context.Context) (Stats, error) {
	ents, rels, err := s.store.Counts(ctx)
	if err != nil {
		return Stats{}, err
	}
	return Stats{Entities: ents, Relations: rels}, nil
}

// DeleteDocGraph 删除指定文档的图谱贡献：只被它引用的节点/边删除，
// 与其他文档共享的节点保留但摘掉该文档的引用（否则删了文档、图谱里还挂着它的旧证据）
func (s *Service) DeleteDocGraph(ctx context.Context, docID string) (entities int, relations int, err error) {
	return s.store.DetachDocRefs(ctx, docID+"#")
}

// dropPrefixRefs 去掉属于该文档的引用，保留其他来源的
func dropPrefixRefs(refs []string, prefix string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if strings.HasPrefix(r, prefix) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Query 图谱问答：实体混合检索 -> 关系多跳扩展 -> 子图上下文 + 原文证据 -> LLM 生成
func (s *Service) Query(ctx context.Context, question string) (*GraphAnswer, error) {
	g, rels, empty, err := s.recall(ctx, question)
	if err != nil {
		return nil, err
	}
	if empty {
		return &GraphAnswer{Question: question, Answer: "知识图谱中未找到相关实体，无法回答。"}, nil
	}
	// 组装子图上下文并生成（无作用域：Query 由平台内部调用，不面向终端用户身份）
	return s.generate(ctx, question, g, rels, nil)
}

// Recall 图谱召回（只检索不生成）：实体混合检索 + 关系多跳扩展，返回子图。
// 供会话等场景把图谱上下文注入 LLM 提示词使用。
func (s *Service) Recall(ctx context.Context, question string) (*GraphAnswer, error) {
	g, rels, empty, err := s.recall(ctx, question)
	if err != nil {
		return nil, err
	}
	if empty {
		return &GraphAnswer{Question: question}, nil
	}
	ents := make([]Entity, 0, len(g.entities))
	for _, e := range g.entities {
		ents = append(ents, *e)
	}
	return &GraphAnswer{Question: question, Entities: ents, Relations: rels}, nil
}

// QueryScoped 按文档白名单隔离的图谱问答：召回后先裁剪到本人文档的实体/关系再生成。
// docIDs 为空表示该用户没有任何文档 → 直接返回"无相关实体"，不做跨用户召回。
func (s *Service) QueryScoped(ctx context.Context, question string, docIDs []string) (*GraphAnswer, error) {
	if len(docIDs) == 0 {
		return &GraphAnswer{Question: question, Answer: "你当前没有已入库的文档，无法回答。"}, nil
	}
	g, rels, empty, err := s.recall(ctx, question)
	if err != nil {
		return nil, err
	}
	if empty {
		return &GraphAnswer{Question: question, Answer: "知识图谱中未找到相关实体，无法回答。"}, nil
	}
	g, rels = scopeByDocs(g, rels, docIDs)
	if len(g.entities) == 0 {
		return &GraphAnswer{Question: question, Answer: "你的文档中未找到相关实体，无法回答。"}, nil
	}
	return s.generate(ctx, question, g, rels, docIDs)
}

// RecallScoped 按文档白名单隔离的图谱召回（只检索不生成）。
func (s *Service) RecallScoped(ctx context.Context, question string, docIDs []string) (*GraphAnswer, error) {
	if len(docIDs) == 0 {
		return &GraphAnswer{Question: question}, nil
	}
	g, rels, empty, err := s.recall(ctx, question)
	if err != nil {
		return nil, err
	}
	if empty {
		return &GraphAnswer{Question: question}, nil
	}
	g, rels = scopeByDocs(g, rels, docIDs)
	ents := make([]Entity, 0, len(g.entities))
	for _, e := range g.entities {
		ents = append(ents, *e)
	}
	return &GraphAnswer{Question: question, Entities: ents, Relations: rels}, nil
}

// scopeByDocs 按文档归属裁剪子图：实体/关系的 chunk_refs 形如 "doc_id#chunk_idx"。
//
// 两件事都必须做，少一件就等于没隔离：
//
//  1. 丢弃「没有任何自有引用」的实体/关系（fail closed，无归属节点不跨用户共享）；
//  2. **把保留下来的实体/关系里的 ChunkRefs 也裁掉别人的部分**。
//
// 只做第 1 步是这个模块此前的实现，也是这个漏洞的由来：实体 Milvus 同时被
// Alice 的 A-doc 和 Bob 的 B-doc 引用，Bob 查询时实体因 B-doc 被保留，
// 但 ChunkRefs 里仍留着 A-doc#0 —— 后续 resolveEvidence 会按它去读原文，
// 把 Alice 的文档内容拼进 Bob 的 Prompt。裁剪引用计数不解决这个问题，
// 泄漏的是原文。
//
// 关系还有额外一关：源/目标实体不在保留集合里就不保留关系，
// 否则会通过关系描述泄漏只属于他人文档的实体名。
func scopeByDocs(g *graph, rels []Relation, docIDs []string) (*graph, []Relation) {
	allowed := make(map[string]struct{}, len(docIDs))
	for _, id := range docIDs {
		allowed[id] = struct{}{}
	}
	// filterRefs 只保留自有文档的引用；顺序不变，便于证据按热度截断
	filterRefs := func(refs []string) []string {
		out := make([]string, 0, len(refs))
		for _, ref := range refs {
			i := strings.LastIndex(ref, "#")
			if i <= 0 {
				continue
			}
			if _, ok := allowed[ref[:i]]; ok {
				out = append(out, ref)
			}
		}
		return out
	}

	out := newGraph()
	for key, e := range g.entities {
		refs := filterRefs(e.ChunkRefs)
		if len(refs) == 0 {
			continue
		}
		cp := *e
		cp.ChunkRefs = refs
		out.entities[key] = &cp
	}

	kept := make([]Relation, 0, len(rels))
	for _, rel := range rels {
		refs := filterRefs(rel.ChunkRefs)
		if len(refs) == 0 {
			continue
		}
		// 两端实体都必须在保留集合内（否则关系本身就在描述别人的实体）
		if _, ok := out.entities[normName(rel.Source)]; !ok {
			continue
		}
		if _, ok := out.entities[normName(rel.Target)]; !ok {
			continue
		}
		cp := rel
		cp.ChunkRefs = refs
		kept = append(kept, cp)
	}
	return out, kept
}

// recall 召回公共步骤：返回子图、关系；empty 表示图中无相关实体
func (s *Service) recall(ctx context.Context, question string) (*graph, []Relation, bool, error) {
	qvec, err := s.embed.EmbedOne(ctx, question)
	if err != nil {
		return nil, nil, false, fmt.Errorf("embed question: %w", err)
	}

	// 1. 实体混合检索
	seeds, err := s.store.SearchEntities(ctx, question, qvec, s.opts.TopK)
	if err != nil {
		return nil, nil, false, err
	}
	if len(seeds) == 0 {
		return nil, nil, true, nil
	}

	// 2. 沿关系边扩展（最多 hops 跳）
	g := newGraph()
	seen := make(map[string]struct{})
	frontier := make([]string, 0, len(seeds))
	for _, e := range seeds {
		g.addEntity(e)
		seen[normName(e.Name)] = struct{}{}
		frontier = append(frontier, e.Name)
	}
	var allRels []Relation
	for hop := 0; hop < s.opts.Hops && len(frontier) > 0; hop++ {
		rels, err := s.store.RelationsOf(ctx, frontier, s.opts.MaxEdges)
		if err != nil {
			return nil, nil, false, err
		}
		next := make([]string, 0)
		for _, r := range rels {
			g.addRelation(r)
			for _, name := range []string{r.Source, r.Target} {
				key := normName(name)
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					next = append(next, name)
				}
			}
		}
		allRels = append(allRels, rels...)
		frontier = next
	}

	// 3. 补全扩展出的节点描述
	discovered, err := s.store.EntitiesByNames(ctx, g.entityNames())
	if err != nil {
		return nil, nil, false, err
	}
	for _, e := range discovered {
		g.addEntity(e)
	}
	return g, allRels, false, nil
}

// generate 组装图谱上下文与证据，调用 LLM 生成
// generate 生成答案。scope 为文档白名单：非空时证据解析再按它过滤一遍
// （scopeByDocs 已裁过引用，这里是第二道闸——防止将来有人改动裁剪逻辑
// 或从别的路径拼进未裁剪的实体）。
func (s *Service) generate(ctx context.Context, question string, g *graph, rels []Relation, scope []string) (*GraphAnswer, error) {
	ents := make([]Entity, 0, len(g.entities))
	for _, e := range g.entities {
		ents = append(ents, *e)
	}
	// 实体按出现频率降序，控制上下文规模
	sort.Slice(ents, func(i, j int) bool { return ents[i].Freq > ents[j].Freq })
	if len(ents) > 30 {
		ents = ents[:30]
	}
	sort.SliceStable(rels, func(i, j int) bool { return rels[i].Weight > rels[j].Weight })
	if len(rels) > 40 {
		rels = rels[:40]
	}

	var sb strings.Builder
	sb.WriteString("知识图谱实体：\n")
	for i, e := range ents {
		fmt.Fprintf(&sb, "[E%d] %s（%s，出现%d次）：%s\n", i+1, e.Name, e.Type, e.Freq, e.Description)
	}
	sb.WriteString("\n知识图谱关系：\n")
	for i, r := range rels {
		fmt.Fprintf(&sb, "[R%d] %s --%s--> %s：%s\n", i+1, r.Source, r.Relation, r.Target, r.Description)
	}

	// 5. 解析实体/关系引用的原文证据
	evidence := s.resolveEvidence(ctx, g, rels, 6, scope)
	if len(evidence) > 0 {
		sb.WriteString("\n相关原文片段：\n")
		for i, ev := range evidence {
			fmt.Fprintf(&sb, "[D%d] %s\n", i+1, ev)
		}
	}

	systemPrompt := renderPrompt(ctx, s.prompts, prompt.KeyGraphSystem, nil)
	userPrompt := renderPrompt(ctx, s.prompts, prompt.KeyGraphUser, map[string]string{
		"context":  sb.String(),
		"question": question,
	})

	ans, err := s.llm.Chat(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("generate: %w", err)
	}

	return &GraphAnswer{
		Question:  question,
		Answer:    ans,
		Entities:  ents,
		Relations: rels,
		Evidence:  evidence,
	}, nil
}

// resolveEvidence 解析 chunk 引用，取回原文片段（按 freq/weight 优先级截断）。
// scope 非空时只解析白名单内的文档引用。
func (s *Service) resolveEvidence(ctx context.Context, g *graph, rels []Relation, max int, scope []string) []string {
	if s.evidence == nil {
		return nil
	}
	var allowed map[string]struct{}
	if len(scope) > 0 {
		allowed = make(map[string]struct{}, len(scope))
		for _, id := range scope {
			allowed[id] = struct{}{}
		}
	}
	byDoc := make(map[string][]int64)
	addRef := func(ref string) {
		i := strings.LastIndex(ref, "#")
		if i <= 0 || i == len(ref)-1 {
			return
		}
		if allowed != nil {
			if _, ok := allowed[ref[:i]]; !ok {
				return
			}
		}
		var idx int64
		if _, err := fmt.Sscanf(ref[i+1:], "%d", &idx); err != nil {
			return
		}
		docID := ref[:i]
		byDoc[docID] = append(byDoc[docID], idx)
	}
	for _, e := range g.entities {
		for _, r := range e.ChunkRefs {
			addRef(r)
		}
	}
	for _, r := range rels {
		for _, ref := range r.ChunkRefs {
			addRef(ref)
		}
	}
	if len(byDoc) == 0 {
		return nil
	}

	var out []string
	for docID, idxs := range byDoc {
		if len(out) >= max {
			break
		}
		if len(idxs) > 10 {
			idxs = idxs[:10]
		}
		texts, err := s.evidence.Texts(ctx, docID, idxs)
		if err != nil {
			continue // 证据获取失败不阻断主流程
		}
		for _, t := range texts {
			out = append(out, t)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

func refOf(docID string, idx int) string {
	return fmt.Sprintf("%s#%d", docID, idx)
}

// renderPrompt 渲染提示词：管理端生效版本优先，异常时回落内置默认。
func renderPrompt(ctx context.Context, p Prompter, key string, vars map[string]string) string {
	if p != nil {
		s, err := p.Render(ctx, key, vars)
		if err == nil && strings.TrimSpace(s) != "" {
			return s
		}
		if err != nil {
			log.Warn("[graphrag] 渲染提示词失败，回落内置默认", "key", key, "err", err)
		}
	}
	if s, ok := prompt.RenderBuiltin(key, vars); ok {
		return s
	}
	return ""
}

func entityContent(e Entity) string {
	return fmt.Sprintf("%s %s: %s", e.Name, e.Type, e.Description)
}

func relationContent(r Relation) string {
	return fmt.Sprintf("%s --%s--> %s: %s", r.Source, r.Relation, r.Target, r.Description)
}

func sortedEntities(m map[string]*Entity) []Entity {
	out := make([]Entity, 0, len(m))
	for _, e := range m {
		out = append(out, *e)
	}
	return out
}

func sortedRelations(m map[string]*Relation) []Relation {
	out := make([]Relation, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	return out
}
