package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-kratos/kratos/v3/log"
)

// 会话归档：把一段已结束的对话总结归纳成一条长期记忆。
//
// 场景：用户开新会话时，上一条还没归档进记忆系统的会话由会话域触发本方法，
// 总结后以 kind=insight 写入长期记忆（Remember 负责向量化 + 图谱抽取 + 内容去重）。
// 只写一条总结，不逐条抽事实：一次调用、成本可控、召回时是一段完整上下文。

// SummarizeResult 会话总结归档结果
type SummarizeResult struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	// Messages 参与总结的消息条数（0/1 = 消息太少，跳过）
	Messages int `json:"messages"`
	// Skipped true = 消息不足或总结为空，未写入记忆
	Skipped bool `json:"skipped"`
	// MemoryID 写入（或命中去重）的记忆 ID
	MemoryID string `json:"memory_id"`
	// Deduped true = 同内容已存在，复用旧记忆
	Deduped bool `json:"deduped"`
	// Summary 生成的总结正文
	Summary string `json:"summary"`
}

const summarizeSystem = `你是会话归档助手。把一段用户与智能体平台的对话总结成一条可供以后召回的长期记忆。

要求：
1. 只依据对话内容，不编造、不推测；对话里没有的信息不要补。
2. 输出 Markdown，固定四节，每节 1~5 条要点，没有内容写「无」：
## 概要
## 关键事实
## 结论与决定
## 待办
3. 全文不超过 600 字；要点用短句，避免复述寒暄与过程性废话。
4. 不要输出任何解释、前后缀或代码块标记。`

// SummarizeConversation 总结一次会话并写入长期记忆。
//
// 消息来源复用 ExtractMemories 的读取顺序（平台会话优先，短期记忆兜底）。
// 消息少于 2 条视为没有可总结内容，返回 Skipped 而不写库，由调用方决定是否标记已归档。
func (u *UseCase) SummarizeConversation(ctx context.Context, sessionID, userID, title string) (*SummarizeResult, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("memory: 会话 ID 不能为空")
	}
	if userID == "" {
		userID = "default"
	}
	title = strings.TrimSpace(title)
	res := &SummarizeResult{SessionID: sessionID, Title: title}

	// 会话归档取全量上限（200 条）；消息渲染时单条已截断，避免超长上下文
	msgs, err := u.extractMessages(ctx, sessionID, userID, 200)
	if err != nil {
		return nil, err
	}
	res.Messages = len(msgs)
	if len(msgs) < 2 {
		res.Skipped = true
		return res, nil
	}

	user := fmt.Sprintf("## 会话标题\n%s\n\n## 对话记录\n%s", nonEmpty(title, sessionID), renderTranscript(msgs))
	raw, err := u.llm.ChatTemp(ctx, summarizeSystem, user, 0.3)
	if err != nil {
		return nil, fmt.Errorf("memory: 会话总结 LLM 调用失败: %w", err)
	}
	summary := strings.TrimSpace(raw)
	if summary == "" || summary == "无" {
		res.Skipped = true
		return res, nil
	}
	summary = clipText(summary, 2000)
	res.Summary = summary

	// 记忆正文带上标题与时间，脱离会话也能看懂；同一会话重复归档会被 checksum 去重
	content := fmt.Sprintf("会话《%s》总结\n\n%s", clipText(nonEmpty(title, sessionID), 60), summary)
	if !msgs[len(msgs)-1].At.IsZero() {
		content = fmt.Sprintf("会话《%s》总结（%s）\n\n%s",
			clipText(nonEmpty(title, sessionID), 60),
			msgs[len(msgs)-1].At.Format("2006-01-02"),
			summary)
	}

	w, err := u.Remember(ctx, RememberInput{
		UserID:     userID,
		Kind:       KindInsight,
		Content:    content,
		Importance: 4,
		Tags:       []string{"会话总结"},
		Source:     "会话归档 · " + clipText(nonEmpty(title, sessionID), 40),
		// 把上次结束的会话 ID 写进记忆：可从记忆反查原始对话，也便于按会话清理
		SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: 会话总结写入记忆失败: %w", err)
	}
	res.MemoryID = w.Memory.ID
	res.Deduped = w.Deduped
	log.Info("memory: 会话已总结归档", "session", sessionID, "messages", res.Messages,
		"memory", res.MemoryID, "deduped", res.Deduped)
	return res, nil
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
