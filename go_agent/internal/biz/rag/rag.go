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
	"go_agent/internal/biz/space"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/loader"
	"go_agent/internal/pkg/principal"
	"go_agent/internal/pkg/trace"

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
	// GraphError 非空 = 双索引部分成功：向量已就绪（Mode 已降级为 rag）、
	// 状态为 ready，仅图谱抽取失败；调用方可稍后用「补图谱」补齐
	GraphError string `json:"graph_error,omitempty"`
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
	// spaces 知识空间解析（V2）。nil 时退化为 V1 三层作用域。
	spaces SpaceScope
	// spaceAccess 空间管理权校验（同实现对象的空间接口视图，nil 表示不支持自定义空间入库）
	spaceAccess SpaceAccess

	topK       int
	topN       int
	cacheTTL   time.Duration
	embedBatch int
	// minRRF RRF 绝对分数线：低于此值的召回直接剔除（无保底）。
	minRRF float64
	// 问答前置开关：范围判定 / 查询改写 / LLM 轻量预审（任一 LLM 失败均降级不阻断）
	scopeCheck bool
	rewrite    bool
	prescreen  bool
	// rerankMin 交叉编码器相关性下限（0~1）：rerank 成功后低于此值的候选剔除
	rerankMin float64
	// rerank 召回精排（可选：conf.Data.rerank 未启用时为 nil，按检索原序返回）
	rerank *Reranker
}

// SpaceScope 知识空间解析（由 biz/space 实现；nil 表示未启用空间体系）。
//
// 只依赖窄接口：RAG 不需要知道空间树怎么建，只需要「这个人能看见哪些空间」。
type SpaceScope interface {
	AllowedSpaceIDs(ctx context.Context, p *principal.Principal) ([]string, error)
}

// SpaceAccess 目标空间的管理权校验（biz/space 实现）。
//
// 文档挂到自定义空间 = 让空间成员可见，属于管理动作；
// 用窄接口而不是直接依赖 space.UseCase，测试 fake 不必实现全量方法。
type SpaceAccess interface {
	CanManage(ctx context.Context, p *principal.Principal, spaceID string) (bool, error)
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
	spaces SpaceScope,
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
	minRRF := rc.GetMinRRF()
	if minRRF <= 0 {
		minRRF = DefaultMinRRF
	}
	rerankMin := c.GetRerank().GetMinScore()
	if rerankMin <= 0 {
		rerankMin = DefaultRerankMin
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
		spaces:     spaces,
		topK:       topK,
		topN:       topN,
		cacheTTL:   ttl,
		embedBatch: batch,
		minRRF:     minRRF,
		scopeCheck: rc.GetScopeCheck(),
		rewrite:    rc.GetRewrite(),
		prescreen:  rc.GetPrescreen(),
		rerankMin:  rerankMin,
		rerank:     NewReranker(c),
	}
	// 同一个实现对象再取一份「管理权校验」的窄接口视图；实现不含该能力时为 nil
	if sa, ok := spaces.(SpaceAccess); ok {
		r.spaceAccess = sa
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
			MinScore:     minRRF, // 种子实体同一 RRF 绝对线，GraphRAG 与 RAG 同理过滤
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

// 索引模式：同一份文档可以只建一种索引，也可以两种同建。
//
// 这是「实时库」的核心：一个库、一份文档，检索侧再按需选 RAG 还是 GraphRAG，
// 不再要求用户把同一篇文档导两遍（导两遍会互相清理，见 IngestDocument 的模式切换）。
const (
	// ModeVector 向量 + BM25 混合检索（原文级召回，秒级入库）
	ModeVector = "rag"
	// ModeGraph 实体关系图谱（跨片段、多跳推理，入库需多次调用 LLM）
	ModeGraph = "graph"
	// ModeBoth 双索引：向量与图谱各写一份，两种检索都能命中
	ModeBoth = "both"
)

// NormalizeMode 归一化索引模式：空值默认向量索引；非法值直接报错，
// 避免拼错的 mode 静默落成 rag——用户以为建了图谱，实际只有向量。
func NormalizeMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeVector:
		return ModeVector, nil
	case ModeGraph:
		return ModeGraph, nil
	case ModeBoth:
		return ModeBoth, nil
	}
	return "", fmt.Errorf("索引模式 %q 不支持：可选 rag（向量）/ graph（图谱）/ both（双索引）", mode)
}

// IngestInput 一次入库的完整参数。
//
// Scope 决定入库后谁能看见："" 等价于 user（个人私有），见 scope.go。
// SpaceID 指定目标知识空间：非空时要求对空间有管理权，文档挂到该空间
// （空间成员按空间授权可见）；留空按 scope 落到对应保留空间。
type IngestInput struct {
	DocID   string
	Source  string
	Mode    string // rag | graph | both
	Text    string
	Scope   string // system | tenant | user
	SpaceID string // 目标知识空间（可选）
}

// Ingest 入库一篇文本（个人作用域的简写，保持既有调用点不变）
func (r *Rag) Ingest(ctx context.Context, docID, source, mode, text string) (*IngestResult, error) {
	return r.IngestDocument(ctx, IngestInput{DocID: docID, Source: source, Mode: mode, Text: text})
}

// IngestDocument 入库一篇文本：分块 -> embedding -> Milvus -> MySQL 元数据 -> 清缓存
func (r *Rag) IngestDocument(ctx context.Context, in IngestInput) (*IngestResult, error) {
	start := time.Now()
	docID, source, mode, text := in.DocID, in.Source, in.Mode, in.Text
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("文本为空")
	}
	// 作用域先归一化再校验权限：非法取值不该以「降级为私有」的方式静默成功
	scope, err := NormalizeScope(in.Scope)
	if err != nil {
		return nil, err
	}
	if !CanWriteScope(ctx, scope) {
		return nil, fmt.Errorf("%w：%s 作用域的知识需要%s", ErrScopeForbidden, scope, scopeHint(scope))
	}
	if docID == "" {
		docID = "doc-" + time.Now().Format("20060102150405")
	}
	if source == "" {
		source = docID
	}
	if mode, err = NormalizeMode(mode); err != nil {
		return nil, err
	}

	// 目标空间解析：显式指定时要求对空间有管理权（挂进空间 = 空间成员可见，
	// 属于管理动作）；未指定按 scope 落到保留空间，行为与历史版本一致。
	spaceID := strings.TrimSpace(in.SpaceID)
	if spaceID != "" {
		if r.spaceAccess == nil {
			return nil, fmt.Errorf("%w：空间体系未启用，不能指定目标空间", ErrScopeForbidden)
		}
		can, aerr := r.spaceAccess.CanManage(ctx, principal.From(ctx), spaceID)
		if aerr != nil {
			return nil, fmt.Errorf("校验目标空间: %w", aerr)
		}
		if !can {
			return nil, fmt.Errorf("%w：对知识空间 %s 无管理权，不能往里放文档", ErrScopeForbidden, spaceID)
		}
	} else {
		spaceID = space.ScopeToSpaceID(scope, identity.TenantID(ctx), identity.UserID(ctx))
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

	// 归属：入库时固化，检索时按 (作用域, 租户, 用户) 过滤。
	// 认证未启用时 = (user, public, default)，与历史数据同一作用域。
	owner := identity.UserID(ctx)
	ownerTenant := identity.TenantID(ctx)

	// 内容未变化则跳过重复向量化（MySQL 元数据判定）
	var prev *Document
	if old, err := r.repo.Get(ctx, docID); err == nil && old != nil {
		prev = old
		// 隔离保护：doc_id 全局唯一，属于别人（或别的作用域）的文档不允许覆盖/复用。
		// 这里按「文档既有作用域」判定而不是按本次请求的作用域，
		// 否则任何人都能把别人的私有文档重灌成自己的 system 知识。
		if !CanWriteDoc(ctx, old) {
			return nil, fmt.Errorf("文档标识 %q 已被其他作用域或用户占用，请换一个文件名或 doc_id", docID)
		}
		if old.Checksum == checksum && old.Status == "ready" && old.Mode == mode && old.Scope == scope && old.SpaceID == spaceID {
			return &IngestResult{
				DocID: docID, Source: source, Mode: mode, Chars: len([]rune(text)),
				Chunks: old.Chunks, Entities: old.Entities, Relations: old.Relations,
				Deduped: true, Duration: time.Since(start),
			}, nil
		}
	}

	res := &IngestResult{DocID: docID, Source: source, Mode: mode, Chars: len([]rune(text))}
	ReportProgress(ctx, "clean", "文档清洗完成（"+fmt.Sprint(len([]rune(text)))+" 字符）")

	// 索引写入顺序：先向量、后图谱。
	// 向量化快、失败面小，先让文档「可检索」；图谱抽取要多次调用 LLM，
	// 放后面即使失败，也不会让一份已经可用的文档白等。
	//
	// 双索引（both）时两个分支都跑，且都不做「模式切换清理」：
	// 另一侧的索引马上会被重写或本来就该保留。
	if mode != ModeGraph {
		// 先记下旧版本主键：Milvus 主键是 autoID，重新入库只会新增行，
		// 不删旧行的话同一 doc 会同时存在多版分块，召回时新旧混着返回
		oldIDs, err := r.store.IDsOfDoc(ctx, docID)
		if err != nil {
			r.saveFailed(ctx, docID, source, mode, scope, spaceID, text, err)
			return nil, fmt.Errorf("读取旧版本分块: %w", err)
		}
		chunks, err := r.ingestVectors(ctx, docID, source, text)
		if err != nil {
			// 新版本没写成功：旧版本原样保留，检索结果不会因此变差
			r.saveFailed(ctx, docID, source, mode, scope, spaceID, text, err)
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
		// 只在「纯向量」模式下清理旧图谱：双索引模式下图谱马上就要重写，
		// 这里删掉再建纯属浪费
		if mode == ModeVector && prev != nil && prev.Mode == ModeGraph {
			if _, _, gerr := r.graph.DeleteDocGraph(ctx, docID); gerr != nil {
				log.Warn("[rag] 模式切换清理旧图谱失败", "doc", docID, "err", gerr)
			}
		}
	}

	if mode != ModeVector {
		ReportProgress(ctx, "graph", "图谱抽取中（LLM 逐块抽取实体与关系）")
		// 图谱入库内部先摘掉本文件的旧引用再写新节点（见 graphrag.Service.Ingest）
		ents, rels, err := r.graph.Ingest(ctx, docID, source, text)
		if err != nil {
			if mode == ModeBoth && res.Chunks > 0 {
				// 部分成功：向量已经写进去了，不能让一次图谱抽取失败把
				// 整篇文档打成「失败」——保留向量索引，文档降级为仅向量
				// 并置为就绪，之后可以随时用「补图谱」把图谱补齐。
				effMode := ModeVector
				sum := sha256.Sum256([]byte(effMode + "|" + text))
				if uerr := r.repo.Upsert(ctx, &Document{
					DocID: docID, UserID: owner, TenantID: ownerTenant, Scope: scope,
					SpaceID: spaceID,
					Source:  source, Mode: effMode, Status: "ready",
					Chars: res.Chars, Chunks: res.Chunks,
					Checksum: hex.EncodeToString(sum[:]),
				}); uerr != nil {
					log.Warn("[rag] 部分成功落库失败", "doc", docID, "err", uerr)
				}
				// 原文落库，保证「补图谱」「补向量」都有原文可用
				if ts, ok := r.repo.(DocumentTextStore); ok {
					_ = ts.SaveText(ctx, docID, text)
				}
				_ = r.cache.Invalidate(ctx)
				res.Mode = effMode
				res.GraphError = err.Error()
				res.Duration = time.Since(start)
				log.Warn("[rag] 双索引部分成功：向量已就绪，图谱抽取失败", "doc", docID, "chunks", res.Chunks, "err", err)
				return res, nil
			}
			r.saveFailed(ctx, docID, source, mode, scope, spaceID, text, err)
			return nil, err
		}
		res.Entities, res.Relations = ents, rels
		// 只在「纯图谱」模式下清理旧分块：双索引模式下分块刚被重写，
		// 删掉会让 RAG 直接查不到这篇文档
		if mode == ModeGraph && prev != nil && prev.Mode != ModeGraph && prev.Mode != "" {
			if n, derr := r.store.DeleteDoc(ctx, docID); derr != nil {
				log.Warn("[rag] 模式切换清理旧分块失败", "doc", docID, "err", derr)
			} else {
				res.Purged = int(n)
			}
		}
	}
	res.Duration = time.Since(start)

	if err := r.repo.Upsert(ctx, &Document{
		DocID: docID, UserID: owner, TenantID: ownerTenant, Scope: scope,
		SpaceID: spaceID,
		Source:  source, Mode: mode, Status: "ready",
		Chars: res.Chars, Chunks: res.Chunks,
		Entities: res.Entities, Relations: res.Relations,
		Checksum: checksum,
	}); err != nil {
		return nil, fmt.Errorf("写入文档元数据: %w", err)
	}
	// 原文落库（可选能力）：「同步索引」把仅图谱文档补建成双索引时，
	// 需要一份原文重放分块；失败只留警告——不影响本次入库结果。
	if ts, ok := r.repo.(DocumentTextStore); ok {
		if terr := ts.SaveText(ctx, docID, text); terr != nil {
			log.Warn("[rag] 保存文档原文失败，该文档之后无法补建向量索引", "doc", docID, "err", terr)
		}
	}
	// 文档变化后旧答案失效
	_ = r.cache.Invalidate(ctx)
	return res, nil
}

// ingestVectors 分块 + 分批 embedding + 写入 Milvus，返回分块数
func (r *Rag) ingestVectors(ctx context.Context, docID, source, text string) (int, error) {
	ReportProgress(ctx, "chunk", "拆分 chunk 中")
	chunks := r.chunker.Chunk(text)
	if len(chunks) == 0 {
		return 0, fmt.Errorf("未能切分出任何文本块（文本为空？）")
	}
	batches := (len(chunks) + r.embedBatch - 1) / r.embedBatch
	ReportProgress(ctx, "chunk", fmt.Sprintf("拆分完成：%d 块（%d 批）", len(chunks), batches))
	for i := 0; i < len(chunks); i += r.embedBatch {
		end := i + r.embedBatch
		if end > len(chunks) {
			end = len(chunks)
		}
		batchNo := i/r.embedBatch + 1
		ReportProgress(ctx, "embed", fmt.Sprintf("向量化中 %d/%d 批（块 %d-%d）", batchNo, batches, i+1, end))
		vecs, err := r.embed.Embed(ctx, chunks[i:end])
		if err != nil {
			return 0, fmt.Errorf("embed batch %d-%d: %w", i, end, err)
		}
		ReportProgress(ctx, "milvus", fmt.Sprintf("写入 Milvus %d/%d 批", batchNo, batches))
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

// IngestFile 解析本地文件后入库（个人作用域）
func (r *Rag) IngestFile(ctx context.Context, path, mode string) (*IngestResult, error) {
	text, err := loader.ExtractFile(path)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	docID := strings.TrimSuffix(name, filepath.Ext(name))
	return r.IngestDocument(ctx, IngestInput{DocID: docID, Source: name, Mode: mode, Text: text})
}

// IngestReader 解析上传流后入库（HTTP 上传用）。
// scope 决定这批知识的可见范围（system / tenant / user，空值 = user）；
// spaceID 指定目标知识空间（可选，要求对空间有管理权）。
func (r *Rag) IngestReader(ctx context.Context, name, docID, source, mode, scope, spaceID string, rd io.Reader) (*IngestResult, string, error) {
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
	res, err := r.IngestDocument(ctx, IngestInput{DocID: docID, Source: source, Mode: mode, Text: text, Scope: scope, SpaceID: spaceID})
	return res, text, err
}

// saveFailed 记录失败状态（便于前端与管理页面排查）。
// spaceID 传目标空间：失败文档也留在原目标位置，树里能看到失败行。
func (r *Rag) saveFailed(ctx context.Context, docID, source, mode, scope, spaceID, text string, cause error) {
	_ = r.repo.Upsert(ctx, &Document{
		DocID: docID, UserID: identity.UserID(ctx), TenantID: identity.TenantID(ctx),
		Scope: scope, SpaceID: spaceID,
		Source: source, Mode: mode, Status: "failed",
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

// Query 完整 RAG 问答管线：
//
//	用户 Query
//	  ↓ ScopeCheck（LLM 判定是否属于知识库范围，out_scope 直接兜底）
//	  ↓ Rewrite（LLM 生成 1 条改写 query，失败降级用原问题）
//	  ↓ 并行召回：向量 topK + BM25 topK，RRF(K=60) 融合
//	  ↓ 过滤 RRF < minRRF（默认 0.0145，无保底）
//	  ↓ LLM 轻量预审（一次调用批量剔除可疑候选）
//	  ↓ Rerank 交叉打分写回 Score，过滤 < rerankMin（默认 0.45）
//	  ↓ 剩余候选作为上下文送入 LLM
//	  ↓ 候选为空 → 直接输出「知识库暂无相关资料」，不硬答
func (r *Rag) Query(ctx context.Context, question, filter string) (ans *Answer, err error) {
	// 外层 span 把「前置 / 检索 / 生成」几段括在一起：
	// 只看 rag.retrieve 无法回答「端到端慢在检索还是生成」。
	ctx, span := trace.Start(ctx, "rag.query",
		trace.String(trace.AttrKind, trace.KindRAG))
	defer func() { span.End(err) }()

	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("问题为空")
	}
	key := AnswerCacheKey(question, filter)
	if cached, ok := r.cache.GetAnswer(ctx, key); ok {
		span.Set("rag.cached", true)
		cached.Cached = true
		return cached, nil
	}

	// —— 前置 1：范围判定 ——
	// 判定失败（LLM 异常/超时）按 in_scope 处理：宁可多检索一次，不误伤。
	if r.scopeCheck {
		if in, decided := r.checkScope(ctx, question); decided && !in {
			span.Set("rag.out_scope", true)
			ans = &Answer{Question: question, Answer: NoContextAnswer, NoContext: true}
			r.cache.PutAnswer(ctx, key, ans, r.cacheTTL)
			return ans, nil
		}
	}

	// —— 前置 2：查询改写 ——
	recallQuery := question
	if r.rewrite {
		if rw := r.rewriteQuery(ctx, question); rw != "" && rw != question {
			span.Set("rag.rewrite", rw)
			recallQuery = rw
		}
	}

	// —— 召回（元数据过滤表达式在 HybridSearch 内下推到 Milvus）——
	hits, err := r.recall(ctx, recallQuery, filter)
	if err != nil {
		return nil, err
	}
	span.Set(trace.AttrRAGHits, len(hits))

	// —— RRF 绝对分数线：低于线的候选剔除（无保底，滤空即空）——
	kept := FilterAbsScore(hits, r.minRRF)
	filteredOut := len(hits) - len(kept)
	if filteredOut > 0 {
		span.Set("rag.rrf_filtered_out", filteredOut)
	}
	hits = kept

	// —— LLM 轻量预审：批量剔除可疑候选 ——
	if r.prescreen && len(hits) > 0 {
		kept, dropped := r.prescreenHits(ctx, question, hits)
		if dropped > 0 {
			span.Set("rag.prescreen_dropped", dropped)
			filteredOut += dropped
		}
		hits = kept
	}

	// —— Rerank 交叉打分 + 相关性下限 ——
	// rerank 成功时 Score 已被写回为 0~1 语义相关性分，可按绝对阈值过滤；
	// 未启用/失败时保持 RRF 原序与原分，不做 0.45 过滤（刻度不同）。
	hits, reranked := r.rerankHits(ctx, recallQuery, hits)
	if reranked && len(hits) > 0 {
		kept := FilterAbsScore(hits, r.rerankMin)
		if dropped := len(hits) - len(kept); dropped > 0 {
			span.Set("rag.rerank_filtered_out", dropped)
			filteredOut += dropped
		}
		hits = kept
	}
	if len(hits) > r.topN {
		hits = hits[:r.topN]
	}

	// —— 空候选兜底：不硬答 ——
	if len(hits) == 0 {
		span.Set("rag.no_context", true)
		ans = &Answer{Question: question, Answer: NoContextAnswer, FilteredOut: filteredOut, NoContext: true}
		r.cache.PutAnswer(ctx, key, ans, r.cacheTTL)
		return ans, nil
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
	ans = &Answer{Question: question, Answer: answer, Citations: hits, FilteredOut: filteredOut}
	r.cache.PutAnswer(ctx, key, ans, r.cacheTTL)
	return ans, nil
}

// checkScope 范围判定：LLM 判断问题是否属于知识库检索范围。
// 返回 (是否在范围内, 是否得出结论)；LLM 失败时 decided=false，由调用方降级放行。
func (r *Rag) checkScope(ctx context.Context, question string) (inScope bool, decided bool) {
	sys := r.render(ctx, prompt.KeyRAGScope, nil)
	if sys == "" {
		return true, false
	}
	out, err := r.llm.ChatTemp(ctx, sys, question, 0)
	if err != nil {
		log.Warn("[rag] 范围判定失败，降级放行", "err", err)
		return true, false
	}
	out = strings.ToLower(strings.TrimSpace(out))
	decided = true
	inScope = !strings.Contains(out, "out_scope") && !strings.Contains(out, "outscope")
	return inScope, decided
}

// rewriteQuery 查询改写：生成 1 条更适合检索的 query。失败返回空串（用原问题）。
func (r *Rag) rewriteQuery(ctx context.Context, question string) string {
	sys := r.render(ctx, prompt.KeyRAGRewrite, nil)
	if sys == "" {
		return ""
	}
	out, err := r.llm.ChatTemp(ctx, sys, question, 0.2)
	if err != nil {
		log.Warn("[rag] 查询改写失败，用原问题", "err", err)
		return ""
	}
	out = strings.TrimSpace(out)
	// 改写结果兜底约束：太长/为空/与原问题相同都退回原问题
	if out == "" || len([]rune(out)) > 120 || out == question {
		return ""
	}
	return out
}

// prescreenHits LLM 轻量预审：一次调用批量判断候选是否可疑。
// 返回 (保留的命中, 剔除数)；LLM 失败或解析异常时原样保留（fail-open）。
func (r *Rag) prescreenHits(ctx context.Context, question string, hits []Hit) ([]Hit, int) {
	sys := r.render(ctx, prompt.KeyRAGPrescreen, nil)
	if sys == "" || r.llm == nil {
		return hits, 0
	}
	var sb strings.Builder
	const preview = 240 // 每条候选只带前 240 字，控制预审 token 成本
	for i, h := range hits {
		text := h.Text
		if len([]rune(text)) > preview {
			text = string([]rune(text)[:preview]) + "…"
		}
		fmt.Fprintf(&sb, "%d. %s\n", i+1, strings.ReplaceAll(text, "\n", " "))
	}
	user := fmt.Sprintf("问题：%s\n\n候选片段：\n%s", question, sb.String())
	out, err := r.llm.ChatTemp(ctx, sys, user, 0)
	if err != nil {
		log.Warn("[rag] 预审失败，全部保留", "err", err)
		return hits, 0
	}
	// 解析：期望每行「<编号> keep|drop」；解析不了的条目默认保留
	drops := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.ToLower(line))
		if !strings.Contains(line, "drop") {
			continue
		}
		if n := leadingInt(line); n >= 1 && n <= len(hits) {
			drops[n-1] = true
		}
	}
	if len(drops) == 0 || len(drops) == len(hits) {
		// 全保留 / 全剔除（可疑：模型误判整批）都原样返回，交给 rerank 阈值兜底
		return hits, 0
	}
	kept := make([]Hit, 0, len(hits)-len(drops))
	for i, h := range hits {
		if !drops[i] {
			kept = append(kept, h)
		}
	}
	return kept, len(drops)
}

// leadingInt 提取字符串开头（跳过非数字前缀）的第一个整数；无数字返回 0。
func leadingInt(s string) int {
	start := -1
	for i, c := range s {
		if c >= '0' && c <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return 0
	}
	n := 0
	for _, c := range s[start:] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			break
		}
	}
	return n
}
// DefaultMinRRF RRF 绝对分数线默认值（K=60）：满分 2/(60+1)≈0.0328（双通道双第一），
// 0.0145 ≈ 「至少在单通道排进前 9 名，或同时出现在双通道」。
const DefaultMinRRF = 0.0145

// DefaultRerankMin 交叉编码器相关性下限默认值（0~1 语义相关性分数，与 RRF 排名分不同刻度）。
const DefaultRerankMin = 0.45

// NoContextAnswer 候选全被过滤时的兜底话术：宁可不答，不让模型拿低相关文本硬编。
const NoContextAnswer = "知识库暂无相关资料"

// FilterAbsScore RRF 绝对分数线过滤：剔除 Score < min 的命中，无保底。
// 候选全被滤空是有效信号（本次召回整体不相关），由问答管线兜底输出 NoContextAnswer。
// min ≤ 0 表示不过滤（原样返回）。
func FilterAbsScore(hits []Hit, min float64) []Hit {
	if len(hits) == 0 || min <= 0 {
		return hits
	}
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if h.Score >= float32(min) {
			out = append(out, h)
		}
	}
	return out
}

// Retrieve 混合检索（Dense + BM25，RRF 融合）。
// 每次召回异步落一条 RAG 召回历史（op_histories），任务内调用自动关联 task。
func (r *Rag) Retrieve(ctx context.Context, question, filter string) (hits []Hit, err error) {
	start := time.Now()
	// 检索 span：embedding + 向量检索 + rerank 三段都在里面，
	// 与 rag.query 的差值是「生成」的耗时。
	ctx, span := trace.Start(ctx, "rag.retrieve",
		trace.String(trace.AttrKind, trace.KindRAG))
	defer func() {
		span.Set(trace.AttrRAGHits, len(hits))
		span.End(err)
	}()
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
	hits, reranked := r.rerankHits(ctx, question, hits)
	if reranked && len(hits) > 0 {
		// rerank 成功：Score 已是 0~1 相关性分，按绝对下限过滤
		hits = FilterAbsScore(hits, r.rerankMin)
	}
	return hits, nil
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
	ids, err := r.visibleDocIDs(ctx, userID)
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
	ids, err := r.visibleDocIDs(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("查询用户文档: %w", err)
	}
	return r.graph.RecallScoped(ctx, question, ids)
}

// GraphQueryForUser 按用户隔离的图谱问答（生成式）。
func (r *Rag) GraphQueryForUser(ctx context.Context, question, userID string) (*graphrag.GraphAnswer, error) {
	ids, err := r.visibleDocIDs(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("查询用户文档: %w", err)
	}
	return r.graph.QueryScoped(ctx, question, ids)
}

// visibleDocIDs 当前身份可见的文档白名单（检索与图谱召回共用）。
//
// V2 空间授权优先：先由空间服务算出可见空间集合，再让仓储按 space_id 过滤；
// 空间服务不可用（未启用 / 查询失败）时退回 V1 三层作用域——检索可以少一点，
// 但不能因为空间服务抖动而变成「全部可见」。这个降级方向是刻意选的。
func (r *Rag) visibleDocIDs(ctx context.Context, userID string) ([]string, error) {
	tenantID := identity.TenantID(ctx)
	if r.spaces != nil {
		ids, err := r.spaces.AllowedSpaceIDs(ctx, principal.From(ctx))
		if err == nil && len(ids) > 0 {
			return r.repo.ListDocIDsBySpaces(ctx, tenantID, userID, ids)
		}
		if err != nil {
			log.Warn("[rag] 解析知识空间失败，退回三层作用域", "err", err)
		}
	}
	return r.repo.ListDocIDs(ctx, tenantID, userID)
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

// recall 纯召回（不含 rerank）：embed 问题 → Dense + BM25 混合检索。
// 问答管线的第一段；外部直接调用的 Retrieve 在其上叠加精排。
func (r *Rag) recall(ctx context.Context, question, filter string) ([]Hit, error) {
	qvec, err := r.embed.EmbedOne(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	return r.store.HybridSearch(ctx, question, qvec, r.topK, filter)
}

// rerankHits 召回精排：交叉编码器重排，并把 0~1 语义相关性分写回 Hit.Score。
// 成功时返回 (重排后的命中, true)；任何失败静默降级为检索原序（精排是增强，不是依赖）。
// 注意：成功后 Score 的刻度从 RRF 排名分变为相关性分，调用方据此选择过滤阈值。
func (r *Rag) rerankHits(ctx context.Context, question string, hits []Hit) ([]Hit, bool) {
	if r.rerank == nil || len(hits) <= 1 {
		return hits, false
	}
	rerankCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	docs := make([]string, len(hits))
	for i, h := range hits {
		docs[i] = h.Text
	}
	order, scores, err := r.rerank.Rerank(rerankCtx, question, docs)
	if err != nil || len(order) == 0 {
		return hits, false
	}
	out := make([]Hit, 0, len(order))
	for _, idx := range order {
		if idx >= 0 && idx < len(hits) {
			h := hits[idx]
			if idx < len(scores) && scores[idx] > 0 {
				h.Score = float32(scores[idx]) // 写回相关性分，替换 RRF 排名分
			}
			out = append(out, h)
		}
	}
	return out, true
}

// GraphQuery 图谱问答（带 Redis 缓存）。
// 每次召回异步落一条 GraphRAG 召回历史（op_histories），任务内调用自动关联 task。
func (r *Rag) GraphQuery(ctx context.Context, question string) (ans *graphrag.GraphAnswer, err error) {
	start := time.Now()
	// 图谱检索单独一类 span：它比向量检索贵一个数量级（多跳 + 证据回查），
	// 混在 rag.retrieve 里会让「图查询慢」和「向量召回慢」分不开。
	ctx, span := trace.Start(ctx, "graphrag.query",
		trace.String(trace.AttrKind, trace.KindGraphRAG))
	defer func() {
		if ans != nil {
			span.Set(trace.AttrGraphNodes, len(ans.Entities))
		}
		span.End(err)
	}()
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

// ListDocs 文档列表（MySQL 权威数据源）：按「系统 ∪ 本租户共享 ∪ 本人私有」可见性过滤，
// scopeFilter 非空时再按作用域收窄（前端「只看系统知识 / 只看我的」）。
func (r *Rag) ListDocs(ctx context.Context, scopeFilter string, limit int) ([]Document, error) {
	if limit <= 0 {
		limit = 200
	}
	return r.repo.List(ctx, identity.TenantID(ctx), identity.UserID(ctx), scopeFilter, limit)
}

// ListDocsInSpace 知识空间内文档清单（知识空间树按节点查看）。
// 管理权或可见权任一通过即可查看；空间体系未启用时报错。
func (r *Rag) ListDocsInSpace(ctx context.Context, spaceID string, limit int) ([]Document, error) {
	sid := strings.TrimSpace(spaceID)
	if sid == "" {
		return nil, fmt.Errorf("space_id 不能为空")
	}
	if err := r.checkSpaceReadable(ctx, sid); err != nil {
		return nil, err
	}
	return r.repo.ListBySpace(ctx, sid, limit)
}

// MoveDoc 把文档移动到目标知识空间（知识空间整理；Confluence 式页面移动）。
//
// 权限双闸：文档自身要可写（CanWriteDoc，防止搬走别人的东西），
// 目标空间要可管理（挂进去 = 空间成员可见，属于授权扩散动作）。
func (r *Rag) MoveDoc(ctx context.Context, docID, targetSpaceID string) (*Document, error) {
	docID = strings.TrimSpace(docID)
	target := strings.TrimSpace(targetSpaceID)
	if docID == "" || target == "" {
		return nil, fmt.Errorf("doc_id 与 space_id 均不能为空")
	}
	if r.spaceAccess == nil {
		return nil, fmt.Errorf("%w：空间体系未启用，不能移动文档", ErrScopeForbidden)
	}
	doc, err := r.repo.Get(ctx, docID)
	if err != nil {
		return nil, fmt.Errorf("读取文档归属: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("文档 %s 不存在", docID)
	}
	if !CanWriteDoc(ctx, doc) {
		return nil, fmt.Errorf("%w：%s 属于 %s 作用域，当前身份无权移动", ErrScopeForbidden, docID, scopeName(doc.Scope))
	}
	can, aerr := r.spaceAccess.CanManage(ctx, principal.From(ctx), target)
	if aerr != nil {
		return nil, fmt.Errorf("校验目标空间: %w", aerr)
	}
	if !can {
		return nil, fmt.Errorf("%w：对知识空间 %s 无管理权，不能把文档移进去", ErrScopeForbidden, target)
	}
	if doc.Scope == ScopeSystem && doc.SpaceID != target {
		// 系统知识挂进普通空间会让空间成员看到平台级知识，至少要求目标空间
		// 的管理权（上面已查）——这里只留提示，不做额外阻断。
		log.Warn("[rag] 系统知识被移动到自定义空间", "doc", docID, "space", target)
	}
	if err := r.repo.UpdateSpaceID(ctx, docID, target); err != nil {
		return nil, fmt.Errorf("更新文档空间: %w", err)
	}
	// 可见性变了，旧答案缓存全部失效
	_ = r.cache.Invalidate(ctx)
	doc.SpaceID = target
	return doc, nil
}

// checkSpaceReadable 查看空间文档的准入：可见（AllowedSpaceIDs 命中）或可管理。
func (r *Rag) checkSpaceReadable(ctx context.Context, spaceID string) error {
	if r.spaceAccess == nil {
		return fmt.Errorf("%w：空间体系未启用", ErrScopeForbidden)
	}
	if can, err := r.spaceAccess.CanManage(ctx, principal.From(ctx), spaceID); err == nil && can {
		return nil
	}
	if r.spaces != nil {
		ids, err := r.spaces.AllowedSpaceIDs(ctx, principal.From(ctx))
		if err == nil {
			for _, id := range ids {
				if id == spaceID {
					return nil
				}
			}
			return fmt.Errorf("%w：知识空间 %s 不在你的可见范围内", ErrScopeForbidden, spaceID)
		}
	}
	return fmt.Errorf("%w：知识空间 %s 不可见", ErrScopeForbidden, spaceID)
}

// sameTenant 归属租户是否一致；历史数据（空租户）视为与默认租户同域，
// 避免升级后老文档突然「不属于任何人」而被拒绝覆盖
func sameTenant(docTenant, current string) bool {
	if docTenant == "" {
		docTenant = identity.DefaultTenant
	}
	if current == "" {
		current = identity.DefaultTenant
	}
	return docTenant == current
}

// DeleteDoc 删除文档：Milvus 分块 + 图谱引用摘除 + MySQL 元数据 + 缓存
//
// 归属校验放在这里而不是只放在 HTTP 层：任务 / Agent 工具等其他入口也走这个方法，
// 校验跟着用例走才不会被新入口绕过。规则按文档自身作用域判定（见 CanWriteDoc）。
func (r *Rag) DeleteDoc(ctx context.Context, docID string) (map[string]any, error) {
	if docID == "" {
		return nil, fmt.Errorf("doc_id 不能为空")
	}
	if doc, err := r.repo.Get(ctx, docID); err != nil {
		return nil, fmt.Errorf("读取文档归属: %w", err)
	} else if doc != nil && !CanWriteDoc(ctx, doc) {
		return nil, fmt.Errorf("%w：%s 属于 %s 作用域，当前身份无权删除", ErrScopeForbidden, docID, scopeName(doc.Scope))
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
