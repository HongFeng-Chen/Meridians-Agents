package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go_agent/internal/biz/rag"
	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/conf"

	"google.golang.org/protobuf/types/known/durationpb"
)

// ---------- fakes ----------

type fakeRepo struct {
	mu    sync.Mutex
	mems  map[string]*LongTermMemory
	order []string
}

func newFakeRepo() *fakeRepo { return &fakeRepo{mems: map[string]*LongTermMemory{}} }

func (f *fakeRepo) Save(ctx context.Context, m *LongTermMemory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.mems[m.ID]; !ok {
		f.order = append(f.order, m.ID)
	}
	cp := *m
	f.mems[m.ID] = &cp
	return nil
}
func (f *fakeRepo) Get(ctx context.Context, id string) (*LongTermMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.mems[id]; ok {
		cp := *m
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeRepo) GetByIDs(ctx context.Context, ids []string) ([]*LongTermMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*LongTermMemory
	for _, id := range ids {
		if m, ok := f.mems[id]; ok {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeRepo) FindByChecksum(ctx context.Context, userID, checksum string) (*LongTermMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 稳定遍历，保证测试可重复
	keys := append([]string(nil), f.order...)
	sort.Strings(keys)
	for _, id := range keys {
		m := f.mems[id]
		if m.UserID == userID && m.Checksum == checksum {
			cp := *m
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeRepo) List(ctx context.Context, userID string, includeExpired bool, limit int) ([]*LongTermMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*LongTermMemory
	for _, id := range f.order {
		m := f.mems[id]
		if userID != "" && m.UserID != userID {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}
func (f *fakeRepo) ListExpired(ctx context.Context, limit int) ([]*LongTermMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	var out []*LongTermMemory
	for _, id := range f.order {
		m := f.mems[id]
		if m.ExpiresAt != nil && now.After(*m.ExpiresAt) {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fakeRepo) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mems, id)
	for i, v := range f.order {
		if v == id {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeRepo) Count(ctx context.Context) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	var total, expired int64
	for _, m := range f.mems {
		total++
		if m.ExpiresAt != nil && now.After(*m.ExpiresAt) {
			expired++
		}
	}
	return total, expired, nil
}

// ---------- 规则与检索历史（接口满足即可，测试不覆盖） ----------

func (f *fakeRepo) ListRules(_ context.Context) ([]*MemoryRule, error)       { return nil, nil }
func (f *fakeRepo) GetRule(_ context.Context, _ uint64) (*MemoryRule, error) { return nil, ErrNotFound }
func (f *fakeRepo) SaveRule(_ context.Context, _ *MemoryRule) error          { return nil }
func (f *fakeRepo) DeleteRule(_ context.Context, _ uint64) error             { return nil }
func (f *fakeRepo) CountBuiltinRules(_ context.Context) (int64, error)       { return 0, nil }
func (f *fakeRepo) SaveRecallLog(_ context.Context, _ *RecallLog) error      { return nil }
func (f *fakeRepo) ListRecallLogs(_ context.Context, _ string, _ int) ([]*RecallLog, error) {
	return nil, nil
}
func (f *fakeRepo) MarkRecalled(_ context.Context, _ []string, _ time.Time) error { return nil }

type fakeShort struct {
	mu      sync.Mutex
	session map[string][]Message
	ttls    []time.Duration
}

func (f *fakeShort) Append(ctx context.Context, sessionID string, msg Message, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ttls = append(f.ttls, ttl)
	f.session[sessionID] = append(f.session[sessionID], msg)
	return nil
}
func (f *fakeShort) History(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.session[sessionID]
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return append([]Message(nil), msgs...), nil
}
func (f *fakeShort) Len(ctx context.Context, sessionID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.session[sessionID])), nil
}
func (f *fakeShort) Clear(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.session, sessionID)
	return nil
}

type fakeVec struct {
	mu      sync.Mutex
	items   map[string][]float32
	hits    []RecalledMemory // Search 固定返回（简化）
	ensured int
}

func newFakeVec() *fakeVec { return &fakeVec{items: map[string][]float32{}} }

func (f *fakeVec) EnsureCollection(ctx context.Context) error { f.ensured++; return nil }
func (f *fakeVec) Insert(ctx context.Context, m *LongTermMemory, vec []float32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[m.ID] = vec
	return nil
}
func (f *fakeVec) Search(ctx context.Context, query string, qvec []float32, topK int, userID string) ([]RecalledMemory, error) {
	return f.hits, nil
}
func (f *fakeVec) DeleteByMemory(ctx context.Context, id string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[id]; ok {
		delete(f.items, id)
		return 1, nil
	}
	return 0, nil
}
func (f *fakeVec) Count(ctx context.Context) (int64, error) { return int64(len(f.items)), nil }

type fakeGraph struct {
	insertedEnts []graphrag.Entity
	insertedRels []graphrag.Relation
	deleted      []string
	seeds        []graphrag.Entity
	relsOf       []graphrag.Relation
}

func (f *fakeGraph) EnsureCollections(ctx context.Context) error { return nil }
func (f *fakeGraph) InsertEntities(ctx context.Context, ents []graphrag.Entity, contents []string, vecs [][]float32) error {
	f.insertedEnts = append(f.insertedEnts, ents...)
	return nil
}
func (f *fakeGraph) InsertRelations(ctx context.Context, rels []graphrag.Relation, vecs [][]float32) error {
	f.insertedRels = append(f.insertedRels, rels...)
	return nil
}
func (f *fakeGraph) ExistingEntities(ctx context.Context, names []string) (map[string]graphrag.Entity, error) {
	return map[string]graphrag.Entity{}, nil
}
func (f *fakeGraph) SearchEntities(ctx context.Context, q string, qvec []float32, topK int) ([]graphrag.Entity, error) {
	return f.seeds, nil
}
func (f *fakeGraph) RelationsOf(ctx context.Context, names []string, limit int) ([]graphrag.Relation, error) {
	return f.relsOf, nil
}
func (f *fakeGraph) EntitiesByNames(ctx context.Context, names []string) ([]graphrag.Entity, error) {
	return nil, nil
}
func (f *fakeGraph) Counts(ctx context.Context) (int64, int64, error) { return 0, 0, nil }
func (f *fakeGraph) DeleteOrphanNodes(ctx context.Context, prefix string) (int, int, error) {
	f.deleted = append(f.deleted, prefix)
	return 0, 0, nil
}

type fakeEmbed struct{}

func (fakeEmbed) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2}
	}
	return out, nil
}
func (fakeEmbed) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}
func (fakeEmbed) Dim() int      { return 2 }
func (fakeEmbed) Model() string { return "fake-embed" }

type fakeLLM struct{ calls int }

func (f *fakeLLM) Chat(ctx context.Context, system, user string) (string, error) {
	f.calls++
	if !strings.Contains(system, "记忆") && !strings.Contains(system, "memory") {
		return "", fmt.Errorf("system prompt 不是记忆模板")
	}
	return "基于记忆的回答 [M1]", nil
}
func (f *fakeLLM) ChatTemp(ctx context.Context, system, user string, temp float64) (string, error) {
	return `{"entities":[{"name":"Milvus","type":"技术","description":"向量库"},{"name":"Redis","type":"技术","description":"缓存"}],"relations":[{"source":"Milvus","target":"Redis","relation":"配合使用","description":"混合架构"}]}`, nil
}
func (f *fakeLLM) Model() string { return "fake-llm" }

type fakePrompter struct{}

func (fakePrompter) Render(ctx context.Context, key string, vars map[string]string) (string, error) {
	return "", fmt.Errorf("走内置回落")
}

// ---------- helpers ----------

func newTestUseCase(t *testing.T, graphOn bool) (*UseCase, *fakeRepo, *fakeShort, *fakeVec, *fakeGraph, *fakeLLM) {
	t.Helper()
	repo := newFakeRepo()
	short := &fakeShort{session: map[string][]Message{}}
	vec := newFakeVec()
	graph := &fakeGraph{}
	llm := &fakeLLM{}
	c := &conf.Data{
		Memory: &conf.Data_Memory{
			ShortTTL:     durationpb.New(time.Hour),
			RecallTopK:   5,
			GraphEnabled: graphOn,
			HistoryLimit: 10,
		},
	}
	u, err := New(repo, short, vec, graph, fakeEmbed{}, llm, fakePrompter{}, nil, c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return u, repo, short, vec, graph, llm
}

// ---------- tests ----------

// Remember：写入 MySQL + 向量化 + 图谱抽取，重复内容去重
func TestRememberAndDedup(t *testing.T) {
	u, repo, _, vec, graph, _ := newTestUseCase(t, true)
	res, err := u.Remember(context.Background(), RememberInput{
		Content: "用户偏好简洁的回答",
		Kind:    KindPreference,
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if res.Deduped {
		t.Fatal("首次写入不应去重")
	}
	if res.Memory.ID == "" || res.Memory.Checksum == "" {
		t.Fatal("缺少 ID 或 checksum")
	}
	if len(vec.items) != 1 {
		t.Fatalf("应写入 1 条向量，实际 %d", len(vec.items))
	}
	if len(graph.insertedEnts) != 2 || len(graph.insertedRels) != 1 {
		t.Fatalf("图谱应写入 2 实体 1 关系，实际 %d/%d", len(graph.insertedEnts), len(graph.insertedRels))
	}
	for _, e := range graph.insertedEnts {
		if len(e.ChunkRefs) != 1 || !strings.HasPrefix(e.ChunkRefs[0], res.Memory.ID+"#") {
			t.Fatalf("实体引用前缀错误: %v", e.ChunkRefs)
		}
	}
	if repo.mems[res.Memory.ID].Entities != 2 {
		t.Fatal("图谱统计未回填 MySQL")
	}

	// 同内容再次写入 → 去重
	res2, err := u.Remember(context.Background(), RememberInput{Content: "用户偏好简洁的回答"})
	if err != nil {
		t.Fatalf("Remember 2: %v", err)
	}
	if !res2.Deduped {
		t.Fatal("重复内容应被去重")
	}
	if res2.Memory.ID != res.Memory.ID {
		t.Fatalf("去重应复用原 ID: %s != %s", res2.Memory.ID, res.Memory.ID)
	}
	if len(vec.items) != 1 {
		t.Fatalf("去重后不应新增向量，实际 %d", len(vec.items))
	}
}

// 过期记忆：Recall 跳过、Purge 清理
func TestExpiryAndPurge(t *testing.T) {
	u, repo, _, vec, graph, _ := newTestUseCase(t, false)

	// 已过期的记忆
	past := time.Now().Add(-time.Hour)
	expired := &LongTermMemory{
		ID: "mem-exp", UserID: "default", Kind: KindFact,
		Content: "过期记忆", Checksum: "c-exp", ExpiresAt: &past,
	}
	_ = repo.Save(context.Background(), expired)
	vec.items["mem-exp"] = []float32{0.1}

	// 未过期的记忆
	future := time.Now().Add(24 * time.Hour)
	ok := &LongTermMemory{
		ID: "mem-ok", UserID: "default", Kind: KindFact,
		Content: "有效记忆", Checksum: "c-ok", ExpiresAt: &future,
	}
	_ = repo.Save(context.Background(), ok)
	vec.items["mem-ok"] = []float32{0.2}

	vec.hits = []RecalledMemory{
		{LongTermMemory: &LongTermMemory{ID: "mem-exp"}, Score: 0.9},
		{LongTermMemory: &LongTermMemory{ID: "mem-ok"}, Score: 0.8},
	}
	res, err := u.Recall(context.Background(), "记忆", "", 0, false, RecallSourceAPI)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].ID != "mem-ok" {
		t.Fatalf("应只召回未过期记忆，实际 %+v", res.Memories)
	}

	// PurgeExpired 只清理过期条目
	n, err := u.PurgeExpired(context.Background(), 0)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("应清理 1 条，实际 %d", n)
	}
	if _, ok := vec.items["mem-exp"]; ok {
		t.Fatal("过期记忆的向量未删除")
	}
	if _, ok := vec.items["mem-ok"]; !ok {
		t.Fatal("有效记忆的向量不应被删除")
	}
	if !strings.HasPrefix(graph.deleted[0], "mem-exp#") {
		t.Fatalf("图谱孤儿清理前缀错误: %v", graph.deleted)
	}
}

// TTL 入参：ttl_days 转换成 expires_at
func TestRememberTTL(t *testing.T) {
	u, repo, _, _, _, _ := newTestUseCase(t, false)
	res, err := u.Remember(context.Background(), RememberInput{Content: "临时事实", TTL: 48 * time.Hour})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	m, _ := repo.Get(context.Background(), res.Memory.ID)
	if m.ExpiresAt == nil {
		t.Fatal("TTL 未转换为 expires_at")
	}
	if d := time.Until(*m.ExpiresAt); d < 47*time.Hour || d > 48*time.Hour {
		t.Fatalf("过期时间不在预期范围: %s", d)
	}
}

// Answer：短期历史 + 长期召回 + LLM，并把本轮问答写回短期记忆
func TestAnswerFlow(t *testing.T) {
	u, _, short, _, _, llm := newTestUseCase(t, false)

	ok := &LongTermMemory{ID: "mem-1", UserID: "default", Kind: KindFact, Content: "用户叫小明", Checksum: "c1"}
	vecItems := u.vec.(*fakeVec)
	vecItems.hits = []RecalledMemory{{LongTermMemory: ok, Score: 0.9}}

	ans, err := u.Answer(context.Background(), "sess-1", "", "我叫什么名字？")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if !strings.Contains(ans.Answer, "[M1]") {
		t.Fatalf("回答应引用记忆编号: %s", ans.Answer)
	}
	if llm.calls != 1 {
		t.Fatalf("LLM 应调用 1 次，实际 %d", llm.calls)
	}
	// 本轮问答写回短期记忆
	n, _ := short.Len(context.Background(), "sess-1")
	if n != 2 {
		t.Fatalf("短期记忆应有 2 条，实际 %d", n)
	}
	msgs, _ := short.History(context.Background(), "sess-1", 10)
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("消息顺序错误: %+v", msgs)
	}
}

// 图谱召回：实体种子 -> 关系扩展 -> 子图上下文
func TestRecallWithGraph(t *testing.T) {
	u, _, _, vec, graph, _ := newTestUseCase(t, false)
	graph.seeds = []graphrag.Entity{{Name: "Milvus", Type: "技术", Freq: 3}}
	graph.relsOf = []graphrag.Relation{{Source: "Milvus", Target: "Redis", Relation: "配合使用", Weight: 2}}

	res, err := u.Recall(context.Background(), "检索架构", "", 0, true, RecallSourceAPI)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Graph == nil {
		t.Fatal("应返回图谱上下文")
	}
	if len(res.Graph.Entities) != 1 {
		t.Fatalf("实体数错误: %d", len(res.Graph.Entities))
	}
	if len(res.Graph.Relations) != 1 {
		t.Fatalf("关系数错误: %d", len(res.Graph.Relations))
	}
	_ = vec
}

// 短期记忆：追加/清空，TTL 生效
func TestShortTermOps(t *testing.T) {
	u, _, short, _, _, _ := newTestUseCase(t, false)
	ctx := context.Background()
	if err := u.AppendMessage(ctx, "s1", "user", "你好"); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := u.AppendMessage(ctx, "s1", "", "空角色默认 user"); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	msgs, _ := u.History(ctx, "s1", 0)
	if len(msgs) != 2 || msgs[1].Role != "user" {
		t.Fatalf("历史消息错误: %+v", msgs)
	}
	if err := u.AppendMessage(ctx, "s1", "", ""); err == nil {
		t.Fatal("空 content 应报错")
	}
	if short.ttls[0] != time.Hour {
		t.Fatalf("TTL 应来自配置: %s", short.ttls[0])
	}
	_ = u.ClearSession(ctx, "s1")
	n, _ := u.SessionLen(ctx, "s1")
	if n != 0 {
		t.Fatal("清空后应无消息")
	}
}

// Delete：MySQL + 向量 + 图谱三处同时清理
func TestDeleteMemory(t *testing.T) {
	u, repo, _, vec, graph, _ := newTestUseCase(t, false)
	res, _ := u.Remember(context.Background(), RememberInput{Content: "待删除", Graph: boolPtr(false)})
	id := res.Memory.ID
	vec.items[id] = []float32{0.3}

	if _, err := u.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := vec.items[id]; ok {
		t.Fatal("向量未删除")
	}
	if len(graph.deleted) != 1 || !strings.HasPrefix(graph.deleted[0], id+"#") {
		t.Fatalf("图谱清理前缀错误: %v", graph.deleted)
	}
	if _, err := u.Get(context.Background(), id); err != ErrNotFound {
		t.Fatalf("删除后应返回 ErrNotFound，实际 %v", err)
	}
	_ = repo
}

// Recall：向量命中但 MySQL 已删除 -> 跳过（向量残留防御）
func TestRecallSkipsMissingRepo(t *testing.T) {
	u, _, _, vec, _, _ := newTestUseCase(t, false)
	vec.hits = []RecalledMemory{
		{LongTermMemory: &LongTermMemory{ID: "mem-ghost"}, Score: 0.9},
	}
	res, err := u.Recall(context.Background(), "查询", "", 0, false, RecallSourceAPI)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Memories) != 0 {
		t.Fatalf("MySQL 无记录时应跳过，实际 %+v", res.Memories)
	}
}

func boolPtr(b bool) *bool { return &b }

// 编译期确认接口实现
var (
	_ rag.Embedder = fakeEmbed{}
	_ GraphStore   = (*fakeGraph)(nil)
)
