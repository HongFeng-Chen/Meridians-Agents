// Package conversation 会话域用例：目录 CRUD + 消息追加 + LLM 对话编排（chat.go）。
//
// Chat 由 LLM 自主决定是否调用 Agent / MCP 工具，并自动召回
// Memory / RAG / GraphRAG 上下文；meta JSON 保存链路索引，
// 任务明细看 agentfactory 的 GetTask，操作历史看 /api/histories。
package conversation

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"
	"unicode/utf8"

	"go_agent/internal/biz/memory"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/mediaauth"

	"github.com/go-kratos/kratos/v3/log"
)

// Err 会话域错误
var (
	ErrNotFound    = errors.New("会话不存在")
	ErrInvalidConv = errors.New("会话参数不合法")
)

// Repo 会话仓储
type Repo interface {
	// Create 新建会话（ID 由用例生成后传入）
	Create(ctx context.Context, c *Conversation) error
	// List 会话目录（updated_at 倒序）
	List(ctx context.Context, f Filter) ([]Conversation, int64, error)
	// Get 会话详情
	Get(ctx context.Context, id string) (*Conversation, error)
	// Update 更新标题 / Agent
	Update(ctx context.Context, c *Conversation) error
	// Delete 删除会话（消息级联删除）
	Delete(ctx context.Context, id string) error
	// AppendMessage 追加消息并刷新会话 updated_at / last_message / 计数
	AppendMessage(ctx context.Context, m *Message) error
	// ListMessages 会话消息（id 正序）
	ListMessages(ctx context.Context, conversationID string) ([]Message, error)
	// ListMessagesPaged 会话消息分页（微信式向上翻页）：id 正序一页 + 是否还有更早消息
	ListMessagesPaged(ctx context.Context, conversationID string, limit int, beforeID int64) ([]Message, bool, error)

	// ListUnsynced 归属用户最近未归档进记忆系统的会话（updated_at 倒序，消息数 ≥ 2）
	ListUnsynced(ctx context.Context, userID, excludeID string, limit int) ([]Conversation, error)
	// MarkMemorySynced 标记会话已归档进记忆系统（成功写入后调用，避免重复总结）
	MarkMemorySynced(ctx context.Context, id string, at time.Time) error

	// CreateDebate 新建辩论
	CreateDebate(ctx context.Context, d *Debate) error
	// GetDebate 按 ID 取辩论
	GetDebate(ctx context.Context, id int64) (*Debate, error)
	// GetActiveDebate 会话内进行中的辩论（running/stopping），没有返回 nil
	GetActiveDebate(ctx context.Context, conversationID string) (*Debate, error)
	// GetLatestDebate 会话最近一场辩论（含已结束），没有返回 nil
	GetLatestDebate(ctx context.Context, conversationID string) (*Debate, error)
	// UpdateDebate 更新辩论状态 / 轮次 / 角色
	UpdateDebate(ctx context.Context, d *Debate) error
	// StopOrphanDebates 把所有活跃状态（running/paused/stopping）的辩论标记为 stopped。
	// 启动恢复用：进程重启后推演 goroutine 不复存在，孤儿辩论若不收尾会永久占用会话。
	StopOrphanDebates(ctx context.Context) (int64, error)
}

// MemoryArchiver 会话归档端口：把一次会话总结成长期记忆。
// 由 memory.UseCase 实现；会话域只依赖这个窄接口，不反向依赖记忆实现细节。
type MemoryArchiver interface {
	SummarizeConversation(ctx context.Context, sessionID, userID, title string) (*memory.SummarizeResult, error)
}

// MemoryBootstrapper 新会话启动召回端口：只读长期记忆，不读会话消息。
// 由 memory.UseCase 实现（Recall 语义召回 / Recent 最近沉淀兜底）。
type MemoryBootstrapper interface {
	Recall(ctx context.Context, query, userID string, topK int, withGraph bool, source string) (*memory.RecallResult, error)
	Recent(ctx context.Context, userID string, limit int) ([]*memory.LongTermMemory, error)
}

// UseCase 会话用例
type UseCase struct {
	repo    Repo
	deps    *ChatDeps // 聊天编排依赖（见 chat.go）
	debates *debateManager
	// archiver 会话归档器（可为 nil：记忆系统未装配时不归档）
	archiver MemoryArchiver
	// boot 新会话启动召回（可为 nil：记忆系统未装配时返回 skipped）
	boot MemoryBootstrapper
}

// NewUseCase 创建会话用例
func NewUseCase(r Repo, d *ChatDeps) *UseCase {
	u := &UseCase{repo: r, deps: d, debates: newDebateManager()}
	if d != nil && d.Memory != nil {
		u.archiver = d.Memory
		u.boot = d.Memory
	}
	// 启动恢复：进程重启后辩论 goroutine 不会复活，把孤儿辩论收尾为已叫停，
	// 否则会话被永久占用、无法开新辩论。
	if n, err := r.StopOrphanDebates(context.Background()); err == nil && n > 0 {
		fmt.Printf("[conversation] 启动恢复：收尾 %d 场孤儿辩论（上次进程重启时仍在推演）\n", n)
	}
	return u
}

// normalizeInput 清洗会话基础字段
func normalizeInput(title, agentKey string) (string, string, error) {
	title = strings.TrimSpace(title)
	agentKey = strings.TrimSpace(strings.ToLower(agentKey))
	if utf8.RuneCountInString(title) > 128 {
		return "", "", fmt.Errorf("%w: 标题过长（≤128 字）", ErrInvalidConv)
	}
	if utf8.RuneCountInString(agentKey) > 64 {
		return "", "", fmt.Errorf("%w: agent_key 过长", ErrInvalidConv)
	}
	return title, agentKey, nil
}

// genID 生成会话 ID：c-<纳秒时间戳36进制>-<随机后缀>
func genID() string {
	return fmt.Sprintf("c-%s-%04d", fmt.Sprintf("%x", time.Now().UnixNano()), rand.Intn(10000))
}

// Create 新建会话。id 可传任务 ID（会话 id = task id）；空则自动生成。
//
// 新会话 = 上一轮工作的边界，这里按固定顺序做两件事：
//  1. 先召回长期记忆（bootstrap，同步、有超时、best-effort）：
//     只查长期记忆，**不召回任何历史会话消息**，避免旧话题细节污染新话题。
//  2. 再把上次还没归档的会话总结成一条长期记忆（异步，写记忆时带上次会话 ID）。
//
// 顺序不能反：先召回到的是「上次会话已沉淀的总结」，而不是把旧会话正文塞进新会话。
func (u *UseCase) Create(ctx context.Context, id, title, agentKey string) (*Conversation, *SessionBootstrap, error) {
	title, agentKey, err := normalizeInput(title, agentKey)
	if err != nil {
		return nil, nil, err
	}
	id = strings.TrimSpace(id)
	if id != "" && utf8.RuneCountInString(id) > 64 {
		return nil, nil, fmt.Errorf("%w: 会话 ID 过长", ErrInvalidConv)
	}
	if id == "" {
		id = genID()
	}
	now := time.Now()
	// 归属用户来自请求身份（认证未启用时为 default）：
	// 会话中心列表与详情都以该字段做隔离，避免跨用户读会话。
	c := &Conversation{
		ID:        id,
		UserID:    identity.UserID(ctx),
		Title:     title,
		AgentKey:  agentKey,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := u.repo.Create(ctx, c); err != nil {
		return nil, nil, err
	}
	// 1) 先召回长期记忆（只有记忆，没有历史会话消息）
	bootstrap := u.bootstrapMemory(ctx, c.UserID, c.Title)
	// 2) 再把上一次结束的会话归纳进长期记忆（写入时带该会话 ID）。
	// 失败只记日志、不标记，下次开新会话时会重试。
	if u.archiver != nil {
		go u.archivePreviousConversations(ctx, c.UserID, c.ID)
	}
	return c, bootstrap, nil
}

const (
	bootstrapTopK   = 5                       // 启动召回条数
	bootstrapMinSc  = 0.15                    // 与聊天召回同一条绝对分数线
	bootstrapBudget = 1500 * time.Millisecond // 建会话不能被召回拖住
)

// bootstrapMemory 新会话启动召回：只查长期记忆，绝不读历史会话消息。
//
// 标题作为语义 query；标题为空（用户直接点「新对话」）没有可用 query，
// 退化为「最近沉淀的 N 条记忆」，避免空查询报错、也保证新会话不是全盲起步。
func (u *UseCase) bootstrapMemory(parent context.Context, userID, title string) *SessionBootstrap {
	bs := &SessionBootstrap{Source: BootstrapSourceMemory}
	if u.boot == nil {
		bs.Skipped = true
		bs.Err = "记忆系统未装配"
		return bs
	}
	ctx, cancel := context.WithTimeout(parent, bootstrapBudget)
	defer cancel()

	var mems []memory.RecalledMemory
	if q := strings.TrimSpace(title); q != "" {
		res, err := u.boot.Recall(ctx, q, userID, bootstrapTopK, false, memory.RecallSourceSessionStart)
		if err != nil {
			bs.Skipped = true
			bs.Err = err.Error()
			return bs
		}
		for _, m := range res.Memories {
			if float64(m.Score) >= bootstrapMinSc {
				mems = append(mems, m)
			}
		}
	} else {
		recent, err := u.boot.Recent(ctx, userID, bootstrapTopK)
		if err != nil {
			bs.Skipped = true
			bs.Err = err.Error()
			return bs
		}
		for _, m := range recent {
			mems = append(mems, memory.RecalledMemory{LongTermMemory: m})
		}
	}
	bs.Memories = mems
	if len(mems) > 0 {
		lines := make([]string, 0, len(mems))
		for _, m := range mems {
			if m.LongTermMemory == nil {
				continue
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s", m.Kind, clipRunes(m.Content, 300)))
		}
		if len(lines) > 0 {
			bs.Block = "### 长期记忆（新会话启动召回）\n" + strings.Join(lines, "\n")
		}
	}
	log.Info("conversation: 新会话启动召回完成", "conv", title, "memories", len(mems))
	return bs
}

// archivePreviousConversations 把归属用户最近未归档的会话总结进长期记忆（best-effort）。
//
// 选「上次」而不是全量：优先最新的未归档会话，最多顺带补 3 条历史欠账，
// 避免一次开新会话触发大量 LLM 调用。上下文与请求解耦（WithoutCancel），
// 请求返回后仍能跑完；失败不标记 MemorySyncedAt，留待下次重试。
func (u *UseCase) archivePreviousConversations(parent context.Context, userID, excludeID string) {
	if u.archiver == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Minute)
	defer cancel()

	convs, err := u.repo.ListUnsynced(ctx, userID, excludeID, 3)
	if err != nil {
		log.Warn("conversation: 查询未归档会话失败", "err", err)
		return
	}
	for _, c := range convs {
		res, err := u.archiver.SummarizeConversation(ctx, c.ID, userID, c.Title)
		if err != nil {
			log.Warn("conversation: 会话归档失败（下次开新会话时重试）", "conv", c.ID, "err", err)
			continue
		}
		// 消息不足 / 总结为空也算处理完，标记避免反复调用 LLM
		if err := u.repo.MarkMemorySynced(ctx, c.ID, time.Now()); err != nil {
			log.Warn("conversation: 标记会话已归档失败", "conv", c.ID, "err", err)
			continue
		}
		log.Info("conversation: 会话已归档进记忆系统", "conv", c.ID,
			"messages", res.Messages, "skipped", res.Skipped, "memory", res.MemoryID)
	}
}

// List 会话目录
func (u *UseCase) List(ctx context.Context, f Filter) ([]Conversation, int64, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	return u.repo.List(ctx, f)
}

// Get 会话详情（不含消息）
func (u *UseCase) Get(ctx context.Context, id string) (*Conversation, error) {
	c, err := u.repo.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	return c, nil
}

// Detail 会话 + 全部消息
func (u *UseCase) Detail(ctx context.Context, id string) (*Conversation, []Message, error) {
	c, err := u.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	msgs, err := u.repo.ListMessages(ctx, c.ID)
	if err != nil {
		return nil, nil, err
	}
	signAttachments(msgs)
	return c, msgs, nil
}

// ListMessagesPage 会话消息分页（微信式向上翻页）：
// limit<=0 时默认 20；beforeID>0 时取更早一页。返回 id 正序消息 + 是否还有更早消息。
func (u *UseCase) ListMessagesPage(ctx context.Context, id string, limit uint32, beforeID int64) ([]Message, bool, error) {
	c, err := u.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	msgs, more, err := u.repo.ListMessagesPaged(ctx, c.ID, int(limit), beforeID)
	if err != nil {
		return nil, false, err
	}
	signAttachments(msgs)
	return msgs, more, nil
}

// Rename 重命名会话
func (u *UseCase) Rename(ctx context.Context, id, title string) (*Conversation, error) {
	id = strings.TrimSpace(id)
	title, _, err := normalizeInput(title, "")
	if err != nil {
		return nil, err
	}
	c, err := u.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	c.Title = title
	if err := u.repo.Update(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Delete 删除会话
func (u *UseCase) Delete(ctx context.Context, id string) error {
	return u.repo.Delete(ctx, strings.TrimSpace(id))
}

// Append 追加消息。校验角色与长度，成功后刷新会话目录预览。
func (u *UseCase) Append(ctx context.Context, conversationID, role, content, meta string, taskID uint64) (*Message, error) {
	return u.AppendAs(ctx, conversationID, role, "", content, meta, taskID)
}

// AppendAs 追加消息（带发言者标识：辩论角色 / 主持人）
func (u *UseCase) AppendAs(ctx context.Context, conversationID, role, speaker, content, meta string, taskID uint64) (*Message, error) {
	return u.appendFull(ctx, conversationID, role, speaker, content, meta, taskID, nil)
}

// AppendWithAttachments 追加带附件的消息（图片 / 视频聊天）。
// 附件需先经 SaveMedia 落盘拿到 URL；附件上限 9 个。
func (u *UseCase) AppendWithAttachments(ctx context.Context, conversationID, role, content, meta string, taskID uint64, atts []Attachment) (*Message, error) {
	return u.appendFull(ctx, conversationID, role, "", content, meta, taskID, atts)
}

func (u *UseCase) appendFull(ctx context.Context, conversationID, role, speaker, content, meta string, taskID uint64, atts []Attachment) (*Message, error) {
	conversationID = strings.TrimSpace(conversationID)
	role = strings.TrimSpace(strings.ToLower(role))
	speaker = strings.TrimSpace(speaker)
	content = strings.TrimSpace(content)
	if conversationID == "" {
		return nil, fmt.Errorf("%w: 会话 ID 必填", ErrInvalidConv)
	}
	valid := false
	for _, r := range ValidRoles {
		if role == r {
			valid = true
			break
		}
	}
	if !valid {
		return nil, fmt.Errorf("%w: 消息角色 %q 不支持（可选 user / assistant）", ErrInvalidConv, role)
	}
	if content == "" && len(atts) == 0 {
		return nil, fmt.Errorf("%w: 消息内容不能为空", ErrInvalidConv)
	}
	if len(atts) > MaxAttachments {
		return nil, fmt.Errorf("%w: 附件最多 %d 个", ErrInvalidConv, MaxAttachments)
	}
	if utf8.RuneCountInString(content) > 20000 {
		return nil, fmt.Errorf("%w: 消息内容过长（≤20000 字）", ErrInvalidConv)
	}
	if utf8.RuneCountInString(meta) > 8000 {
		meta = string([]rune(meta)[:8000])
	}
	if utf8.RuneCountInString(speaker) > 64 {
		speaker = string([]rune(speaker)[:64])
	}
	if _, err := u.Get(ctx, conversationID); err != nil {
		return nil, err
	}
	m := &Message{
		ConversationID: conversationID,
		Role:           role,
		Speaker:        speaker,
		Content:        content,
		Meta:           strings.TrimSpace(meta),
		TaskID:         taskID,
		Attachments:    atts,
		CreatedAt:      time.Now(),
	}
	if err := u.repo.AppendMessage(ctx, m); err != nil {
		return nil, err
	}
	// 返回给调用方的这份要带签名（存储里保持裸地址，见 signAttachments）
	signAttachments([]Message{*m})
	return m, nil
}

// signAttachments 给附件地址补访问签名。
//
// 存储里始终是裸地址 /api/files/{bucket}/{name}：签名随密钥变化，
// 落库的话换密钥/重启（随机密钥）就全失效，还得做数据迁移。
// 读的时候现签，历史消息不用动，签名也永远是新密钥算出来的。
func signAttachments(msgs []Message) {
	for i := range msgs {
		for j := range msgs[i].Attachments {
			msgs[i].Attachments[j].URL = mediaauth.Sign(msgs[i].Attachments[j].URL)
		}
	}
}
