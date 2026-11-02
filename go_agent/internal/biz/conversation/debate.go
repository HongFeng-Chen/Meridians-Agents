// 辩论会话编排：同一会话内多个自定义临时角色按人设自主发言、相互辩论，
// 用户仅作为监督者（叫停 / 补充引导 / 增减角色），全程消息落库渐进展示。
//
// 生命周期：
//
//	CreateDebate（status=running，启动 goroutine）
//	  → 每轮按角色顺序发言（发言前重读 DB：角色增减、叫停即时生效）
//	  → 结束判定（rounds 跑满轮次 / consensus 由 LLM 每轮后判定收敛）
//	  → summarize：主持人中立汇总各方观点输出综合结论（status=finished/stopped）
package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	debateMinRoles   = 2
	debateMaxRoles   = 6
	debateMaxRounds  = 12
	debateTranscript = 40 // 注入提示词的最近消息条数
	debateClip       = 600
)

// debateManager 会话内进行中的辩论（防同会话并发多场）
type debateManager struct {
	mu sync.Mutex
	// convIDs 有 goroutine 在跑的会话 ID
	convIDs map[string]struct{}
}

func newDebateManager() *debateManager {
	return &debateManager{convIDs: map[string]struct{}{}}
}

func (m *debateManager) claim(convID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.convIDs[convID]; ok {
		return false
	}
	m.convIDs[convID] = struct{}{}
	return true
}

func (m *debateManager) release(convID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.convIDs, convID)
}

// —— 对外用例 ——

// CreateDebate 创建辩论并启动后台推演
func (u *UseCase) CreateDebate(ctx context.Context, convID, topic string, roles []DebateRole, maxRounds int, endRule string) (*Debate, error) {
	if u.deps == nil || u.deps.LLM == nil {
		return nil, fmt.Errorf("辩论能力需要配置 LLM 数据源")
	}
	conv, err := u.Get(ctx, convID)
	if err != nil {
		return nil, err
	}
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return nil, fmt.Errorf("%w: 辩论议题不能为空", ErrInvalidConv)
	}
	if utf8.RuneCountInString(topic) > 2000 {
		return nil, fmt.Errorf("%w: 议题过长（≤2000 字）", ErrInvalidConv)
	}
	if len(roles) < debateMinRoles || len(roles) > debateMaxRoles {
		return nil, fmt.Errorf("%w: 辩论角色需 %d~%d 个", ErrInvalidConv, debateMinRoles, debateMaxRoles)
	}
	seen := map[string]struct{}{}
	for i := range roles {
		r := &roles[i]
		r.Name = strings.TrimSpace(r.Name)
		r.Persona = strings.TrimSpace(r.Persona)
		r.Stance = strings.TrimSpace(r.Stance)
		r.Style = strings.TrimSpace(r.Style)
		if r.Name == "" {
			return nil, fmt.Errorf("%w: 角色 %d 缺少名称", ErrInvalidConv, i+1)
		}
		if utf8.RuneCountInString(r.Name) > 20 {
			return nil, fmt.Errorf("%w: 角色名 %q 过长（≤20 字）", ErrInvalidConv, r.Name)
		}
		if _, dup := seen[r.Name]; dup {
			return nil, fmt.Errorf("%w: 角色名重复 %q", ErrInvalidConv, r.Name)
		}
		seen[r.Name] = struct{}{}
		if utf8.RuneCountInString(r.Persona) > 500 {
			r.Persona = string([]rune(r.Persona)[:500])
		}
		if utf8.RuneCountInString(r.Stance) > 200 {
			r.Stance = string([]rune(r.Stance)[:200])
		}
		if utf8.RuneCountInString(r.Style) > 200 {
			r.Style = string([]rune(r.Style)[:200])
		}
	}
	if maxRounds <= 0 {
		maxRounds = 4
	}
	if maxRounds > debateMaxRounds {
		maxRounds = debateMaxRounds
	}
	if endRule == "" {
		endRule = EndRuleRounds
	}
	if endRule != EndRuleRounds && endRule != EndRuleConsensus {
		return nil, fmt.Errorf("%w: 结束判定 %q 不支持（可选 rounds / consensus）", ErrInvalidConv, endRule)
	}

	if active, _ := u.repo.GetActiveDebate(ctx, conv.ID); active != nil {
		return nil, fmt.Errorf("%w: 会话已有进行中的辩论（#%d），请先等它结束或叫停", ErrInvalidConv, active.ID)
	}

	now := time.Now()
	d := &Debate{
		ConversationID: conv.ID,
		Topic:          topic,
		Status:         DebateRunning,
		MaxRounds:      maxRounds,
		EndRule:        endRule,
		Roles:          roles,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := u.repo.CreateDebate(ctx, d); err != nil {
		return nil, err
	}
	if !u.debates.claim(conv.ID) {
		return nil, fmt.Errorf("%w: 会话辩论已在推演中", ErrInvalidConv)
	}
	go u.runDebate(d.ID, conv.ID)
	return d, nil
}

// CurrentDebate 会话当前 / 最近一场辩论
func (u *UseCase) CurrentDebate(ctx context.Context, convID string) (*Debate, error) {
	if _, err := u.Get(ctx, convID); err != nil {
		return nil, err
	}
	d, err := u.repo.GetActiveDebate(ctx, convID)
	if err != nil {
		return nil, err
	}
	if d != nil {
		return d, nil
	}
	return u.repo.GetLatestDebate(ctx, convID)
}

// PauseDebate 暂停辩论：当前发言结束后挂起（不再开始下一次发言），可恢复
func (u *UseCase) PauseDebate(ctx context.Context, convID string) (*Debate, error) {
	d, err := u.repo.GetActiveDebate(ctx, convID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: 会话没有进行中的辩论", ErrInvalidConv)
	}
	if d.Status == DebateRunning {
		d.Status = DebatePaused
		if err := u.repo.UpdateDebate(ctx, d); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// ResumeDebate 恢复已暂停的辩论，从暂停处继续推演
func (u *UseCase) ResumeDebate(ctx context.Context, convID string) (*Debate, error) {
	d, err := u.repo.GetActiveDebate(ctx, convID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: 会话没有进行中的辩论", ErrInvalidConv)
	}
	if d.Status == DebatePaused {
		d.Status = DebateRunning
		if err := u.repo.UpdateDebate(ctx, d); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// StopDebate 请求停止辩论（当前发言结束后收尾汇总）
func (u *UseCase) StopDebate(ctx context.Context, convID string) (*Debate, error) {
	d, err := u.repo.GetActiveDebate(ctx, convID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: 会话没有进行中的辩论", ErrInvalidConv)
	}
	if d.Status == DebateRunning || d.Status == DebatePaused {
		d.Status = DebateStopping
		if err := u.repo.UpdateDebate(ctx, d); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// AddDebateRole 辩论进行中追加角色（下一轮生效）
func (u *UseCase) AddDebateRole(ctx context.Context, convID string, r DebateRole) (*Debate, error) {
	d, err := u.repo.GetActiveDebate(ctx, convID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: 会话没有进行中的辩论", ErrInvalidConv)
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return nil, fmt.Errorf("%w: 角色名必填", ErrInvalidConv)
	}
	for _, exist := range d.Roles {
		if exist.Name == r.Name {
			return nil, fmt.Errorf("%w: 角色名 %q 已存在", ErrInvalidConv, r.Name)
		}
	}
	if len(d.Roles) >= debateMaxRoles {
		return nil, fmt.Errorf("%w: 角色数已达上限 %d", ErrInvalidConv, debateMaxRoles)
	}
	d.Roles = append(d.Roles, r)
	if err := u.repo.UpdateDebate(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// RemoveDebateRole 辩论进行中移除角色（下一轮生效，至少保留 2 个）
func (u *UseCase) RemoveDebateRole(ctx context.Context, convID, name string) (*Debate, error) {
	d, err := u.repo.GetActiveDebate(ctx, convID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: 会话没有进行中的辩论", ErrInvalidConv)
	}
	next := d.Roles[:0]
	found := false
	for _, exist := range d.Roles {
		if exist.Name == name {
			found = true
			continue
		}
		next = append(next, exist)
	}
	if !found {
		return nil, fmt.Errorf("%w: 角色 %q 不存在", ErrInvalidConv, name)
	}
	if len(next) < debateMinRoles {
		return nil, fmt.Errorf("%w: 至少保留 %d 个角色", ErrInvalidConv, debateMinRoles)
	}
	d.Roles = append([]DebateRole{}, next...)
	if err := u.repo.UpdateDebate(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// —— 后台推演 ——

// runDebate 辩论主循环（goroutine 内执行；每步重读 DB，叫停 / 增减角色即时生效）
func (u *UseCase) runDebate(debateID int64, convID string) {
	defer u.debates.release(convID)
	ctx := context.Background()
	d, err := u.repo.GetDebate(ctx, debateID)
	if err != nil {
		return
	}

	stopped := false
roundLoop:
	for round := 1; round <= d.MaxRounds; round++ {
		cur, err := u.repo.GetDebate(ctx, debateID)
		if err != nil {
			return
		}
		if cur.Status == DebateStopping {
			stopped = true
			break
		}
		// 暂停期间挂起等待（恢复 / 叫停 / 进程重启退出）
		if u.waitWhilePaused(ctx, debateID) {
			stopped = true
			break
		}
		// 等待结束后重读：cur 是暂停前读的，直接写回会把过期状态覆盖恢复结果
		cur, err = u.repo.GetDebate(ctx, debateID)
		if err != nil {
			return
		}
		cur.Round = round
		_ = u.repo.UpdateDebate(ctx, cur)

		for _, role := range cur.Roles {
			// 发言间检查叫停与角色变动（重读 DB）
			fresh, err := u.repo.GetDebate(ctx, debateID)
			if err != nil {
				return
			}
			if fresh.Status == DebateStopping {
				stopped = true
				break roundLoop
			}
			// 每次发言前同样响应暂停：当前发言完成后挂起
			if u.waitWhilePaused(ctx, debateID) {
				stopped = true
				break roundLoop
			}
			// 找当前角色（可能在上一条发言后被移除）
			var target *DebateRole
			for i := range fresh.Roles {
				if fresh.Roles[i].Name == role.Name {
					target = &fresh.Roles[i]
					break
				}
			}
			if target == nil {
				continue
			}
			speech := u.debateSpeak(ctx, fresh, round, *target)
			meta := map[string]any{"op": "debate", "debate_id": debateID, "round": round}
			mb, _ := json.Marshal(meta)
			if _, err := u.AppendAs(ctx, convID, RoleAssistant, role.Name, speech, string(mb), 0); err != nil {
				return
			}
		}

		// 共识收敛判定
		if cur.EndRule == EndRuleConsensus {
			if end, reason := u.debateConverged(ctx, debateID); end {
				_, _ = u.AppendAs(ctx, convID, RoleAssistant, "主持人",
					fmt.Sprintf("各方观点已充分交锋（判定：%s），辩论提前结束，下面输出综合结论。", reason),
					"", 0)
				break
			}
		}
	}

	u.debateSummarize(ctx, debateID, convID, stopped)
}

// waitWhilePaused 暂停期间阻塞轮询，直到恢复或叫停；返回 true 表示已叫停。
// 每 1.2s 重读一次 DB 状态，进程重启则随 goroutine 一起退出。
func (u *UseCase) waitWhilePaused(ctx context.Context, debateID int64) bool {
	for {
		d, err := u.repo.GetDebate(ctx, debateID)
		if err != nil {
			return false
		}
		switch d.Status {
		case DebateStopping:
			return true
		case DebatePaused:
			time.Sleep(1200 * time.Millisecond)
		default:
			return false
		}
	}
}

// debateSpeak 生成一个角色的一次发言
func (u *UseCase) debateSpeak(ctx context.Context, d *Debate, round int, role DebateRole) string {
	system := fmt.Sprintf(
		"你是辩论会中的角色「%s」。\n人设：%s\n立场：%s\n发言风格：%s\n\n要求：\n1. 始终保持角色人设与立场，观点可以基于他人发言适当演化，但不轻易叛变；\n2. 回应其他角色（尤其是与你对立的）的观点，指出漏洞或给出论据；\n3. 每次发言 100~300 字，直接输出发言正文，不要输出角色名前缀、不要输出 JSON；\n4. 用户的发言是监督引导，可以回应但立场不受其强迫。",
		role.Name, role.personaOr("经验丰富的专业人士"), role.stanceOr("基于自身人设形成"), role.styleOr("严谨自然"),
	)
	user := fmt.Sprintf("## 议题\n%s\n\n## 最近对话（[角色名] 发言内容）\n%s\n\n现在是第 %d 轮，请以「%s」的身份输出本次发言：",
		d.Topic, u.debateTranscript(ctx, d.ConversationID), round, role.Name)
	out, err := u.deps.LLM.Chat(ctx, system, user)
	if err != nil {
		return fmt.Sprintf("（%s 本次发言生成失败：%v）", role.Name, err)
	}
	return strings.TrimSpace(out)
}

// debateConverged 共识收敛判定：返回 (是否提前结束, 原因)
func (u *UseCase) debateConverged(ctx context.Context, debateID int64) (bool, string) {
	d, err := u.repo.GetDebate(ctx, debateID)
	if err != nil {
		return false, ""
	}
	system := "你是辩论会的中立裁判。判断当前各方观点是否已经充分交锋且没有新增论点（达成共识或进入重复循环）。输出严格 JSON：{\"end\": true/false, \"reason\": \"一句话原因\"}"
	user := fmt.Sprintf("## 议题\n%s\n\n## 最近对话\n%s", d.Topic, u.debateTranscript(ctx, d.ConversationID))
	out, err := u.deps.LLM.Chat(ctx, system, user)
	if err != nil {
		return false, ""
	}
	var res struct {
		End    bool   `json:"end"`
		Reason string `json:"reason"`
	}
	raw, err := extractJSON(out)
	if err != nil {
		return false, ""
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return false, ""
	}
	if res.Reason == "" {
		res.Reason = "裁判判定可提前结束"
	}
	return res.End, res.Reason
}

// debateSummarize 主持人中立汇总，输出综合结论并结束辩论
func (u *UseCase) debateSummarize(ctx context.Context, debateID int64, convID string, stopped bool) {
	d, err := u.repo.GetDebate(ctx, debateID)
	if err != nil {
		return
	}
	system := "你是辩论会的中立主持人。请完整汇总各方观点与核心分歧，不做立场偏袒，最后给出中立的综合结论（可包含各立场各自成立的前提与建议的后续验证方向）。500 字以内，直接输出正文。"
	user := fmt.Sprintf("## 议题\n%s\n\n## 辩论过程\n%s\n\n%s\n请输出中立综合结论。",
		d.Topic, u.debateTranscriptAll(ctx, convID), map[bool]string{true: "辩论被用户中途叫停，请汇总到叫停为止的进展。", false: "辩论已按规则结束。"}[stopped])
	out, err := u.deps.LLM.Chat(ctx, system, user)
	answer := strings.TrimSpace(out)
	if err != nil {
		answer = fmt.Sprintf("汇总生成失败：%v", err)
	}
	status := map[bool]string{true: DebateStopped, false: DebateFinished}[stopped]
	meta := map[string]any{"op": "debate_summary", "debate_id": debateID, "status": status}
	mb, _ := json.Marshal(meta)
	_, _ = u.AppendAs(ctx, convID, RoleAssistant, "主持人", answer, string(mb), 0)

	now := time.Now()
	d.Status = status
	d.FinishedAt = &now
	_ = u.repo.UpdateDebate(ctx, d)
}

// —— 提示词材料 ——

// debateTranscript 最近对话（限制条数），角色消息带名字，用户消息标注监督者
func (u *UseCase) debateTranscript(ctx context.Context, convID string) string {
	return u.transcript(ctx, convID, debateTranscript)
}

// debateTranscriptAll 全部对话（汇总用）
func (u *UseCase) debateTranscriptAll(ctx context.Context, convID string) string {
	return u.transcript(ctx, convID, 0)
}

func (u *UseCase) transcript(ctx context.Context, convID string, limit int) string {
	msgs, err := u.repo.ListMessages(ctx, convID)
	if err != nil {
		return "（无）"
	}
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	var b strings.Builder
	for _, m := range msgs {
		name := m.Speaker
		if name == "" {
			if m.Role == RoleUser {
				name = "用户（监督者）"
			} else {
				name = "助手"
			}
		}
		fmt.Fprintf(&b, "[%s] %s\n", name, clipRunes(m.Content, debateClip))
	}
	if b.Len() == 0 {
		return "（无）"
	}
	return strings.TrimSpace(b.String())
}

// —— 小工具 ——

func (r DebateRole) personaOr(def string) string {
	if strings.TrimSpace(r.Persona) == "" {
		return def
	}
	return r.Persona
}

func (r DebateRole) stanceOr(def string) string {
	if strings.TrimSpace(r.Stance) == "" {
		return def
	}
	return r.Stance
}

func (r DebateRole) styleOr(def string) string {
	if strings.TrimSpace(r.Style) == "" {
		return def
	}
	return r.Style
}

// extractJSON 从模型输出中提取第一个平衡的 JSON 对象原文
func extractJSON(text string) (string, error) {
	start := strings.Index(text, "{")
	if start < 0 {
		return "", fmt.Errorf("输出中没有 JSON 对象")
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if esc {
			esc = false
			continue
		}
		switch {
		case inStr && ch == '\\':
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return text[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("JSON 对象不完整")
}
