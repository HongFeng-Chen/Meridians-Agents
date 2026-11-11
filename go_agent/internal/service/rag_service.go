package service

// RAG / GraphRAG 管理面。
//
// 本文件是 api/rag/v1/rag.proto 的服务端实现：方法签名由 protoc 生成，
// 通过 v1.RegisterRagServiceHTTPServer / RegisterRagServiceGRPCServer 注册，
// 一份实现同时对外提供 REST 与 gRPC 两种调用方式。
//
// 分层约定：这里只做「协议 ↔ 领域模型」的转换与参数校验，解析、分块、
// 检索、图谱抽取等业务全部在 internal/biz/rag。

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	v1 "go_agent/api/rag/v1"
	"go_agent/internal/biz/rag"
	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/httpx"
)

// RagService RAG 管理面：导入、检索问答、图谱、文档管理、文件解析。
type RagService struct {
	v1.UnimplementedRagServiceServer

	uc  *rag.Rag
	cfg *conf.Data
}

// 编译期断言：同一份实现必须同时满足 HTTP 与 gRPC 两套生成接口。
var (
	_ v1.RagServiceHTTPServer = (*RagService)(nil)
	_ v1.RagServiceServer     = (*RagService)(nil)
)

// NewRagService 创建 RAG 服务
func NewRagService(uc *rag.Rag, c *conf.Data) *RagService {
	return &RagService{uc: uc, cfg: c}
}

// Health 服务健康检查与运行参数。
func (s *RagService) Health(_ context.Context, _ *v1.HealthRequest) (*v1.HealthReply, error) {
	return &v1.HealthReply{
		Status: "ok",
		Time:   time.Now().Format(time.RFC3339),
		Rag:    toRagInfo(s.uc.Info()),
	}, nil
}

// Ingest 文本入库：分块 -> 向量化 -> Milvus -> MySQL 元数据 -> 清缓存。
func (s *RagService) Ingest(ctx context.Context, req *v1.IngestRequest) (*v1.IngestResult, error) {
	res, err := s.ingest(ctx, req, req.GetMode())
	if err != nil {
		return nil, err
	}
	return res, nil
}

// GraphIngest 图谱构建：LLM 抽取实体与关系后入图集合（mode 固定 graph）。
func (s *RagService) GraphIngest(ctx context.Context, req *v1.IngestRequest) (*v1.IngestResult, error) {
	return s.ingest(ctx, req, "graph")
}

func (s *RagService) ingest(ctx context.Context, req *v1.IngestRequest, mode string) (*v1.IngestResult, error) {
	if strings.TrimSpace(req.GetText()) == "" {
		return nil, badRequest("请求体无效，需要 text 字段")
	}
	res, err := s.uc.IngestDocument(ctx, rag.IngestInput{
		DocID:  req.GetDocId(),
		Source: req.GetSource(),
		Mode:   mode,
		Text:   req.GetText(),
		Scope:  req.GetScope(),
	})
	if err != nil {
		return nil, ragErr(err)
	}
	return toIngestResult(res), nil
}

// Query 混合检索问答（Dense + BM25，RRF 融合）。
func (s *RagService) Query(ctx context.Context, req *v1.QueryRequest) (*v1.Answer, error) {
	if strings.TrimSpace(req.GetQuestion()) == "" {
		return nil, badRequest("请求体无效，需要 question 字段")
	}
	// 隔离：过滤条件一律由服务端按当前登录用户生成，忽略客户端传入的 filter，
	// 否则客户端可用任意表达式检索他人文档（也是表达式注入面）。
	filter, empty, err := s.uc.DocFilterForUser(ctx, scopeUser(ctx))
	if err != nil {
		return nil, internalErr(err)
	}
	if empty {
		return &v1.Answer{Question: req.GetQuestion(), Answer: "你当前没有已入库的文档，无法回答。", Citations: []*v1.Hit{}}, nil
	}
	ans, err := s.uc.Query(ctx, req.GetQuestion(), filter)
	if err != nil {
		return nil, internalErr(err)
	}
	out := &v1.Answer{Question: ans.Question, Answer: ans.Answer, Cached: ans.Cached}
	out.Citations = make([]*v1.Hit, 0, len(ans.Citations))
	for _, h := range ans.Citations {
		out.Citations = append(out.Citations, toHit(h))
	}
	return out, nil
}

// GraphQuery 图谱问答：实体召回 + 多跳扩展 + 原文证据。
func (s *RagService) GraphQuery(ctx context.Context, req *v1.GraphQueryRequest) (*v1.GraphAnswer, error) {
	if strings.TrimSpace(req.GetQuestion()) == "" {
		return nil, badRequest("请求体无效，需要 question 字段")
	}
	ans, err := s.uc.GraphQueryForUser(ctx, req.GetQuestion(), scopeUser(ctx))
	if err != nil {
		return nil, internalErr(err)
	}
	return toGraphAnswer(ans), nil
}

// GetStats 系统概览。
func (s *RagService) GetStats(ctx context.Context, _ *v1.GetStatsRequest) (*v1.StatsReply, error) {
	st := s.uc.Stats(ctx)
	return &v1.StatsReply{
		MilvusUri:  s.cfg.GetMilvus().GetUri(),
		Collection: s.cfg.GetMilvus().GetCollection(),
		Chunks:     int32(st.Chunks),
		Docs:       int32(st.Docs),
		Entities:   int32(st.Entities),
		Relations:  int32(st.Relations),
		Rag:        toRagInfo(s.uc.Info()),
		Stores: map[string]string{
			"vector": "milvus",
			"meta":   "mysql/rag_documents",
			"cache":  "redis/rag:ans:*",
		},
	}, nil
}

// ListDocs 文档列表。
func (s *RagService) ListDocs(ctx context.Context, req *v1.ListDocsRequest) (*v1.ListDocsReply, error) {
	list, err := s.uc.ListDocs(ctx, req.GetScope(), int(req.GetLimit()))
	if err != nil {
		return nil, internalErr(err)
	}
	out := &v1.ListDocsReply{Docs: make([]*v1.Document, 0, len(list)), Total: int32(len(list))}
	for _, d := range list {
		out.Docs = append(out.Docs, &v1.Document{
			DocId:     d.DocID,
			Source:    d.Source,
			Mode:      d.Mode,
			Status:    d.Status,
			Chars:     int32(d.Chars),
			Chunks:    int32(d.Chunks),
			Entities:  int32(d.Entities),
			Relations: int32(d.Relations),
			Checksum:  d.Checksum,
			Scope:     d.Scope,
			UserId:    d.UserID,
			TenantId:  d.TenantID,
			CreatedAt: d.CreatedAt.Format(time.RFC3339),
			UpdatedAt: d.UpdatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}

// DeleteDoc 删除文档（向量分块 + 图谱孤儿节点 + 元数据 + 缓存）。
func (s *RagService) DeleteDoc(ctx context.Context, req *v1.DeleteDocRequest) (*v1.DeleteDocReply, error) {
	if strings.TrimSpace(req.GetDocId()) == "" {
		return nil, badRequest("doc_id 不能为空")
	}
	doc, err := s.uc.GetDoc(ctx, req.GetDocId())
	if err != nil {
		return nil, internalErr(err)
	}
	if doc == nil {
		return nil, notFound("文档不存在：" + req.GetDocId())
	}
	// 删除权限按文档自身作用域判定：系统知识要平台管理员，租户共享要租户管理员
	// 或贡献者，个人知识仅本人。规则在 biz 层（rag.CanWriteDoc），这里只负责翻译成 403。
	if !rag.CanWriteDoc(ctx, doc) {
		return nil, forbidden("RAG_SCOPE_FORBIDDEN",
			"无权删除该文档（作用域与当前身份的可见范围不一致）")
	}
	res, err := s.uc.DeleteDoc(ctx, req.GetDocId())
	if err != nil {
		return nil, ragErr(err)
	}
	return &v1.DeleteDocReply{
		DocId:            req.GetDocId(),
		DeletedChunks:    int32(anyInt(res["deleted_chunks"])),
		DeletedEntities:  int32(anyInt(res["deleted_entities"])),
		DeletedRelations: int32(anyInt(res["deleted_relations"])),
		GraphWarning:     anyString(res["graph_warning"]),
	}, nil
}

// ListFormats 支持的文件格式清单。
func (s *RagService) ListFormats(_ context.Context, _ *v1.ListFormatsRequest) (*v1.ListFormatsReply, error) {
	formats := s.uc.Formats()
	out := &v1.ListFormatsReply{Formats: make([]*v1.Format, 0, len(formats)), MaxSize: s.uc.MaxUploadSize()}
	for _, f := range formats {
		out.Formats = append(out.Formats, &v1.Format{Ext: f.Ext, Type: f.Type})
	}
	return out, nil
}

// ExtractText 解析文件为纯文本，不入库。
//
// HTTP 走 multipart（文件名与内容来自解码器），gRPC 直接填 file/name。
func (s *RagService) ExtractText(ctx context.Context, req *v1.ExtractTextRequest) (*v1.ExtractTextReply, error) {
	file, err := httpx.ResolveFile(ctx, req.GetFile(), req.GetName())
	if err != nil {
		return nil, err
	}
	text, err := s.uc.ExtractText(file.Filename, bytes.NewReader(file.Data))
	if err != nil {
		return nil, badRequest(err.Error())
	}
	return &v1.ExtractTextReply{
		Name:  file.Filename,
		Type:  s.uc.DescribeFile(file.Filename),
		Chars: int32(len([]rune(text))),
		Text:  text,
	}, nil
}

// UploadAndIngest 上传文件并直接入库。
func (s *RagService) UploadAndIngest(ctx context.Context, req *v1.UploadAndIngestRequest) (*v1.UploadAndIngestReply, error) {
	file, err := httpx.ResolveFile(ctx, req.GetFile(), req.GetName())
	if err != nil {
		return nil, err
	}
	mode := req.GetMode()
	if mode == "" {
		mode = "rag"
	}
	res, _, err := s.uc.IngestReader(ctx, file.Filename, req.GetDocId(), req.GetSource(), mode, req.GetScope(), bytes.NewReader(file.Data))
	if err != nil {
		return nil, ragErr(err)
	}
	out := &v1.UploadAndIngestReply{
		DocId:   res.DocID,
		Source:  res.Source,
		Name:    filepath.Base(file.Filename),
		Chars:   int32(res.Chars),
		Mode:    res.Mode,
		Deduped: res.Deduped,
		Purged:  int32(res.Purged),
	}
	// 双索引会同时返回两套计数：调用方按需展示，不用再猜 mode 是哪一种
	out.Chunks = int32(res.Chunks)
	out.Entities = int32(res.Entities)
	out.Relations = int32(res.Relations)
	return out, nil
}

// ---------- 领域模型 → 协议 ----------

// timeLayout 契约里所有时间字段的格式（与 Go time.Time 的 JSON 输出一致）
const timeLayout = time.RFC3339

func toIngestResult(res *rag.IngestResult) *v1.IngestResult {
	if res == nil {
		return &v1.IngestResult{}
	}
	return &v1.IngestResult{
		DocId:     res.DocID,
		Source:    res.Source,
		Mode:      res.Mode,
		Chars:     int32(res.Chars),
		Chunks:    int32(res.Chunks),
		Entities:  int32(res.Entities),
		Relations: int32(res.Relations),
		Deduped:   res.Deduped,
		Purged:    int32(res.Purged),
		Duration:  int64(res.Duration),
	}
}

func toHit(h rag.Hit) *v1.Hit {
	return &v1.Hit{
		Id:       h.ID,
		DocId:    h.DocID,
		ChunkIdx: int32(h.ChunkIdx),
		Source:   h.Source,
		Text:     h.Text,
		Score:    h.Score,
	}
}

func toGraphAnswer(ans *graphrag.GraphAnswer) *v1.GraphAnswer {
	out := &v1.GraphAnswer{Evidence: ans.Evidence, Cached: ans.Cached, Answer: ans.Answer, Question: ans.Question}
	out.Entities = make([]*v1.Entity, 0, len(ans.Entities))
	for _, e := range ans.Entities {
		out.Entities = append(out.Entities, ToProtoEntity(e))
	}
	out.Relations = make([]*v1.Relation, 0, len(ans.Relations))
	for _, r := range ans.Relations {
		out.Relations = append(out.Relations, ToProtoRelation(r))
	}
	return out
}

// ToProtoEntity 图谱实体（RAG 与记忆共用）
func ToProtoEntity(e graphrag.Entity) *v1.Entity {
	return &v1.Entity{
		Name:        e.Name,
		Type:        e.Type,
		Description: e.Description,
		Freq:        int32(e.Freq),
		ChunkRefs:   e.ChunkRefs,
	}
}

// ToProtoRelation 图谱关系（RAG 与记忆共用）
func ToProtoRelation(r graphrag.Relation) *v1.Relation {
	return &v1.Relation{
		Source:      r.Source,
		Target:      r.Target,
		Relation:    r.Relation,
		Description: r.Description,
		Weight:      r.Weight,
		ChunkRefs:   r.ChunkRefs,
	}
}

// toRagInfo 把 biz 的运行参数 map 转成协议里的结构化快照。
// biz 层返回 map[string]any 是为了兼容历史管理接口，这里做一次显式收口，
// 避免把 map 直接透出到对外契约（gRPC 也需要强类型）。
func toRagInfo(m map[string]any) *v1.RagInfo {
	if m == nil {
		return nil
	}
	return &v1.RagInfo{
		EmbedModel: anyString(m["embed_model"]),
		EmbedDim:   int32(anyInt(m["embed_dim"])),
		LlmModel:   anyString(m["llm_model"]),
		TopK:       int32(anyInt(m["top_k"])),
		TopN:       int32(anyInt(m["top_n"])),
		CacheTtl:   anyString(m["cache_ttl"]),
		GraphTopK:  int32(anyInt(m["graph_top_k"])),
	}
}

// anyInt / anyString 宽容地从 map[string]any 取值（biz 层混用 int / int64）。
func anyInt(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func anyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func anyBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// intPtr proto 的 optional int32 → 领域层指针（nil 表示"不修改"）
func intPtr(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// ragErr RAG 领域错误 → 传输层错误。
//
// 作用域越权是「权限问题」不是「服务器故障」：403 让前端能提示「换个作用域」，
// 500 只会被当成后端崩了。
func ragErr(err error) error {
	switch {
	case errors.Is(err, rag.ErrInvalidScope):
		return badRequest(err.Error())
	case errors.Is(err, rag.ErrScopeForbidden):
		return forbidden("RAG_SCOPE_FORBIDDEN", err.Error())
	default:
		return internalErr(err)
	}
}
