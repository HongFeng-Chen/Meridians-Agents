package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// summarizeLLM 只服务会话总结：system 必须是归档提示词
type summarizeLLM struct {
	fail  bool
	calls int
	last  string
}

func (s *summarizeLLM) Chat(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("不应走 Chat")
}

func (s *summarizeLLM) ChatTemp(_ context.Context, system, user string, _ float64) (string, error) {
	s.calls++
	s.last = user
	if s.fail {
		return "", errors.New("LLM 超时")
	}
	if !strings.Contains(system, "会话归档助手") {
		return "", errors.New("system prompt 不是会话归档模板")
	}
	return "## 概要\n- 用户咨询 Milvus 3.0 的检索增强\n## 关键事实\n- 生产环境用 Go + Milvus\n" +
		"## 结论与决定\n- 采用 Dense + BM25 混合检索\n## 待办\n- 补一份评测用例", nil
}

func (s *summarizeLLM) Model() string { return "summary-llm" }

func seedShort(t *testing.T, short *fakeShort, n int) {
	t.Helper()
	msgs := []Message{
		{Role: "user", Content: "Milvus 3.0 在检索性能上有什么增强？", At: time.Now().Add(-10 * time.Minute)},
		{Role: "assistant", Content: "新增 DiskANN 索引与稀疏检索。", At: time.Now().Add(-9 * time.Minute)},
		{Role: "user", Content: "我们生产用 Go，准备上混合检索。", At: time.Now().Add(-8 * time.Minute)},
		{Role: "assistant", Content: "建议 Dense + BM25，用 RRF 融合。", At: time.Now().Add(-7 * time.Minute)},
	}
	for i := 0; i < n && i < len(msgs); i++ {
		short.session["s1"] = append(short.session["s1"], msgs[i])
	}
}

// TestSummarizeConversation 总结写入长期记忆：kind=insight、标签会话总结、正文带标题
func TestSummarizeConversation(t *testing.T) {
	u, repo, short, vec, _, _ := newTestUseCase(t, false)
	llm := &summarizeLLM{}
	u.llm = llm
	seedShort(t, short, 4)

	res, err := u.SummarizeConversation(context.Background(), "s1", "alice", "Milvus 检索咨询")
	if err != nil {
		t.Fatalf("SummarizeConversation: %v", err)
	}
	if res.Skipped || res.MemoryID == "" {
		t.Fatalf("应写入记忆，实际 %+v", res)
	}
	if res.Messages != 4 {
		t.Fatalf("参与总结的消息数应为 4，实际 %d", res.Messages)
	}
	m := repo.mems[res.MemoryID]
	if m == nil {
		t.Fatal("记忆未落库")
	}
	if m.Kind != KindInsight || m.UserID != "alice" {
		t.Fatalf("kind/user 不符: %+v", m)
	}
	if len(m.Tags) != 1 || m.Tags[0] != "会话总结" {
		t.Fatalf("标签应为 [会话总结]，实际 %v", m.Tags)
	}
	// 写入时带上「上次结束的会话 ID」，记忆可反查原始对话
	if m.SessionID != "s1" {
		t.Fatalf("记忆应记录来源会话 s1，实际 %q", m.SessionID)
	}
	if !strings.Contains(m.Content, "Milvus 检索咨询") || !strings.Contains(m.Content, "## 结论与决定") {
		t.Fatalf("正文应带标题与总结结构，实际:\n%s", m.Content)
	}
	if len(vec.items) != 1 {
		t.Fatalf("应向量化 1 条，实际 %d", len(vec.items))
	}
	// prompt 里应带上标题与对话记录
	if !strings.Contains(llm.last, "Milvus 检索咨询") || !strings.Contains(llm.last, "对话记录") {
		t.Fatalf("user prompt 缺标题或对话记录: %s", llm.last)
	}
}

// TestSummarizeConversationTooShort 消息不足 2 条：跳过，不写库、不调 LLM
func TestSummarizeConversationTooShort(t *testing.T) {
	u, repo, short, _, _, _ := newTestUseCase(t, false)
	llm := &summarizeLLM{}
	u.llm = llm
	seedShort(t, short, 1)

	res, err := u.SummarizeConversation(context.Background(), "s1", "alice", "半句话")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !res.Skipped || res.MemoryID != "" {
		t.Fatalf("应跳过，实际 %+v", res)
	}
	if llm.calls != 0 || len(repo.mems) != 0 {
		t.Fatalf("跳过时不应调用 LLM 或写库（calls=%d, mems=%d）", llm.calls, len(repo.mems))
	}
}

// TestSummarizeConversationLLMFailure LLM 失败向上返回错误（调用方据此不标记已归档）
func TestSummarizeConversationLLMFailure(t *testing.T) {
	u, _, short, _, _, _ := newTestUseCase(t, false)
	u.llm = &summarizeLLM{fail: true}
	seedShort(t, short, 4)

	if _, err := u.SummarizeConversation(context.Background(), "s1", "alice", "标题"); err == nil {
		t.Fatal("LLM 失败应返回错误")
	}
}

// TestSummarizeConversationDedup 同一会话重复总结：内容一致时命中去重，不重复入库
func TestSummarizeConversationDedup(t *testing.T) {
	u, repo, short, _, _, _ := newTestUseCase(t, false)
	u.llm = &summarizeLLM{}
	seedShort(t, short, 4)

	first, err := u.SummarizeConversation(context.Background(), "s1", "alice", "标题")
	if err != nil {
		t.Fatalf("首次总结: %v", err)
	}
	second, err := u.SummarizeConversation(context.Background(), "s1", "alice", "标题")
	if err != nil {
		t.Fatalf("二次总结: %v", err)
	}
	if !second.Deduped || second.MemoryID != first.MemoryID {
		t.Fatalf("重复总结应命中去重，实际 %+v", second)
	}
	if len(repo.mems) != 1 {
		t.Fatalf("去重后应只有 1 条记忆，实际 %d", len(repo.mems))
	}
}
