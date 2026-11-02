package conversation

// 会话用例单测：fake repo 验证 ID 生成（会话 id = task id 透传）、消息追加校验、
// 目录预览刷新等核心规则。
import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRepo struct {
	convs  map[string]*Conversation
	msgs   map[string][]Message
	called []string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{convs: map[string]*Conversation{}, msgs: map[string][]Message{}}
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
	c, err := uc.Create(context.Background(), "42", "混排编排演示", "dag-demo")
	if err != nil {
		t.Fatalf("带任务 ID 创建失败: %v", err)
	}
	if c.ID != "42" || c.AgentKey != "dag-demo" {
		t.Fatalf("会话 ID/Agent 不符合预期: %+v", c)
	}
	// 空 ID 自动生成
	c2, err := uc.Create(context.Background(), "", "检索会话", "")
	if err != nil || !strings.HasPrefix(c2.ID, "c-") {
		t.Fatalf("自动生成会话 ID 异常: %v %+v", err, c2)
	}
}

func TestAppendValidation(t *testing.T) {
	r := newFakeRepo()
	uc := NewUseCase(r, nil)
	ctx := context.Background()
	c, _ := uc.Create(ctx, "", "测试", "")

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
	c, _ := uc.Create(ctx, "", "旧标题", "")
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
