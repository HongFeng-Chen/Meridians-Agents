package conversation

// 会话用例单测：fake repo 验证 ID 生成（会话 id = task id 透传）、消息追加校验、
// 目录预览刷新等核心规则。
import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"go_agent/internal/biz/memory"
	"go_agent/internal/pkg/identity"
)

type fakeRepo struct {
	convs  map[string]*Conversation
	msgs   map[string][]Message
	called []string
	synced map[string]time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{convs: map[string]*Conversation{}, msgs: map[string][]Message{}, synced: map[string]time.Time{}}
}

// ListUnsynced 未归档会话（内存实现，按 updated_at 倒序）
func (f *fakeRepo) ListUnsynced(_ context.Context, userID, excludeID string, limit int) ([]Conversation, error) {
	out := make([]Conversation, 0)
	for id, c := range f.convs {
		if id == excludeID || c.MemorySyncedAt != nil || c.MessageCount < 2 {
			continue
		}
		if userID != "" && c.UserID != userID {
			continue
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkMemorySynced 回填归档时间
func (f *fakeRepo) MarkMemorySynced(_ context.Context, id string, at time.Time) error {
	f.synced[id] = at
	if c, ok := f.convs[id]; ok {
		c.MemorySyncedAt = &at
	}
	return nil
}

func (f *fakeRepo) Create(_ context.Context, c *Conversation) error {
	f.called = append(f.called, "create:"+c.ID)
	f.convs[c.ID] = c
	return nil
}

func (f *fakeRepo) List(_ context.Context, f2 Filter) ([]Conversation, int64, error) {
	return nil, 0, nil
}

func (f *fakeRepo) Get(_ context.Context, id string) (*Conversation, error) {
	c, ok := f.convs[id]
	if !ok {
		return nil, nil
	}
	return c, nil
}

func (f *fakeRepo) Update(_ context.Context, c *Conversation) error {
	f.convs[c.ID] = c
	return nil
}

func (f *fakeRepo) Delete(_ context.Context, id string) error {
	delete(f.convs, id)
	delete(f.msgs, id)
	return nil
}

func (f *fakeRepo) AppendMessage(_ context.Context, m *Message) error {
	m.ID = int64(len(f.msgs[m.ConversationID]) + 1)
	f.msgs[m.ConversationID] = append(f.msgs[m.ConversationID], *m)
	f.convs[m.ConversationID].MessageCount++
	f.convs[m.ConversationID].LastMessage = m.Content
	f.called = append(f.called, "append:"+m.Role)
	return nil
}

func (f *fakeRepo) ListMessages(_ context.Context, conversationID string) ([]Message, error) {
	return f.msgs[conversationID], nil
}

func (f *fakeRepo) ListMessagesPaged(_ context.Context, conversationID string, limit int, beforeID int64) ([]Message, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	all := f.msgs[conversationID]
	// 过滤游标之前的消息（id 正序）
	var cand []Message
	for _, m := range all {
		if beforeID <= 0 || m.ID < beforeID {
			cand = append(cand, m)
		}
	}
	hasMore := len(cand) > limit
	if hasMore {
		cand = cand[len(cand)-limit:] // 取最新 limit 条
	}
	return cand, hasMore, nil
}

func TestCreateWithTaskID(t *testing.T) {
	uc := NewUseCase(newFakeRepo(), nil)
	// 会话 id = task id：显式传入任务 ID
	c, _, err := uc.Create(context.Background(), "42", "混排编排演示", "dag-demo")
	if err != nil {
		t.Fatalf("带任务 ID 创建失败: %v", err)
	}
	if c.ID != "42" || c.AgentKey != "dag-demo" {
		t.Fatalf("会话 ID/Agent 不符合预期: %+v", c)
	}
	// 空 ID 自动生成
	c2, _, err := uc.Create(context.Background(), "", "检索会话", "")
	if err != nil || !strings.HasPrefix(c2.ID, "c-") {
		t.Fatalf("自动生成会话 ID 异常: %v %+v", err, c2)
	}
}

func TestAppendValidation(t *testing.T) {
	r := newFakeRepo()
	uc := NewUseCase(r, nil)
	ctx := context.Background()
	c, _, _ := uc.Create(ctx, "", "测试", "")

	if _, err := uc.Append(ctx, c.ID, "system", "hi", "", 0); !errors.Is(err, ErrInvalidConv) {
		t.Fatalf("非法角色应被拒绝: %v", err)
	}
	if _, err := uc.Append(ctx, c.ID, "user", "  ", "", 0); !errors.Is(err, ErrInvalidConv) {
		t.Fatalf("空内容应被拒绝: %v", err)
	}
	if _, err := uc.Append(ctx, "not-exists", "user", "hi", "", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的会话应返回 NotFound: %v", err)
	}
	m, err := uc.Append(ctx, c.ID, "user", "你好", `{"op":"agent"}`, 7)
	if err != nil || m.TaskID != 7 {
		t.Fatalf("追加消息失败: %v %+v", err, m)
	}
	if _, err = uc.Append(ctx, c.ID, "assistant", "回复内容", "", 7); err != nil {
		t.Fatalf("追加 assistant 消息失败: %v", err)
	}
	detail, msgs, err := uc.Detail(ctx, c.ID)
	if err != nil || len(msgs) != 2 || detail.MessageCount != 2 {
		t.Fatalf("会话详情消息数异常: %v %d", err, len(msgs))
	}
	if detail.LastMessage != "回复内容" {
		t.Fatalf("最近消息预览未刷新: %q", detail.LastMessage)
	}
}

func TestRenameAndDelete(t *testing.T) {
	r := newFakeRepo()
	uc := NewUseCase(r, nil)
	ctx := context.Background()
	c, _, _ := uc.Create(ctx, "", "旧标题", "")
	if _, err := uc.Rename(ctx, c.ID, "新标题"); err != nil {
		t.Fatalf("重命名失败: %v", err)
	}
	got, _ := uc.Get(ctx, c.ID)
	if got.Title != "新标题" {
		t.Fatalf("标题未更新: %q", got.Title)
	}
	if err := uc.Delete(ctx, c.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := uc.Get(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应 NotFound: %v", err)
	}
	_ = time.Now
}

// —— 辩论方法桩（辩论编排不在此文件测试）——

func (f *fakeRepo) CreateDebate(_ context.Context, d *Debate) error {
	f.called = append(f.called, "create_debate")
	return nil
}

func (f *fakeRepo) GetDebate(_ context.Context, id int64) (*Debate, error) {
	return nil, ErrNotFound
}

func (f *fakeRepo) GetActiveDebate(_ context.Context, _ string) (*Debate, error) {
	return nil, nil
}

func (f *fakeRepo) GetLatestDebate(_ context.Context, _ string) (*Debate, error) {
	return nil, nil
}

func (f *fakeRepo) UpdateDebate(_ context.Context, _ *Debate) error { return nil }

func (f *fakeRepo) StopOrphanDebates(_ context.Context) (int64, error) { return 0, nil }

// TestCreateRecordsOwner 会话创建时固化归属用户（认证态取令牌身份，未认证取 default）
func TestCreateRecordsOwner(t *testing.T) {
	repo := newFakeRepo()
	u := NewUseCase(repo, nil)

	c, _, err := u.Create(identity.With(context.Background(), identity.Info{UserID: "alice", Auth: "jwt"}), "", "测试会话", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.UserID != "alice" {
		t.Fatalf("归属用户应为 alice，实际 %q", c.UserID)
	}
	if repo.convs[c.ID].UserID != "alice" {
		t.Fatalf("仓储落库归属不符: %+v", repo.convs[c.ID])
	}

	// 未注入身份（单机开发态）→ default
	c2, _, err := u.Create(context.Background(), "", "匿名会话", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c2.UserID != identity.DefaultUser {
		t.Fatalf("未认证应落 default，实际 %q", c2.UserID)
	}
}

// —— 会话归档进记忆系统 ——

type fakeArchiver struct {
	got     []string
	failOn  map[string]bool
	skipped bool
}

func (f *fakeArchiver) SummarizeConversation(_ context.Context, sessionID, userID, title string) (*memory.SummarizeResult, error) {
	f.got = append(f.got, sessionID)
	if f.failOn[sessionID] {
		return nil, errors.New("LLM 调用失败")
	}
	return &memory.SummarizeResult{SessionID: sessionID, Title: title, Messages: 4,
		Skipped: f.skipped, MemoryID: "m-" + sessionID}, nil
}

// TestArchivePreviousConversation 开新会话时归档「上次未归档会话」：
// 只挑有实质消息、未归档、非新会话本身的那一条；归档后不再重复，失败不标记。
func TestArchivePreviousConversation(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	// 旧会话（已被归档，不该再处理）
	repo.convs["old-synced"] = &Conversation{ID: "old-synced", UserID: "alice", Title: "旧会话",
		MessageCount: 6, UpdatedAt: now.Add(-2 * time.Hour), MemorySyncedAt: &now}
	// 上次会话（未归档，应被总结）
	repo.convs["prev"] = &Conversation{ID: "prev", UserID: "alice", Title: "上次会话",
		MessageCount: 4, UpdatedAt: now.Add(-time.Hour)}
	// 只有一条消息的会话（不算实质对话）
	repo.convs["thin"] = &Conversation{ID: "thin", UserID: "alice", Title: "闲聊半句",
		MessageCount: 1, UpdatedAt: now.Add(-30 * time.Minute)}
	// 新会话本身（exclude）
	repo.convs["new"] = &Conversation{ID: "new", UserID: "alice", Title: "新会话", UpdatedAt: now}

	arch := &fakeArchiver{}
	u := &UseCase{repo: repo, archiver: arch}
	u.archivePreviousConversations(context.Background(), "alice", "new")

	if len(arch.got) != 1 || arch.got[0] != "prev" {
		t.Fatalf("应只总结上次未归档会话 prev，实际 %v", arch.got)
	}
	if _, ok := repo.synced["prev"]; !ok {
		t.Fatal("成功后应回填 memory_synced_at")
	}
	if _, ok := repo.synced["thin"]; ok {
		t.Fatal("消息不足的会话不应被归档")
	}

	// 再跑一次：已归档，不应重复总结
	arch.got = nil
	u.archivePreviousConversations(context.Background(), "alice", "new")
	if len(arch.got) != 0 {
		t.Fatalf("已归档会话不应重复总结，实际 %v", arch.got)
	}
}

// TestArchivePreviousConversationFailureKeepsUnsynced 总结失败不标记，下次还会重试
func TestArchivePreviousConversationFailureKeepsUnsynced(t *testing.T) {
	repo := newFakeRepo()
	repo.convs["prev"] = &Conversation{ID: "prev", UserID: "bob", Title: "上次会话",
		MessageCount: 3, UpdatedAt: time.Now().Add(-time.Hour)}
	arch := &fakeArchiver{failOn: map[string]bool{"prev": true}}
	u := &UseCase{repo: repo, archiver: arch}
	u.archivePreviousConversations(context.Background(), "bob", "new")
	if _, ok := repo.synced["prev"]; ok {
		t.Fatal("失败时不应标记已归档，否则会丢数据")
	}
}

// TestCreateWithoutArchiver 未装配记忆系统时创建会话不受影响
func TestCreateWithoutArchiver(t *testing.T) {
	repo := newFakeRepo()
	u := NewUseCase(repo, nil)
	if _, _, err := u.Create(context.Background(), "", "无记忆系统", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// —— 新会话启动召回（只召回长期记忆） ——

type fakeBoot struct {
	recallQueries []string
	recallUser    string
	recentUser    string
	mems          []memory.RecalledMemory
	recent        []*memory.LongTermMemory
	err           error
}

func (f *fakeBoot) Recall(_ context.Context, query, userID string, _ int, _ bool, source string) (*memory.RecallResult, error) {
	f.recallQueries = append(f.recallQueries, query)
	f.recallUser = userID
	if f.err != nil {
		return nil, f.err
	}
	return &memory.RecallResult{Query: query, UserID: userID, Memories: f.mems}, nil
}

func (f *fakeBoot) Recent(_ context.Context, userID string, _ int) ([]*memory.LongTermMemory, error) {
	f.recentUser = userID
	if f.err != nil {
		return nil, f.err
	}
	return f.recent, nil
}

// TestCreateBootstrapsLongTermMemory 新会话创建时按标题召回长期记忆：
// 只带记忆内容，不夹带任何历史会话消息；弱相关（< 0.15）被过滤。
func TestCreateBootstrapsLongTermMemory(t *testing.T) {
	repo := newFakeRepo()
	boot := &fakeBoot{mems: []memory.RecalledMemory{
		{LongTermMemory: &memory.LongTermMemory{ID: "m1", Kind: "insight", Content: "上次把 MCP 工具改成了幂等调用"},
			Score: 0.72},
		{LongTermMemory: &memory.LongTermMemory{ID: "m2", Kind: "fact", Content: "弱相关噪声"}, Score: 0.04},
	}}
	u := &UseCase{repo: repo, boot: boot}

	c, bs, err := u.Create(identity.With(context.Background(), identity.Info{UserID: "alice"}), "", "MCP 工具怎么改的", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(boot.recallQueries) != 1 || boot.recallQueries[0] != "MCP 工具怎么改的" {
		t.Fatalf("应以会话标题做一次语义召回，实际 %v", boot.recallQueries)
	}
	if boot.recallUser != "alice" {
		t.Fatalf("召回命名空间应为当前用户 alice，实际 %q", boot.recallUser)
	}
	if bs == nil || bs.Source != BootstrapSourceMemory || bs.Skipped {
		t.Fatalf("启动召回结果异常: %+v", bs)
	}
	if len(bs.Memories) != 1 || bs.Memories[0].ID != "m1" {
		t.Fatalf("应过滤弱相关记忆，只留 m1，实际 %+v", bs.Memories)
	}
	if !strings.Contains(bs.Block, "上次把 MCP 工具改成了幂等调用") {
		t.Fatalf("上下文块应包含召回正文: %q", bs.Block)
	}
	// 新会话本身只有目录记录，没有消息
	if len(repo.msgs[c.ID]) != 0 {
		t.Fatalf("新会话不应带任何历史消息，实际 %d 条", len(repo.msgs[c.ID]))
	}
}

// TestCreateBootstrapFallsBackToRecent 标题为空时退化为最近沉淀，不发空查询
func TestCreateBootstrapFallsBackToRecent(t *testing.T) {
	repo := newFakeRepo()
	boot := &fakeBoot{recent: []*memory.LongTermMemory{
		{ID: "m9", Kind: "insight", Content: "用户偏好表格展示"},
	}}
	u := &UseCase{repo: repo, boot: boot}
	_, bs, err := u.Create(identity.With(context.Background(), identity.Info{UserID: "bob"}), "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(boot.recallQueries) != 0 {
		t.Fatalf("空标题不应发语义召回（空 query 会报错），实际 %v", boot.recallQueries)
	}
	if boot.recentUser != "bob" || len(bs.Memories) != 1 || bs.Memories[0].ID != "m9" {
		t.Fatalf("应回退到最近记忆，实际 user=%q memories=%+v", boot.recentUser, bs.Memories)
	}
}

// TestCreateBootstrapFailureDoesNotBlock 召回失败不阻塞建会话（best-effort）
func TestCreateBootstrapFailureDoesNotBlock(t *testing.T) {
	repo := newFakeRepo()
	u := &UseCase{repo: repo, boot: &fakeBoot{err: errors.New("milvus 超时")}}
	c, bs, err := u.Create(context.Background(), "", "出问题的召回", "")
	if err != nil || c == nil {
		t.Fatalf("召回失败不应阻塞建会话: %v", err)
	}
	if bs == nil || !bs.Skipped || !strings.Contains(bs.Err, "milvus") {
		t.Fatalf("应标记 skipped 并带失败原因: %+v", bs)
	}
}
