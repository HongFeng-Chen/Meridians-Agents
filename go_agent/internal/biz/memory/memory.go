package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"go_agent/internal/biz/history"
	"go_agent/internal/biz/prompt"
	"go_agent/internal/biz/rag"
	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/conf"

	"github.com/go-kratos/kratos/v3/log"
)

// UseCase 记忆系统用例
type UseCase struct {
	repo       LongTermRepo
	short      ShortTermStore
	vec        VectorStore
	graph      GraphStore
	extractor  *graphrag.Extractor
	embed      rag.Embedder
	llm        rag.Chatter
	prompts    rag.Prompter
	shortTTL   time.Duration
	recallTopK int
	historyLim int
	graphOn    bool
	maxEnt     int
	maxRel     int
	// hist 统一操作历史（记忆召回记录，可为 nil：不记录）
	hist history.Recorder
	// conv 会话消息来源（可为 nil）：短期窗口为空时回源会话消息并反写 Redis
	conv ConversationSource
}

// New 创建记忆用例并确保向量集合存在（wire provider）
func New(
	repo LongTermRepo,
	short ShortTermStore,
	vec VectorStore,
	graph GraphStore,
	embed rag.Embedder,
	llm rag.Chatter,
	prompts rag.Prompter,
	hist history.Recorder,
	c *conf.Data,
) (*UseCase, error) {
	mc := c.GetMemory()
	shortTTL := mc.GetShortTTL().AsDuration()
	if shortTTL <= 0 {
		shortTTL = 24 * time.Hour
	}
	topK := int(mc.GetRecallTopK())
	if topK <= 0 {
		topK = 8
	}
	histLim := int(mc.GetHistoryLimit())
	if histLim <= 0 {
		histLim = 20
	}
	maxEnt := int(mc.GetGraphMaxEntities())
	if maxEnt <= 0 {
		maxEnt = 8
	}
	maxRel := int(mc.GetGraphMaxRelations())
	if maxRel <= 0 {
		maxRel = 6
	}
	graphOn := mc.GetGraphEnabled()

	u := &UseCase{
		repo:       repo,
		short:      short,
		vec:        vec,
		graph:      graph,
		embed:      embed,
		llm:        llm,
		prompts:    prompts,
		shortTTL:   shortTTL,
		recallTopK: topK,
		historyLim: histLim,
		graphOn:    graphOn,
		maxEnt:     maxEnt,
		maxRel:     maxRel,
		hist:       hist,
	}
	u.extractor = graphrag.NewExtractor(llm, prompts, maxEnt, maxRel)

	// 内置记忆规则种子（事实/画像/技能，幂等）
	seedBuiltinRules(context.Background(), repo)

	// 启动即确保记忆向量集合存在且已加载（残缺集合自愈补索引）
	readyCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := vec.EnsureCollection(readyCtx); err != nil {
		return nil, fmt.Errorf("初始化记忆向量集合: %w", err)
	}
	return u, nil
}

// normalize 补齐入参默认值
func normalize(in *RememberInput) error {
	in.Content = strings.TrimSpace(in.Content)
	if in.Content == "" {
		return fmt.Errorf("memory: content 不能为空")
	}
	if in.UserID == "" {
		in.UserID = "default"
	}
	if in.Kind == "" {
		in.Kind = KindFact
	}
	if in.TTL > 0 {
		t := time.Now().Add(in.TTL)
		in.ExpiresAt = &t
	}
	return nil
}

// checksumOf 内容指纹：同用户同内容视为同一条记忆
func checksumOf(userID, content string) string {
	sum := sha256.Sum256([]byte(userID + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

// newID 生成记忆 ID（mem- + 16 位随机 hex）
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "mem-" + hex.EncodeToString(b)
}

// Remember 写入长期记忆：MySQL 权威落库（含过期时间）-> Milvus 向量化（支持召回）
// -> GraphRAG 抽取实体关系入图谱（可关，失败降级为警告不阻断）。
func (u *UseCase) Remember(ctx context.Context, in RememberInput) (*RememberResult, error) {
	if err := normalize(&in); err != nil {
		return nil, err
	}
	sum := checksumOf(in.UserID, in.Content)

	// 内容级去重：同用户同内容直接复用已有记忆，不重复向量化/抽取
	if old, err := u.repo.FindByChecksum(ctx, in.UserID, sum); err == nil && old != nil && !old.ExpiredAt(time.Now()) {
		return &RememberResult{Memory: old, Deduped: true}, nil
	}

	m := &LongTermMemory{
		ID:         in.ID,
		UserID:     in.UserID,
		Kind:       in.Kind,
		Content:    in.Content,
		Importance: in.Importance,
		Tags:       in.Tags,
		Source:     in.Source,
		RuleName:   in.RuleName,
		Checksum:   sum,
		ExpiresAt:  in.ExpiresAt,
	}
	if m.ID == "" {
		m.ID = newID()
	}

	// 1. 向量化入 Milvus（召回主通道，失败则整体失败）
	vec, err := u.embed.EmbedOne(ctx, in.Content)
	if err != nil {
		return nil, fmt.Errorf("memory: 向量化失败: %w", err)
	}
	if err := u.vec.Insert(ctx, m, vec); err != nil {
		return nil, fmt.Errorf("memory: 写入向量库失败: %w", err)
	}

	// 2. MySQL 权威落库
	if err := u.repo.Save(ctx, m); err != nil {
		return nil, fmt.Errorf("memory: 写入长期记忆失败: %w", err)
	}

	// 3. GraphRAG 图谱存储（best-effort：图谱失败不阻断记忆写入）
	useGraph := in.Graph == nil || *in.Graph
	res := &RememberResult{Memory: m}
	if useGraph && u.graphOn {
		ents, rels, gerr := u.ingestGraph(ctx, m)
		if gerr != nil {
			res.GraphWarning = gerr.Error()
			log.Warn("memory: 图谱抽取失败（记忆已入库）", "id", m.ID, "err", gerr)
		} else {
			res.Entities, res.Relations = ents, rels
			m.Entities, m.Relations = ents, rels
			_ = u.repo.Save(ctx, m) // 回填图谱统计
		}
	}
	return res, nil
}

// logRecall 落检索历史并回填命中记忆的召回统计（best-effort）
func (u *UseCase) logRecall(ctx context.Context, source, query, userID string, topK int, latencyMS int64, res *RecallResult) {
	if source == "" {
		source = RecallSourceAPI
	}
	ids := make([]string, 0, len(res.Memories))
	for _, m := range res.Memories {
		ids = append(ids, m.ID)
	}
	l := &RecallLog{
		UserID:    userID,
		Query:     clipText(query, 1000),
		Source:    source,
		TopK:      topK,
		Hits:      len(res.Memories),
		MemoryIDs: ids,
		LatencyMS: latencyMS,
		CreatedAt: time.Now(),
	}
	if err := u.repo.SaveRecallLog(ctx, l); err != nil {
		log.Warn("memory: 检索历史写入失败", "err", err)
	}
	if len(ids) > 0 {
		if err := u.repo.MarkRecalled(ctx, ids, time.Now()); err != nil {
			log.Warn("memory: 召回统计回填失败", "err", err)
		}
	}
}

// ingestGraph 记忆内容 -> 实体/关系抽取 -> 与库中已有实体合并 -> 向量化入图集合。
// 记忆是短文本（单块），引用统一为 "memoryID#0"，删除时按此前缀清理孤儿节点。
func (u *UseCase) ingestGraph(ctx context.Context, m *LongTermMemory) (int, int, error) {
	ex, err := u.extractor.Extract(ctx, m.Content)
	if err != nil {
		return 0, 0, fmt.Errorf("抽取实体关系: %w", err)
	}
	ref := m.ID + "#0"

	// 与库中已有实体合并（跨记忆/文档去重，频率累加）
	names := make([]string, 0, len(ex.Entities))
	for _, e := range ex.Entities {
		names = append(names, e.Name)
	}
	existing, err := u.graph.ExistingEntities(ctx, names)
	if err != nil {
		return 0, 0, fmt.Errorf("查询已有实体: %w", err)
	}
	ents := make([]graphrag.Entity, 0, len(ex.Entities))
	entMap := make(map[string]graphrag.Entity, len(ex.Entities))
	for _, e := range ex.Entities {
		if old, ok := existing[normName(e.Name)]; ok {
			e.Freq = 1 + old.Freq
			if len(old.Description) > len(e.Description) {
				e.Description = old.Description
			}
			e.ChunkRefs = mergeRefs([]string{ref}, old.ChunkRefs)
		} else {
			e.Freq = 1
			e.ChunkRefs = []string{ref}
		}
		ents = append(ents, e)
		entMap[normName(e.Name)] = e
	}

	// 关系只保留两端都在实体集内的边
	rels := make([]graphrag.Relation, 0, len(ex.Relations))
	for _, r := range ex.Relations {
		if _, ok := entMap[normName(r.Source)]; !ok {
			continue
		}
		if _, ok := entMap[normName(r.Target)]; !ok {
			continue
		}
		r.Weight = 1
		r.ChunkRefs = []string{ref}
		rels = append(rels, r)
	}

	if len(ents) > 0 {
		contents := make([]string, len(ents))
		for i, e := range ents {
			contents[i] = entityContent(e)
		}
		vecs, err := u.embed.Embed(ctx, contents)
		if err != nil {
			return 0, 0, fmt.Errorf("向量化实体: %w", err)
		}
		if err := u.graph.InsertEntities(ctx, ents, contents, vecs); err != nil {
			return 0, 0, fmt.Errorf("写入实体: %w", err)
		}
	}
	if len(rels) > 0 {
		descs := make([]string, len(rels))
		for i, r := range rels {
			descs[i] = relationContent(r)
		}
		vecs, err := u.embed.Embed(ctx, descs)
		if err != nil {
			return 0, 0, fmt.Errorf("向量化关系: %w", err)
		}
		if err := u.graph.InsertRelations(ctx, rels, vecs); err != nil {
			return 0, 0, fmt.Errorf("写入关系: %w", err)
		}
	}
	return len(ents), len(rels), nil
}

// Recall 长期记忆召回：Milvus 混合检索（BM25+Dense）+ 过滤已过期
// + 可选图谱多跳上下文（与文档图谱共享节点，能召回跨记忆的关联）。
// 每次召回异步落一条记忆召回历史（op_histories），任务内调用自动关联 task。
func (u *UseCase) Recall(ctx context.Context, query, userID string, topK int, withGraph bool, source string) (res *RecallResult, err error) {
	start := time.Now()
	defer func() {
		latency := time.Since(start).Milliseconds()
		if u.hist == nil {
			return
		}
		rec := &history.Record{
			Kind:      history.KindMemoryRecall,
			Ref:       "vector",
			Query:     query + "@" + userID,
			OK:        err == nil,
			LatencyMS: latency,
		}
		if withGraph && u.graph != nil {
			rec.Ref = "vector+graph"
		}
		if err != nil {
			rec.Summary = err.Error()
		} else if res != nil {
			summary := fmt.Sprintf("命中 %d 条记忆", len(res.Memories))
			if res.Graph != nil {
				summary += fmt.Sprintf("，图谱 %d 实体 / %d 关系", len(res.Graph.Entities), len(res.Graph.Relations))
			}
			rec.Summary = summary
			rec.Detail = buildRecallDetail(res, withGraph)
		}
		u.hist.Record(ctx, rec)

		// 检索历史 + 召回统计回填（失败不阻断召回主流程）
		if err == nil && res != nil {
			u.logRecall(ctx, source, query, userID, topK, latency, res)
		}
	}()

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("memory: 查询为空")
	}
	if topK <= 0 {
		topK = u.recallTopK
	}
	if userID == "" {
		userID = "default"
	}
	res = &RecallResult{Query: query, UserID: userID}

	qvec, err := u.embed.EmbedOne(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("memory: 向量化查询失败: %w", err)
	}

	// 1. 向量召回 + 补全 MySQL 权威记录 + 过滤过期
	hits, err := u.vec.Search(ctx, query, qvec, topK, userID)
	if err != nil {
		return nil, err
	}
	if len(hits) > 0 {
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.ID
		}
		mems, err := u.repo.GetByIDs(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("memory: 读取记忆详情: %w", err)
		}
		byID := make(map[string]*LongTermMemory, len(mems))
		for _, m := range mems {
			byID[m.ID] = m
		}
		now := time.Now()
		for _, h := range hits {
			m, ok := byID[h.ID]
			if !ok || m.ExpiredAt(now) {
				continue // 向量残留但 MySQL 已删/已过期：跳过
			}
			res.Memories = append(res.Memories, RecalledMemory{LongTermMemory: m, Score: h.Score})
		}
	}

	// 2. 图谱召回：实体混合检索 -> 关系多跳扩展 -> 子图
	if withGraph && u.graph != nil {
		seeds, err := u.graph.SearchEntities(ctx, query, qvec, 6)
		if err != nil {
			log.Warn("memory: 图谱实体检索失败（跳过图谱召回）", "err", err)
		} else if len(seeds) > 0 {
			gc := &GraphContext{Entities: seeds}
			seen := make(map[string]string, len(seeds)) // normKey -> 原始名
			seenRels := make(map[string]struct{})
			frontier := make([]string, 0, len(seeds))
			for _, e := range seeds {
				seen[normName(e.Name)] = e.Name
				frontier = append(frontier, e.Name)
			}
			for hop := 0; hop < 2 && len(frontier) > 0; hop++ {
				rels, err := u.graph.RelationsOf(ctx, frontier, 50)
				if err != nil {
					break
				}
				frontier = frontier[:0]
				for _, r := range rels {
					relKey := normName(r.Source) + "|" + normName(r.Target) + "|" + normName(r.Relation)
					if _, dup := seenRels[relKey]; dup {
						continue
					}
					seenRels[relKey] = struct{}{}
					gc.Relations = append(gc.Relations, r)
					for _, name := range []string{r.Source, r.Target} {
						if _, ok := seen[normName(name)]; !ok {
							seen[normName(name)] = name
							frontier = append(frontier, name)
						}
					}
				}
			}
			if extras := extraNames(seen, seeds); len(extras) > 0 {
				if more, err := u.graph.EntitiesByNames(ctx, extras); err == nil {
					gc.Entities = append(gc.Entities, more...)
				}
			}
			sort.SliceStable(gc.Relations, func(i, j int) bool { return gc.Relations[i].Weight > gc.Relations[j].Weight })
			if len(gc.Relations) > 20 {
				gc.Relations = gc.Relations[:20]
			}
			res.Graph = gc
		}
	}
	return res, nil
}

// extraNames 返回 seen 中不属于种子实体的原始名（多跳扩展发现的新节点）
func extraNames(seen map[string]string, seeds []graphrag.Entity) []string {
	seedKeys := make(map[string]struct{}, len(seeds))
	for _, e := range seeds {
		seedKeys[normName(e.Name)] = struct{}{}
	}
	out := make([]string, 0, len(seen)-len(seedKeys))
	for _, orig := range seen {
		if _, ok := seedKeys[normName(orig)]; !ok {
			out = append(out, orig)
		}
	}
	return out
}

// Answer 基于记忆回答：短期历史 + 长期召回（向量+图谱）组装上下文 -> LLM 生成，
// 并把本轮问答写回短期记忆（续期）。
func (u *UseCase) Answer(ctx context.Context, sessionID, userID, question string) (*MemoryAnswer, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("memory: 问题为空")
	}
	if userID == "" {
		userID = "default"
	}

	// 1. 短期历史 + 长期召回
	history, _ := u.short.History(ctx, sessionID, u.historyLim)
	recall, err := u.Recall(ctx, question, userID, 0, true, RecallSourceAnswer)
	if err != nil {
		return nil, err
	}

	// 2. 组装上下文
	systemPrompt := u.render(ctx, prompt.KeyMemorySystem, nil)
	userPrompt := u.render(ctx, prompt.KeyMemoryUser, map[string]string{
		"history":  renderHistory(history),
		"context":  renderRecall(recall),
		"question": question,
	})

	// 3. LLM 生成
	ans, err := u.llm.Chat(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("memory: 生成回答失败: %w", err)
	}

	// 4. 本轮问答写回短期记忆（写入续期）
	now := time.Now()
	_ = u.short.Append(ctx, sessionID, Message{Role: "user", Content: question, At: now}, u.shortTTL)
	_ = u.short.Append(ctx, sessionID, Message{Role: "assistant", Content: ans, At: now}, u.shortTTL)

	return &MemoryAnswer{SessionID: sessionID, Question: question, Answer: ans, Recall: recall}, nil
}

// ---------- 短期记忆 ----------

// AppendMessage 追加一条会话消息到短期记忆（写入续期）
func (u *UseCase) AppendMessage(ctx context.Context, sessionID, role, content string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(content) == "" {
		return fmt.Errorf("memory: session_id 与 content 不能为空")
	}
	if role == "" {
		role = "user"
	}
	return u.short.Append(ctx, sessionID, Message{Role: role, Content: content, At: time.Now()}, u.shortTTL)
}

// History 会话最近消息（按时间正序）：
// 先读 Redis 短期窗口；为空且注入了会话消息来源时，回源 MySQL 会话消息
// （同 session_id = 会话 ID）并反写 Redis（带 TTL 续期），保证有内容可查。
func (u *UseCase) History(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = u.historyLim
	}
	msgs, err := u.short.History(ctx, sessionID, limit)
	if err != nil || len(msgs) > 0 || u.conv == nil {
		return msgs, err
	}
	// Redis 无数据：从会话消息表回源最近 limit 条
	src, serr := u.conv.ListRecent(ctx, sessionID, limit)
	if serr != nil || len(src) == 0 {
		return msgs, err
	}
	for _, m := range src {
		_ = u.short.Append(ctx, sessionID, Message{Role: m.Role, Content: m.Content, At: m.At}, u.shortTTL)
	}
	return u.short.History(ctx, sessionID, limit)
}

// ClearSession 清空会话短期记忆
func (u *UseCase) ClearSession(ctx context.Context, sessionID string) error {
	return u.short.Clear(ctx, sessionID)
}

// ---------- 会话消息回源 ----------

// ShortMessage 会话消息回源结构（来自 conversation 消息表的最小投影）
type ShortMessage struct {
	Role    string
	Content string
	At      time.Time
}

// ConversationSource 会话消息来源（由 data 层适配 conversation 仓库提供）。
// 可为 nil：未注入时短期窗口为空就是空，不回源。
type ConversationSource interface {
	// ListRecent 取会话最近 limit 条消息（按时间正序返回）
	ListRecent(ctx context.Context, conversationID string, limit int) ([]ShortMessage, error)
}

// SetConversationSource 注入会话消息来源（wire 装配后调用；仅启动期使用，无并发）。
func (u *UseCase) SetConversationSource(src ConversationSource) {
	u.conv = src
}

// SessionLen 会话消息数
func (u *UseCase) SessionLen(ctx context.Context, sessionID string) (int64, error) {
	return u.short.Len(ctx, sessionID)
}

// ---------- 管理 ----------

// Get 长期记忆详情（向量 + MySQL）
func (u *UseCase) Get(ctx context.Context, id string) (*LongTermMemory, error) {
	m, err := u.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrNotFound
	}
	m.Expired = m.ExpiredAt(time.Now())
	return m, nil
}

// List 记忆列表；includeExpired=false 时过滤已过期
func (u *UseCase) List(ctx context.Context, userID string, includeExpired bool, limit int) ([]*LongTermMemory, error) {
	if limit <= 0 {
		limit = 100
	}
	mems, err := u.repo.List(ctx, userID, includeExpired, limit)
	if err != nil {
		return nil, err
	}
	if !includeExpired {
		now := time.Now()
		out := mems[:0]
		for _, m := range mems {
			if !m.ExpiredAt(now) {
				out = append(out, m)
			}
		}
		mems = out
	}
	return mems, nil
}

// Delete 删除长期记忆：MySQL + 向量 + 图谱孤儿节点
func (u *UseCase) Delete(ctx context.Context, id string) (*RememberResult, error) {
	m, err := u.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// 返回被删除的记录本身（含创建时间等），便于调用方做审计/撤销提示
	res := &RememberResult{Memory: m}
	if n, err := u.vec.DeleteByMemory(ctx, id); err != nil {
		log.Warn("memory: 删除向量失败", "id", id, "err", err)
	} else {
		_ = n
	}
	if ents, rels, err := u.graph.DeleteOrphanNodes(ctx, id+"#"); err != nil {
		res.GraphWarning = err.Error()
	} else {
		res.Entities, res.Relations = ents, rels
	}
	if err := u.repo.Delete(ctx, id); err != nil {
		return nil, fmt.Errorf("memory: 删除记录失败: %w", err)
	}
	return res, nil
}

// PurgeExpired 清理全部已过期记忆（MySQL + 向量 + 图谱），返回清理条数
func (u *UseCase) PurgeExpired(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	expired, err := u.repo.ListExpired(ctx, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range expired {
		_, _ = u.vec.DeleteByMemory(ctx, m.ID)
		if _, _, gerr := u.graph.DeleteOrphanNodes(ctx, m.ID+"#"); gerr != nil {
			log.Warn("memory: 清理图谱孤儿节点失败", "id", m.ID, "err", gerr)
		}
		if err := u.repo.Delete(ctx, m.ID); err != nil {
			return n, fmt.Errorf("memory: 清理 %s 失败: %w", m.ID, err)
		}
		n++
	}
	return n, nil
}

// Stats 记忆规模概览
func (u *UseCase) Stats(ctx context.Context) (map[string]any, error) {
	total, expired, err := u.repo.Count(ctx)
	if err != nil {
		return nil, err
	}
	vecN, _ := u.vec.Count(ctx)
	out := map[string]any{
		"memories_total":   total,
		"memories_expired": expired,
		"vector_entities":  vecN,
	}
	if ents, rels, err := u.graph.Counts(ctx); err == nil {
		out["graph_entities"] = ents
		out["graph_relations"] = rels
	}
	return out, nil
}

// Info 运行参数
func (u *UseCase) Info() map[string]any {
	return map[string]any{
		"embed_model":  u.embed.Model(),
		"embed_dim":    u.embed.Dim(),
		"short_ttl":    u.shortTTL.String(),
		"recall_top_k": u.recallTopK,
		"history_lim":  u.historyLim,
		"graph_on":     u.graphOn,
	}
}

// render 渲染提示词：管理端生效版本优先，异常时回落内置默认。
func (u *UseCase) render(ctx context.Context, key string, vars map[string]string) string {
	if u.prompts != nil {
		s, err := u.prompts.Render(ctx, key, vars)
		if err == nil && strings.TrimSpace(s) != "" {
			return s
		}
		if err != nil {
			log.Warn("[memory] 渲染提示词失败，回落内置默认", "key", key, "err", err)
		}
	}
	if s, ok := prompt.RenderBuiltin(key, vars); ok {
		return s
	}
	return ""
}

// renderHistory 短期历史 -> 文本
func renderHistory(msgs []Message) string {
	if len(msgs) == 0 {
		return "（无历史对话）"
	}
	var sb strings.Builder
	for _, m := range msgs {
		role := m.Role
		if role == "assistant" {
			role = "助手"
		} else if role == "user" {
			role = "用户"
		}
		fmt.Fprintf(&sb, "%s：%s\n", role, m.Content)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderRecall 召回结果 -> 编号化文本上下文
func renderRecall(r *RecallResult) string {
	var sb strings.Builder
	if len(r.Memories) > 0 {
		sb.WriteString("相关长期记忆：\n")
		for i, m := range r.Memories {
			fmt.Fprintf(&sb, "[M%d] (%s, 重要度%d) %s\n", i+1, m.Kind, m.Importance, m.Content)
		}
	}
	if r.Graph != nil && (len(r.Graph.Entities) > 0 || len(r.Graph.Relations) > 0) {
		sb.WriteString("\n记忆图谱：\n实体：\n")
		for i, e := range r.Graph.Entities {
			fmt.Fprintf(&sb, "[E%d] %s（%s）：%s\n", i+1, e.Name, e.Type, e.Description)
		}
		sb.WriteString("关系：\n")
		for i, rel := range r.Graph.Relations {
			fmt.Fprintf(&sb, "[R%d] %s --%s--> %s\n", i+1, rel.Source, rel.Relation, rel.Target)
		}
	}
	if sb.Len() == 0 {
		return "（没有找到相关记忆）"
	}
	return strings.TrimRight(sb.String(), "\n")
}

// entityContent / relationContent 与 graphrag.Service 保持一致的向量化文本格式
func entityContent(e graphrag.Entity) string {
	return fmt.Sprintf("%s %s: %s", e.Name, e.Type, e.Description)
}

func relationContent(r graphrag.Relation) string {
	return fmt.Sprintf("%s --%s--> %s: %s", r.Source, r.Relation, r.Target, r.Description)
}

// normName / mergeRefs 与 graphrag 包一致的归一化与引用合并
func normName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out = append(out, c)
	}
	return string(out)
}

func mergeRefs(a, b []string) []string {
	seen := make(map[string]struct{}, len(a))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	out := a
	for _, s := range b {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}
