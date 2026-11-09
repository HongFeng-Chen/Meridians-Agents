// Package rag 编排完整 RAG 流水线：
// 分块 -> 向量化 -> Milvus 入库 -> 元数据落 MySQL -> Redis 缓存问答结果。
package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go_agent/internal/biz/history"
	"go_agent/internal/biz/prompt"
	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/loader"

	"github.com/go-kratos/kratos/v3/log"
)

// IngestResult 入库结果
type IngestResult struct {
	DocID     string `json:"doc_id"`
	Source    string `json:"source"`
	Mode      string `json:"mode"`
	Chars     int    `json:"chars"`
	Chunks    int    `json:"chunks"`
	Entities  int    `json:"entities"`
	Relations int    `json:"relations"`
	Deduped   bool   `json:"deduped"` // 内容未变，跳过重复向量化
	// Purged 本次清理掉的旧版本行数（分块或图谱节点）：更新文档时写新版本
	// 之后再删旧版本，这个数不为 0 说明确实替换了一份旧内容
	Purged   int           `json:"purged"`
	Duration time.Duration `json:"duration"`
}

// Rag RAG 用例：混合检索问答 + GraphRAG 图谱问答
type Rag struct {
	store    ChunkStore
	graph    *graphrag.Service
	repo     DocumentRepo
	cache    AnswerCache
	locker   Locker
	embed    Embedder
	llm      Chatter
	prompts  Prompter
	chunker  *Chunker
	graphTop int
	// hist 统一操作历史（RAG / GraphRAG 召回记录，可为 nil：不记录）
	hist history.Recorder

	topK       int
	topN       int
	cacheTTL   time.Duration
	embedBatch int
	// rerank 召回精排（可选：conf.Data.rerank 未启用时为 nil，按检索原序返回）
	rerank *Reranker
}

// NewRag 构建 RAG 用例（wire provider）
func NewRag(
	store ChunkStore,
	graphStore graphrag.Store,
	repo DocumentRepo,
	cache AnswerCache,
	locker Locker,
	embed Embedder,
	llm Chatter,
	prompts Prompter,
	hist history.Recorder,
	c *conf.Data,
) (*Rag, error) {
	rc := c.GetRag()
	topK := int(rc.GetTopK())
	if topK <= 0 {
		topK = 10
	}
	topN := int(rc.GetTopN())
	if topN <= 0 {
		topN = 5
	}
	maxChunk := int(rc.GetMaxChunk())
	if maxChunk <= 0 {
		maxChunk = 600
	}
	overlap := int(rc.GetOverlap())
	if overlap <= 0 {
		overlap = 100
	}
	batch := int(rc.GetEmbedBatch())
	if batch <= 0 {
		batch = 32
	}
	ttl := rc.GetCacheTTL().AsDuration()
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	ch := New(maxChunk, overlap)
	r := &Rag{
		store:      store,
		repo:       repo,
		cache:      cache,
		locker:     locker,
		embed:      embed,
		llm:        llm,
		prompts:    prompts,
		chunker:    ch,
		graphTop:   int(rc.GetGraphTopK()),
		hist:       hist,
		topK:       topK,
		topN:       topN,
		cacheTTL:   ttl,
		embedBatch: batch,
		rerank:     NewReranker(c),
	}

	// 启动即确保主集合存在且已加载（残缺集合会自动补索引）
	readyCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := store.EnsureCollection(readyCtx); err != nil {
		return nil, fmt.Errorf("初始化向量集合: %w", err)
	}

	// GraphRAG 复用同一 Milvus 连接、Embedding 与 LLM，仅需注入图集合与取证回调
	gs, err := graphrag.New(context.Background(), graphrag.Deps{
		Store:    graphStore,
		Embedder: embed,
		LLM:      llm,
		Prompts:  prompts,
		Evidence: &evidenceAdapter{store: store},
		Splitter: ch,
		Options: graphrag.Options{
			TopK:         int(rc.GetGraphTopK()),
			Hops:         int(rc.GetGraphHops()),
			MaxEdges:     int(rc.GetGraphMaxEdges()),
			MaxEntities:  int(rc.GetGraphMaxEntities()),
			MaxRelations: int(rc.GetGraphMaxRelations()),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 GraphRAG: %w", err)
	}
	r.graph = gs
	return r, nil
}

// EnsureReady 启动期检查向量集合（不存在则创建，残缺则补索引）
func (r *Rag) EnsureReady(ctx context.Context) error {
	return r.store.EnsureCollection(ctx)
}

// Ingest 入库一篇文本：分块 -> embedding -> Milvus -> MySQL 元数据 -> 清缓存
func (r *Rag) Ingest(ctx context.Context, docID, source, mode, text string) (*IngestResult, error) {
	start := time.Now()
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("文本为空")
	}
	if docID == "" {
		docID = "doc-" + time.Now().Format("20060102150405")
	}
	if source == "" {
		source = docID
	}
	if mode == "" {
		mode = "rag"
	}

	// 同一文档并发入库只允许一个（Redis 锁）
	lockKey := IngestLockKey(docID)
	ok, err := r.locker.TryLock(ctx, lockKey, 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("获取入库锁: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("文档 %s 正在入库中，请稍后重试", docID)
	}
	defer func() { _ = r.locker.Unlock(ctx, lockKey) }()

	sum := sha256.Sum256([]byte(mode + "|" + text))
	checksum := hex.EncodeToString(sum[:])

	// 归属用户：入库时固化，检索时按此过滤（认证未启用时 = default）
	owner := identity.UserID(ctx)

	// 内容未变化则跳过重复向量化（MySQL 元数据判定）
	var prev *Document
	if old, err := r.repo.Get(ctx, docID); err == nil && old != nil {
		prev = old
		// 隔离保护：doc_id 全局唯一，属于别人的同名文档不允许覆盖/复用
		if old.UserID != "" && old.UserID != owner {
			return nil, fmt.Errorf("文档标识 %q 已被其他用户占用，请换一个文件名或 doc_id", docID)
		}
		if old.Checksum == checksum && old.Status == "ready" && old.Mode == mode {
			return &IngestResult{
				DocID: docID, Source: source, Mode: mode, Chars: len([]rune(text)),
				Chunks: old.Chunks, Entities: old.Entities, Relations: old.Relations,
				Deduped: true, Duration: time.Since(start),
			}, nil
		}
	}

	res := &IngestResult{DocID: docID, Source: source, Mode: mode, Chars: len([]rune(text))}
	if mode == "graph" {
		// 图谱入库内部先摘掉本文件的旧引用再写新节点（见 graphrag.Service.Ingest）
		ents, rels, err := r.graph.Ingest(ctx, docID, source, text)
		if err != nil {
			r.saveFailed(ctx, docID, source, mode, text, err)
			return nil, err
		}
		res.Entities, res.Relations = ents, rels
		// 由 rag 改成 graph：旧分块留着会被普通问答召回，删掉
		if prev != nil && prev.Mode == "rag" {
			if n, derr := r.store.DeleteDoc(ctx, docID); derr != nil {
				log.Warn("[rag] 模式切换清理旧分块失败", "doc", docID, "err", derr)
			} else {
				res.Purged = int(n)
			}
		}
	} else {
		// 先记下旧版本主键：Milvus 主键是 autoID，重新入库只会新增行，
		// 不删旧行的话同一 doc 会同时存在多版分块，召回时新旧混着返回
		oldIDs, err := r.store.IDsOfDoc(ctx, docID)
		if err != nil {
			r.saveFailed(ctx, docID, source, mode, text, err)
			return nil, fmt.Errorf("读取旧版本分块: %w", err)
		}
		chunks, err := r.ingestVectors(ctx, docID, source, text)
		if err != nil {
			// 新版本没写成功：旧版本原样保留，检索结果不会因此变差
			r.saveFailed(ctx, docID, source, mode, text, err)
			return nil, err
		}
		res.Chunks = chunks
		if n, derr := r.store.DeleteByIDs(ctx, oldIDs); derr != nil {
			// 新版本已就位。删不掉只是留下重复：下次内容变化时会连同这批
			// ID 一起被记录并清理（IDsOfDoc 返回的是全量），属于自愈型残留
			log.Warn("[rag] 旧版本分块清理失败，下次入库会自动清掉", "doc", docID, "err", derr)
		} else {
			res.Purged = int(n)
		}
		// 由 graph 改成 rag：旧图谱引用留着会出现在图谱问答里，摘掉
		if prev != nil && prev.Mode == "graph" {
			if _, _, gerr := r.graph.DeleteDocGraph(ctx, docID); gerr != nil {
				log.Warn("[rag] 模式切换清理旧图谱失败", "doc", docID, "err", gerr)
			}
		}
	}
	res.Duration = time.Since(start)

	if err := r.repo.Upsert(ctx, &Document{
		DocID: docID, UserID: owner, Source: source, Mode: mode, Status: "ready",
		Chars: res.Chars, Chunks: res.Chunks,
		Entities: res.Entities, Relations: res.Relations,
		Checksum: checksum,
	}); err != nil {
		return nil, fmt.Errorf("写入文档元数据: %w", err)
	}
	// 文档变化后旧答案失效
	_ = r.cache.Invalidate(ctx)
	return res, nil
}

// ingestVectors 分块 + 分批 embedding + 写入 Milvus，返回分块数
func (r *Rag) ingestVectors(ctx context.Context, docID, source, text string) (int, error) {
	chunks := r.chunker.Chunk(text)
	if len(chunks) == 0 {
		return 0, fmt.Errorf("未能切分出任何文本块（文本为空？）")
	}
	for i := 0; i < len(chunks); i += r.embedBatch {
		end := i + r.embedBatch
		if end > len(chunks) {
			end = len(chunks)
		}
		vecs, err := r.embed.Embed(ctx, chunks[i:end])
		if err != nil {
			return 0, fmt.Errorf("embed batch %d-%d: %w", i, end, err)
		}
		if err := r.store.Insert(ctx, docID, source, chunks[i:end], vecs); err != nil {
			return 0, err
		}
	}
	return len(chunks), nil
}

// Format 支持的文件格式（前端用于 accept 与提示）
type Format struct {
	Ext  string `json:"ext"`
	Type string `json:"type"`
}

// Formats 返回支持的文件格式列表
func (r *Rag) Formats() []Format {
	exts := loader.SupportedExts()
	out := make([]Format, 0, len(exts))
	for _, e := range exts {
		out = append(out, Format{Ext: e, Type: loader.Describe("a." + e)})
	}
	return out
}

// MaxUploadSize 单文件解析上限（字节）
func (r *Rag) MaxUploadSize() int64 { return loader.MaxFileSize }

// DescribeFile 返回文件名对应的类型描述
func (r *Rag) DescribeFile(name string) string { return loader.Describe(name) }

// ExtractText 只解析不上库（前端预览/编辑用）
func (r *Rag) ExtractText(name string, rd io.Reader) (string, error) {
	return loader.ExtractReader(name, rd)
}

// IngestFile 解析本地文件后入库
func (r *Rag) IngestFile(ctx context.Context, path, mode string) (*IngestResult, error) {
	text, err := loader.ExtractFile(path)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	docID := strings.TrimSuffix(name, filepath.Ext(name))
	return r.Ingest(ctx, docID, name, mode, text)
}

// IngestReader 解析上传流后入库（HTTP 上传用）
func (r *Rag) IngestReader(ctx context.Context, name, docID, source, mode string, rd io.Reader) (*IngestResult, string, error) {
	text, err := loader.ExtractReader(name, rd)
	if err != nil {
		return nil, "", err
	}
	if docID == "" {
		docID = strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	}
	if source == "" {
		source = filepath.Base(name)
	}
	res, err := r.Ingest(ctx, docID, source, mode, text)
	return res, text, err
}

// saveFailed 记录失败状态（便于前端与管理页面排查）
func (r *Rag) saveFailed(ctx context.Context, docID, source, mode, text string, cause error) {
	_ = r.repo.Upsert(ctx, &Document{
		DocID: docID, UserID: identity.UserID(ctx), Source: source, Mode: mode, Status: "failed",
		Chars: len([]rune(text)),
	})
}

// render 渲染提示词：优先取管理端当前生效版本，失败时回落到内置默认，
// 保证提示词服务（MySQL/Redis）异常时不阻断问答。
func (r *Rag) render(ctx context.Context, key string, vars map[string]string) string {
	if r.prompts != nil {
		s, err := r.prompts.Render(ctx, key, vars)
		if err == nil && strings.TrimSpace(s) != "" {
			return s
		}
		if err != nil {
			log.Warn("[rag] 渲染提示词失败，回落内置默认", "key", key, "err", err)
		}
	}
	if s, ok := prompt.RenderBuiltin(key, vars); ok {
		return s
	}
	return ""
}

// Query 完整 RAG：Redis 缓存 -> 混合检索 -> 提示词 -> LLM 生成
func (r *Rag) Query(ctx context.Context, question, filter string) (*Answer, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("问题为空")
	}
	key := AnswerCacheKey(question, filter)
	if ans, ok := r.cache.GetAnswer(ctx, key); ok {
		ans.Cached = true
		return ans, nil
	}

	hits, err := r.Retrieve(ctx, question, filter)
	if err != nil {
		return nil, err
	}
	if len(hits) > r.topN {
		hits = hits[:r.topN]
	}

	var sb strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&sb, "[%d] (来源: %s)\n%s\n\n", i+1, h.Source, h.Text)
	}
	// 提示词由管理端维护（/api/prompts），改词即时生效，无需重启
	systemPrompt := r.render(ctx, prompt.KeyRAGSystem, nil)
	userPrompt := r.render(ctx, prompt.KeyRAGUser, map[string]string{
		"context":  sb.String(),
		"question": question,
	})

	answer, err := r.llm.Chat(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("生成回答: %w", err)
	}
	ans := &Answer{Question: question, Answer: answer, Citations: hits}
	r.cache.PutAnswer(ctx, key, ans, r.cacheTTL)
	return ans, nil
}

// Retrieve 混合检索（Dense + BM25，RRF 融合）。
// 每次召回异步落一条 RAG 召回历史（op_histories），任务内调用自动关联 task。
func (r *Rag) Retrieve(ctx context.Context, question, filter string) (hits []Hit, err error) {
	start := time.Now()
	defer func() {
		if r.hist == nil {
			return
		}
		rec := &history.Record{
			Kind:      history.KindRagRecall,
			Ref:       filter,
			Query:     question,
			OK:        err == nil,
			LatencyMS: time.Since(start).Milliseconds(),
		}
		if err != nil {
			rec.Summary = err.Error()
		} else {
			summary := fmt.Sprintf("召回 %d 条", len(hits))
			for i, h := range hits {
				if i >= 3 {
					break
				}
				summary += fmt.Sprintf("；%s(%.3f)", h.DocID, h.Score)
			}
			rec.Summary = summary
			rec.Detail = buildRecallDetail(filter, hits)
		}
		r.hist.Record(ctx, rec)
	}()

	qvec, err := r.embed.EmbedOne(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	hits, err = r.store.HybridSearch(ctx, question, qvec, r.topK, filter)
	if err != nil {
		return nil, err
	}
	return r.rerankHits(ctx, question, hits), nil
}

// chunkDocIDField Milvus 分块集合里的文档 ID 字段名（与 milvusdb/models 保持一致）。
// 放在 biz 层是为了让「按用户白名单过滤」的表达式构造不依赖存储实现。
const chunkDocIDField = "doc_id"

// RetrieveForUser 按用户隔离的检索：只召回归属该用户文档的分块。
// 实现方式：MySQL 拿到该用户 doc_id 白名单 → 生成 Milvus 过滤表达式 doc_id in [...]。
// 用户没有文档时直接返回空，省掉一次 embedding 与向量检索。
func (r *Rag) RetrieveForUser(ctx context.Context, question, userID string) ([]Hit, error) {
	filter, empty, err := r.userDocFilter(ctx, userID)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, nil
	}
	return r.Retrieve(ctx, question, filter)
}

// userDocFilter 用户文档白名单过滤表达式；empty=true 表示该用户没有任何文档
func (r *Rag) userDocFilter(ctx context.Context, userID string) (filter string, empty bool, err error) {
	ids, err := r.repo.ListDocIDs(ctx, userID)
	if err != nil {
		return "", false, fmt.Errorf("查询用户文档: %w", err)
	}
	if len(ids) == 0 {
		return "", true, nil
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = strconv.Quote(id)
	}
	return fmt.Sprintf("%s in [%s]", chunkDocIDField, strings.Join(quoted, ",")), false, nil
}

// GraphRecallForUser 按用户隔离的图谱召回：只保留引用了该用户文档的实体与关系。
func (r *Rag) GraphRecallForUser(ctx context.Context, question, userID string) (*graphrag.GraphAnswer, error) {
	ids, err := r.repo.ListDocIDs(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("查询用户文档: %w", err)
	}
	return r.graph.RecallScoped(ctx, question, ids)
}

// GraphQueryForUser 按用户隔离的图谱问答（生成式）。
func (r *Rag) GraphQueryForUser(ctx context.Context, question, userID string) (*graphrag.GraphAnswer, error) {
	ids, err := r.repo.ListDocIDs(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("查询用户文档: %w", err)
	}
	return r.graph.QueryScoped(ctx, question, ids)
}

// DocFilterForUser 用户文档白名单的 Milvus 过滤表达式；empty=true 表示该用户没有文档。
// 供问答接口在调用 Query（带 filter 的生成式检索）前复用同一份隔离条件。
func (r *Rag) DocFilterForUser(ctx context.Context, userID string) (string, bool, error) {
	return r.userDocFilter(ctx, userID)
}

// GetDoc 文档元数据（归属校验用；不存在返回 nil）
func (r *Rag) GetDoc(ctx context.Context, docID string) (*Document, error) {
	return r.repo.Get(ctx, docID)
}

// rerankHits 召回精排：topK 条 → 交叉编码器重排 → 按相关度降序。
// 任何失败静默降级为检索原序（精排是增强，不是依赖）。
func (r *Rag) rerankHits(ctx context.Context, question string, hits []Hit) []Hit {
	if r.rerank == nil || len(hits) <= 1 {
		return hits
	}
	rerankCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	docs := make([]string, len(hits))
	for i, h := range hits {
		docs[i] = h.Text
	}
	order, err := r.rerank.Rerank(rerankCtx, question, docs)
	if err != nil || len(order) == 0 {
		return hits
	}
	out := make([]Hit, 0, len(order))
	for _, idx := range order {
		if idx >= 0 && idx < len(hits) {
			out = append(out, hits[idx])
		}
	}
	return out
}

// GraphQuery 图谱问答（带 Redis 缓存）。
// 每次召回异步落一条 GraphRAG 召回历史（op_histories），任务内调用自动关联 task。
func (r *Rag) GraphQuery(ctx context.Context, question string) (ans *graphrag.GraphAnswer, err error) {
	start := time.Now()
	defer func() {
		if r.hist == nil {
			return
		}
		rec := &history.Record{
			Kind:      history.KindGraphRecall,
			Ref:       "graph",
			Query:     question,
			OK:        err == nil,
			LatencyMS: time.Since(start).Milliseconds(),
		}
		if err != nil {
			rec.Summary = err.Error()
		} else if ans != nil {
			rec.Summary = fmt.Sprintf("实体 %d / 关系 %d / 证据 %d", len(ans.Entities), len(ans.Relations), len(ans.Evidence))
			// 明细落库：命中实体 / 命中关系 / 检索子图证据 / 回答（详情弹层查看）
			rec.Detail = buildGraphDetail(ans)
		}
		r.hist.Record(ctx, rec)
	}()

	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("问题为空")
	}
	key := AnswerCacheKey("graph|"+question, "")
	if cached, ok := r.cache.GetAnswer(ctx, key); ok {
		ans = &graphrag.GraphAnswer{
			Question: cached.Question, Answer: cached.Answer, Cached: true,
		}
		return ans, nil
	}
	ans, err = r.graph.Query(ctx, question)
	if err != nil {
		return nil, err
	}
	// 复用同一缓存通道：图谱答案不存引用，仅缓存文本
	r.cache.PutAnswer(ctx, key, &Answer{Question: ans.Question, Answer: ans.Answer}, r.cacheTTL)
	return ans, nil
}

// GraphStats 图谱规模
func (r *Rag) GraphStats(ctx context.Context) (graphrag.Stats, error) {
	return r.graph.Stats(ctx)
}

// GraphRecall 图谱召回（只检索不生成，无缓存）：实体混合检索 + 关系多跳扩展。
// 供会话场景把图谱上下文注入 LLM 提示词使用。
func (r *Rag) GraphRecall(ctx context.Context, question string) (*graphrag.GraphAnswer, error) {
	return r.graph.Recall(ctx, question)
}

// ListDocs 文档列表（MySQL 权威数据源）；userID 非空时只返回该用户的文档
func (r *Rag) ListDocs(ctx context.Context, userID string, limit int) ([]Document, error) {
	if limit <= 0 {
		limit = 200
	}
	return r.repo.List(ctx, userID, limit)
}

// DeleteDoc 删除文档：Milvus 分块 + 图谱孤儿节点 + MySQL 元数据 + 缓存
func (r *Rag) DeleteDoc(ctx context.Context, docID string) (map[string]any, error) {
	if docID == "" {
		return nil, fmt.Errorf("doc_id 不能为空")
	}
	nChunks, err := r.store.DeleteDoc(ctx, docID)
	if err != nil {
		return nil, fmt.Errorf("删除向量分块: %w", err)
	}
	nEnts, nRels, gerr := r.graph.DeleteDocGraph(ctx, docID)
	if e := r.repo.Delete(ctx, docID); e != nil {
		return nil, fmt.Errorf("删除文档元数据: %w", e)
	}
	_ = r.cache.Invalidate(ctx)

	resp := map[string]any{
		"doc_id":            docID,
		"deleted_chunks":    nChunks,
		"deleted_entities":  nEnts,
		"deleted_relations": nRels,
	}
	if gerr != nil {
		resp["graph_warning"] = gerr.Error()
	}
	return resp, nil
}

// Stats 系统概览（向量规模 + 文档数 + 图谱规模）
func (r *Rag) Stats(ctx context.Context) Stats {
	var st Stats
	if n, err := r.store.Count(ctx); err == nil {
		st.Chunks = n
	}
	if n, err := r.repo.Count(ctx); err == nil {
		st.Docs = n
	}
	if gs, err := r.graph.Stats(ctx); err == nil {
		st.Entities = gs.Entities
		st.Relations = gs.Relations
	}
	return st
}

// Info 运行参数（管理接口展示）
func (r *Rag) Info() map[string]any {
	return map[string]any{
		"embed_model": r.embed.Model(),
		"embed_dim":   r.embed.Dim(),
		"llm_model":   r.llm.Model(),
		"top_k":       r.topK,
		"top_n":       r.topN,
		"cache_ttl":   r.cacheTTL.String(),
		"graph_top_k": r.graphTop,
	}
}

// AnswerCacheKey 问答缓存键：问题 + 过滤条件的 sha256 前 12 字节。
//
// 键的语义属于领域（"什么算同一个问题"），所以由 biz 决定；data 层只负责存取，
// 批量失效时按前缀 SCAN（前缀登记在 internal/data/redis/models.AnswerPrefix）。
// 两侧前缀必须一致，契约由 internal/data/redis 的测试守着。
func AnswerCacheKey(question, filter string) string {
	sum := sha256.Sum256([]byte(question + "\x00" + filter))
	return "rag:ans:" + hex.EncodeToString(sum[:12])
}

// IngestLockKey 文档入库锁键（同一 doc_id 并发导入互斥）。
// 前缀同样登记在 internal/data/redis/models.IngestLockPrefix。
func IngestLockKey(docID string) string {
	return "rag:lock:ingest:" + docID
}

// evidenceAdapter 把 ChunkStore 的过滤查询适配为 graphrag.Evidence
type evidenceAdapter struct {
	store ChunkStore
}

func (e *evidenceAdapter) Texts(ctx context.Context, docID string, idxs []int64) ([]string, error) {
	if len(idxs) == 0 {
		return nil, nil
	}
	list := make([]string, len(idxs))
	for i, v := range idxs {
		list[i] = fmt.Sprintf("%d", v)
	}
	filter := fmt.Sprintf(`doc_id == %s and chunk_idx in [%s]`, quote(docID), strings.Join(list, ","))
	hits, err := e.store.GetTexts(ctx, filter, 10)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Text)
	}
	return out, nil
}

// quote 转义 Milvus 标量过滤表达式中的字符串字面量
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
