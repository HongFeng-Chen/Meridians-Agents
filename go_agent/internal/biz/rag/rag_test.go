package rag

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go_agent/internal/biz/prompt"
	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/conf"
)

// ---------- fakes ----------

type fakeStore struct {
	mu         sync.Mutex
	chunks     []string
	inserts    int
	lastFilter string // 最近一次 HybridSearch 收到的过滤表达式（隔离断言用）
	// 主键模拟：Milvus 是 autoID，重复入库只会新增行，旧行要显式删
	ids        []int64
	nextID     int64
	deleted    []int64
	chunkDoc   string // 最近一次入库的 docID（假存储只服务单文档测试）
	failInsert bool   // 模拟写入失败（验证「新版本没写成，旧版本不动」）
}

func (f *fakeStore) EnsureCollection(ctx context.Context) error { return nil }
func (f *fakeStore) Insert(ctx context.Context, docID, source string, chunks []string, vecs [][]float32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInsert {
		return errors.New("milvus 不可用")
	}
	f.chunks = append(f.chunks, chunks...)
	f.chunkDoc = docID
	for range chunks {
		f.ids = append(f.ids, f.nextID)
		f.nextID++
	}
	f.inserts++
	return nil
}
func (f *fakeStore) HybridSearch(ctx context.Context, q string, vec []float32, topK int, filter string) ([]Hit, error) {
	f.mu.Lock()
	f.lastFilter = filter
	f.mu.Unlock()
	return []Hit{{DocID: "d1", Source: "a.txt", Text: "命中片段", Score: 0.9}}, nil
}
func (f *fakeStore) GetTexts(ctx context.Context, filter string, limit int) ([]Hit, error) {
	return nil, nil
}
func (f *fakeStore) Count(ctx context.Context) (int64, error) { return int64(len(f.chunks)), nil }
func (f *fakeStore) IDsOfDoc(ctx context.Context, docID string) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.ids))
	copy(out, f.ids)
	return out, nil
}

func (f *fakeStore) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, ids...)
	return int64(len(ids)), nil
}

func (f *fakeStore) DeleteDoc(ctx context.Context, docID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := int64(len(f.chunks))
	f.chunks = nil
	return n, nil
}

type fakeGraphStore struct{}

func (fakeGraphStore) EnsureCollections(ctx context.Context) error { return nil }
func (fakeGraphStore) InsertEntities(ctx context.Context, ents []graphrag.Entity, contents []string, vectors [][]float32) error {
	return nil
}
func (fakeGraphStore) InsertRelations(ctx context.Context, rels []graphrag.Relation, vectors [][]float32) error {
	return nil
}
func (fakeGraphStore) ExistingEntities(ctx context.Context, names []string) (map[string]graphrag.Entity, error) {
	return map[string]graphrag.Entity{}, nil
}
func (fakeGraphStore) SearchEntities(ctx context.Context, question string, qvec []float32, topK int) ([]graphrag.Entity, error) {
	return []graphrag.Entity{{Name: "Milvus", Type: "技术", Description: "向量库", Freq: 1}}, nil
}
func (fakeGraphStore) RelationsOf(ctx context.Context, names []string, limit int) ([]graphrag.Relation, error) {
	return nil, nil
}
func (fakeGraphStore) EntitiesByNames(ctx context.Context, names []string) ([]graphrag.Entity, error) {
	return nil, nil
}
func (fakeGraphStore) Counts(ctx context.Context) (int64, int64, error) { return 1, 0, nil }
func (fakeGraphStore) DetachDocRefs(ctx context.Context, prefix string) (int, int, error) {
	return 0, 0, nil
}

type fakeRepo struct {
	mu   sync.Mutex
	docs map[string]*Document
}

func newFakeRepo() *fakeRepo { return &fakeRepo{docs: make(map[string]*Document)} }

func (f *fakeRepo) Upsert(ctx context.Context, doc *Document) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *doc
	cp.UpdatedAt = time.Now()
	if old, ok := f.docs[doc.DocID]; ok {
		cp.CreatedAt = old.CreatedAt
	} else {
		cp.CreatedAt = cp.UpdatedAt
	}
	f.docs[doc.DocID] = &cp
	return nil
}
func (f *fakeRepo) Get(ctx context.Context, docID string) (*Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.docs[docID]; ok {
		cp := *d
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeRepo) List(ctx context.Context, tenantID, userID string, limit int) ([]Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Document, 0, len(f.docs))
	for _, d := range f.docs {
		if !visibleTo(d, tenantID, userID) {
			continue
		}
		out = append(out, *d)
	}
	return out, nil
}

// ListDocIDs 该租户该用户文档 ID 白名单（检索隔离用）
func (f *fakeRepo) ListDocIDs(ctx context.Context, tenantID, userID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.docs))
	for _, d := range f.docs {
		if !visibleTo(d, tenantID, userID) {
			continue
		}
		out = append(out, d.DocID)
	}
	return out, nil
}

// visibleTo 模拟仓储的 (tenant, user) 双重过滤；空归属视为同域（历史数据）
func visibleTo(d *Document, tenantID, userID string) bool {
	dt, du := d.TenantID, d.UserID
	if dt == "" {
		dt = "public"
	}
	if tenantID != "" && dt != tenantID {
		return false
	}
	if userID != "" && du != "" && du != userID {
		return false
	}
	return true
}
func (f *fakeRepo) Delete(ctx context.Context, docID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.docs, docID)
	return nil
}
func (f *fakeRepo) Count(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.docs)), nil
}

type fakeCache struct {
	mu         sync.Mutex
	answers    map[string]*Answer
	invalidSet int
	locks      map[string]bool
}

func newFakeCache() *fakeCache {
	return &fakeCache{answers: make(map[string]*Answer), locks: make(map[string]bool)}
}

func (f *fakeCache) GetAnswer(ctx context.Context, key string) (*Answer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.answers[key]
	return a, ok
}
func (f *fakeCache) PutAnswer(ctx context.Context, key string, ans *Answer, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[key] = ans
}
func (f *fakeCache) Invalidate(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = make(map[string]*Answer)
	f.invalidSet++
	return nil
}
func (f *fakeCache) TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.locks[key] {
		return false, nil
	}
	f.locks[key] = true
	return true, nil
}
func (f *fakeCache) Unlock(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.locks, key)
	return nil
}

type fakeEmbedder struct{}

func (fakeEmbedder) Dim() int      { return 8 }
func (fakeEmbedder) Model() string { return "fake-embed" }
func (fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 8)
		v[0] = float32(i + 1)
		out[i] = v
	}
	return out, nil
}
func (fakeEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	return make([]float32, 8), nil
}

type fakeLLM struct {
	mu         sync.Mutex
	calls      int
	lastSystem string
	lastUser   string
}

func (f *fakeLLM) Chat(ctx context.Context, system, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastSystem = system
	f.lastUser = user
	return "基于资料的答案", nil
}
func (f *fakeLLM) ChatTemp(ctx context.Context, system, user string, temp float64) (string, error) {
	return `{"entities":[{"name":"Milvus","type":"技术","description":"向量库"}],"relations":[]}`, nil
}
func (f *fakeLLM) Model() string { return "fake-llm" }

// fakePrompter 模拟管理端提示词：按 key 返回自定义内容并记录调用
type fakePrompter struct {
	mu   sync.Mutex
	tpl  map[string]string
	seen []string
}

func (f *fakePrompter) Render(ctx context.Context, key string, vars map[string]string) (string, error) {
	f.mu.Lock()
	f.seen = append(f.seen, key)
	f.mu.Unlock()
	tpl, ok := f.tpl[key]
	if !ok {
		return "", errors.New("prompt not found: " + key)
	}
	for k, v := range vars {
		tpl = strings.ReplaceAll(tpl, "{{"+k+"}}", v)
	}
	return tpl, nil
}

// ---------- helpers ----------

// newTestRag 构建被测用例；可选传入 prompter，不传则走内置提示词回落
func newTestRag(t *testing.T, prompter ...Prompter) (*Rag, *fakeStore, *fakeRepo, *fakeCache, *fakeLLM) {
	t.Helper()
	store := &fakeStore{}
	repo := newFakeRepo()
	cache := newFakeCache()
	llmClient := &fakeLLM{}
	cfg := &conf.Data{
		Embedding: &conf.Data_Embedding{Dimension: 8},
		Rag: &conf.Data_Rag{
			TopK: 5, TopN: 3, MaxChunk: 100, Overlap: 20, EmbedBatch: 2,
			GraphTopK: 2, GraphHops: 1, GraphMaxEdges: 10, GraphMaxEntities: 3, GraphMaxRelations: 3,
		},
	}
	var p Prompter
	if len(prompter) > 0 {
		p = prompter[0]
	}
	r, err := NewRag(store, fakeGraphStore{}, repo, cache, cache, fakeEmbedder{}, llmClient, p, nil, cfg)
	if err != nil {
		t.Fatalf("NewRag: %v", err)
	}
	return r, store, repo, cache, llmClient
}

// ---------- tests ----------

// 入库：应切块、写向量库、写 MySQL 元数据，并清空问答缓存
func TestIngestWritesVectorsAndMetadata(t *testing.T) {
	r, store, repo, cache, _ := newTestRag(t)
	text := strings.Repeat("Milvus 支持 BM25 与稠密向量的混合检索。", 12)

	res, err := r.Ingest(context.Background(), "doc1", "a.txt", "rag", text)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Chunks == 0 || len(store.chunks) == 0 {
		t.Fatalf("未写入任何分块: %+v", res)
	}
	if res.Chars != len([]rune(text)) {
		t.Errorf("chars = %d, want %d", res.Chars, len([]rune(text)))
	}

	doc, err := repo.Get(context.Background(), "doc1")
	if err != nil || doc == nil {
		t.Fatalf("MySQL 元数据未写入: %v", err)
	}
	if doc.Status != "ready" || doc.Chunks != res.Chunks || doc.Mode != "rag" {
		t.Errorf("元数据不正确: %+v", doc)
	}
	if doc.Checksum == "" {
		t.Error("checksum 为空，无法做内容去重")
	}
	if cache.invalidSet == 0 {
		t.Error("入库后未清理问答缓存")
	}
}

// 同一内容重复入库：应命中 checksum 去重，不再重复向量化
func TestIngestDeduplicatesByChecksum(t *testing.T) {
	r, store, _, _, _ := newTestRag(t)
	text := "GraphRAG 通过知识图谱增强检索。"
	if _, err := r.Ingest(context.Background(), "doc2", "b.txt", "rag", text); err != nil {
		t.Fatalf("首次入库: %v", err)
	}
	inserts := store.inserts

	res, err := r.Ingest(context.Background(), "doc2", "b.txt", "rag", text)
	if err != nil {
		t.Fatalf("重复入库: %v", err)
	}
	if !res.Deduped {
		t.Error("应识别为重复内容（deduped=true）")
	}
	if store.inserts != inserts {
		t.Errorf("重复入库不应再写向量，inserts %d -> %d", inserts, store.inserts)
	}
}

// 并发入库同一文档：Redis 锁应拦下第二个
func TestIngestRejectsConcurrentSameDoc(t *testing.T) {
	r, _, _, cache, _ := newTestRag(t)
	// 手动占住锁，模拟另一个请求正在入库
	if ok, _ := cache.TryLock(context.Background(), "rag:lock:ingest:doc3", time.Minute); !ok {
		t.Fatal("准备锁失败")
	}
	if _, err := r.Ingest(context.Background(), "doc3", "c.txt", "rag", "内容"); err == nil {
		t.Fatal("并发入库应被拒绝")
	} else if !strings.Contains(err.Error(), "正在入库") {
		t.Errorf("错误信息不明确: %v", err)
	}
}

// 问答：首次调用 LLM，第二次命中 Redis 缓存不再调用
func TestQueryUsesCache(t *testing.T) {
	r, _, _, _, llmClient := newTestRag(t)
	ctx := context.Background()

	ans, err := r.Query(ctx, "Milvus 支持什么检索？", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if ans.Cached {
		t.Error("首次查询不应标记为缓存")
	}
	if llmClient.calls != 1 {
		t.Fatalf("首次查询应调用一次 LLM，实际 %d", llmClient.calls)
	}

	again, err := r.Query(ctx, "Milvus 支持什么检索？", "")
	if err != nil {
		t.Fatalf("重复 Query: %v", err)
	}
	if !again.Cached {
		t.Error("第二次查询应命中缓存")
	}
	if llmClient.calls != 1 {
		t.Errorf("命中缓存后不应再调用 LLM，实际 %d 次", llmClient.calls)
	}
}

// 提示词来自管理端：Query 必须使用模板渲染结果，而不是代码里的常量
func TestQueryUsesManagedPrompts(t *testing.T) {
	p := &fakePrompter{tpl: map[string]string{
		prompt.KeyRAGSystem: "系统提示词来自管理端",
		prompt.KeyRAGUser:   "问题={{question}}",
	}}
	r, _, _, _, llmClient := newTestRag(t, p)

	if _, err := r.Query(context.Background(), "问题A", ""); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if llmClient.lastSystem != "系统提示词来自管理端" {
		t.Errorf("system 未使用管理端模板: %q", llmClient.lastSystem)
	}
	if llmClient.lastUser != "问题=问题A" {
		t.Errorf("user 模板渲染错误: %q", llmClient.lastUser)
	}
	if len(p.seen) < 2 {
		t.Errorf("应分别渲染 system 与 user 模板，实际 %v", p.seen)
	}
}

// 提示词服务故障时回落到内置模板，问答不中断
func TestQueryFallsBackToBuiltinPrompt(t *testing.T) {
	p := &fakePrompter{tpl: map[string]string{}} // 任何 key 都返回错误
	r, _, _, _, llmClient := newTestRag(t, p)

	if _, err := r.Query(context.Background(), "随便问", ""); err != nil {
		t.Fatalf("Query 不应因提示词服务故障而失败: %v", err)
	}
	if !strings.Contains(llmClient.lastSystem, "知识库问答助手") {
		t.Errorf("未回落到内置系统提示词: %q", llmClient.lastSystem)
	}
	if !strings.Contains(llmClient.lastUser, "随便问") {
		t.Errorf("内置用户模板未渲染问题: %q", llmClient.lastUser)
	}
}

// 统计：分块数来自 Milvus，文档数来自 MySQL
func TestStatsCombinesMilvusAndMySQL(t *testing.T) {
	r, _, _, _, _ := newTestRag(t)
	ctx := context.Background()
	if _, err := r.Ingest(ctx, "d9", "x.txt", "rag", strings.Repeat("统计测试文本。", 30)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	st := r.Stats(ctx)
	if st.Docs != 1 {
		t.Errorf("Docs = %d, want 1", st.Docs)
	}
	if st.Chunks == 0 {
		t.Error("Chunks 应大于 0")
	}
}

// 删除：向量 + 元数据都要清掉
func TestDeleteDocClearsStoreAndRepo(t *testing.T) {
	r, store, repo, _, _ := newTestRag(t)
	ctx := context.Background()
	if _, err := r.Ingest(ctx, "d10", "y.txt", "rag", strings.Repeat("待删除文本。", 20)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, err := r.DeleteDoc(ctx, "d10"); err != nil {
		t.Fatalf("DeleteDoc: %v", err)
	}
	if len(store.chunks) != 0 {
		t.Errorf("向量分块未清空，剩余 %d", len(store.chunks))
	}
	if doc, _ := repo.Get(ctx, "d10"); doc != nil {
		t.Error("MySQL 元数据未删除")
	}
}

// 图谱入库：走 graph 分支，写入实体/关系计数
func TestGraphIngestRecordsCounts(t *testing.T) {
	r, _, repo, _, _ := newTestRag(t)
	ctx := context.Background()
	res, err := r.Ingest(ctx, "g1", "graph.txt", "graph", "Milvus 是一个向量数据库。")
	if err != nil {
		t.Fatalf("Graph ingest: %v", err)
	}
	if res.Entities == 0 {
		t.Error("应抽取出实体")
	}
	doc, _ := repo.Get(ctx, "g1")
	if doc == nil || doc.Mode != "graph" || doc.Entities != res.Entities {
		t.Errorf("图谱文档元数据不正确: %+v", doc)
	}
}

// —— P1-07：更新文档必须清掉旧版本向量 ——

// 内容变化后重新入库：旧版本分块要按主键删掉，否则新旧两版会同时被召回。
func TestReingestPurgesOldVersion(t *testing.T) {
	r, store, _, _, _ := newTestRag(t)
	ctx := context.Background()

	v1 := strings.Repeat("旧版本内容：Milvus 只支持向量检索。", 10)
	if _, err := r.Ingest(ctx, "doc-v", "v.txt", "rag", v1); err != nil {
		t.Fatalf("首次入库: %v", err)
	}
	store.mu.Lock()
	oldIDs := append([]int64(nil), store.ids...)
	store.mu.Unlock()
	if len(oldIDs) == 0 {
		t.Fatal("首次入库没有产生分块 ID")
	}

	v2 := strings.Repeat("新版本内容：Milvus 支持 BM25 与向量混合检索。", 14)
	res, err := r.Ingest(ctx, "doc-v", "v.txt", "rag", v2)
	if err != nil {
		t.Fatalf("重新入库: %v", err)
	}
	if res.Deduped {
		t.Fatal("内容已变，不应命中 checksum 去重")
	}

	store.mu.Lock()
	deleted := append([]int64(nil), store.deleted...)
	store.mu.Unlock()
	if len(deleted) != len(oldIDs) {
		t.Fatalf("应删除 %d 个旧版本分块，实际 %d", len(oldIDs), len(deleted))
	}
	for i, id := range deleted {
		if id != oldIDs[i] {
			t.Fatalf("删除的 ID 与旧版本不一致: got %v want %v", deleted, oldIDs)
		}
	}
	if res.Purged != len(oldIDs) {
		t.Fatalf("Purged = %d, want %d", res.Purged, len(oldIDs))
	}
}

// 新版本写入失败时不能动旧版本：先删后写会造成「文档还在、向量没了」。
func TestReingestKeepsOldVersionWhenWriteFails(t *testing.T) {
	r, store, repo, _, _ := newTestRag(t)
	ctx := context.Background()

	v1 := strings.Repeat("可用的旧版本内容。", 10)
	if _, err := r.Ingest(ctx, "doc-f", "f.txt", "rag", v1); err != nil {
		t.Fatalf("首次入库: %v", err)
	}
	store.mu.Lock()
	oldChunks := len(store.chunks)
	store.failInsert = true
	store.mu.Unlock()

	if _, err := r.Ingest(ctx, "doc-f", "f.txt", "rag", strings.Repeat("新版本内容。", 12)); err == nil {
		t.Fatal("写入失败时应返回错误")
	}

	store.mu.Lock()
	deleted := len(store.deleted)
	chunks := len(store.chunks)
	store.mu.Unlock()
	if deleted != 0 {
		t.Fatalf("写入失败时不应删除旧版本，实际删了 %d 个", deleted)
	}
	if chunks != oldChunks {
		t.Fatalf("旧版本分块被改动: %d -> %d", oldChunks, chunks)
	}
	doc, err := repo.Get(ctx, "doc-f")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if doc.Status != "failed" {
		t.Fatalf("失败状态应落库，实际 %s", doc.Status)
	}
}
