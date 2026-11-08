package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v3/log"
)

// ---------- 内置规则（参考阿里云百炼的记忆规则模型） ----------

// builtinRules 内置三种记忆规则；种子在 New 启动时幂等写入（已有内置规则则跳过）。
// 指令会拼进提取提示词，告诉 LLM 从会话中筛选「值得长期记住」的内容。
func builtinRules() []*MemoryRule {
	return []*MemoryRule{
		{
			Name: "事实记忆规则",
			Type: RuleTypeFact,
			Kind: KindFact,
			Instruction: "提取用户在对话中明确陈述的客观事实，包括：个人背景（姓名、职业、公司、所在团队）、" +
				"正在进行的项目与技术栈、明确表达的偏好与决定（例如「我们生产环境用 K8s」「以后都用中文回复我」）、" +
				"重要承诺与待办。只提取用户说过的内容，不要推断；每条事实必须自包含（脱离对话也能看懂），一句话一条。",
		},
		{
			Name: "用户画像规则",
			Type: RuleTypeProfile,
			Kind: KindProfile,
			Instruction: "从对话中归纳能刻画用户画像的信息，包括：技术角色与水平（例如「资深 Go 后端」「初学前端」）、" +
				"沟通与工作偏好（喜欢简洁答案 / 要详细解释、偏好代码示例）、目标与动机（在学习什么、想达成什么）。" +
				"只依据对话中明确体现的内容归纳，每条画像一个维度，措辞为第三人称（例如「用户偏好…」）。",
		},
		{
			Name: "技能记忆规则",
			Type: RuleTypeSkill,
			Kind: KindSkill,
			Instruction: "提取用户教授或演示的技能、工作流程与操作方法，即「以后遇到类似任务应该怎么做」的可复用经验。" +
				"例如：部署某个服务的固定步骤、处理某类问题的套路、特定工具的用法约定。" +
				"每条技能描述为操作指南形式（先…再…最后…），只提取用户明确交代过的方法，不要凭常识补全。",
		},
	}
}

// seedBuiltinRules 幂等写入内置规则（库中没有任何内置规则时才种入）
func seedBuiltinRules(ctx context.Context, repo LongTermRepo) {
	n, err := repo.CountBuiltinRules(ctx)
	if err != nil {
		log.Warn("memory: 内置规则种子检查失败", "err", err)
		return
	}
	if n > 0 {
		return
	}
	now := time.Now()
	for _, r := range builtinRules() {
		r.Builtin = true
		r.Enabled = true
		r.CreatedAt, r.UpdatedAt = now, now
		if err := repo.SaveRule(ctx, r); err != nil {
			log.Warn("memory: 写入内置规则失败", "name", r.Name, "err", err)
		}
	}
	log.Info("memory: 内置记忆规则已种入（事实/画像/技能）")
}

// ---------- 规则 CRUD ----------

// ListRules 规则列表
func (u *UseCase) ListRules(ctx context.Context) ([]*MemoryRule, error) {
	return u.repo.ListRules(ctx)
}

// ListRecallLogs 检索历史（时间倒序）
func (u *UseCase) ListRecallLogs(ctx context.Context, userID string, limit int) ([]*RecallLog, error) {
	return u.repo.ListRecallLogs(ctx, userID, limit)
}

// CreateRule 新建规则（type 缺省 custom；内置类型仅允许内部种入）
func (u *UseCase) CreateRule(ctx context.Context, name, typ, kind, instruction string, enabled bool) (*MemoryRule, error) {
	name = strings.TrimSpace(name)
	instruction = strings.TrimSpace(instruction)
	if name == "" {
		return nil, fmt.Errorf("memory: 规则名称不能为空")
	}
	if len([]rune(instruction)) < 10 {
		return nil, fmt.Errorf("memory: 提取指令至少 10 个字，需描述「提取什么」")
	}
	if typ == "" {
		typ = RuleTypeCustom
	}
	switch typ {
	case RuleTypeFact, RuleTypeProfile, RuleTypeSkill, RuleTypeCustom:
	default:
		return nil, fmt.Errorf("memory: 未知规则类型 %q", typ)
	}
	if kind == "" {
		kind = KindFact
	}
	if enabledSet(typ, kind) == "" {
		kind = KindFact // 未知 kind 回退
	}
	now := time.Now()
	r := &MemoryRule{
		Name:        name,
		Type:        typ,
		Kind:        kind,
		Instruction: instruction,
		Builtin:     false,
		Enabled:     enabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := u.repo.SaveRule(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// enabledSet 校验 kind 是否合法（复用一个小判断，避免引入多余函数）
func enabledSet(_, kind string) string {
	switch kind {
	case KindFact, KindPreference, KindEvent, KindInsight, KindTask, KindProfile, KindSkill:
		return kind
	}
	return ""
}

// UpdateRule 更新规则；内置规则同样可改 name/instruction/enabled（type/kind 内置不改 kind 可选）
func (u *UseCase) UpdateRule(ctx context.Context, id uint64, name, kind, instruction string, enabled bool) (*MemoryRule, error) {
	r, err := u.repo.GetRule(ctx, id)
	if err != nil {
		return nil, err
	}
	if v := strings.TrimSpace(name); v != "" {
		r.Name = v
	}
	if v := strings.TrimSpace(instruction); v != "" {
		if len([]rune(v)) < 10 {
			return nil, fmt.Errorf("memory: 提取指令至少 10 个字")
		}
		r.Instruction = v
	}
	if !r.Builtin {
		v := strings.TrimSpace(kind)
		if v != "" {
			if enabledSet("", v) == "" {
				return nil, fmt.Errorf("memory: 未知记忆类型 %q", v)
			}
			r.Kind = v
		}
	}
	r.Enabled = enabled
	r.UpdatedAt = time.Now()
	if err := u.repo.SaveRule(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// DeleteRule 删除规则（内置不可删）
func (u *UseCase) DeleteRule(ctx context.Context, id uint64) error {
	r, err := u.repo.GetRule(ctx, id)
	if err != nil {
		return err
	}
	if r.Builtin {
		return fmt.Errorf("memory: 内置规则不可删除，可停用（enabled=false）")
	}
	return u.repo.DeleteRule(ctx, id)
}

// ---------- 提取管线 ----------

const extractSystem = `你是记忆提取引擎。你的任务：按「提取指令」从对话记录中筛选值得写入长期记忆的内容。

要求：
1. 严格按提取指令判断取舍，不要提取指令范围之外的内容；没有可提取的就返回空数组。
2. 每条记忆必须自包含、脱离对话上下文也能看懂；同一件事只留一条，不编造、不推测。
3. importance 为 1~5 整数：5=核心且长期有效，3=一般有用，1=可能短期有效。
4. tags 为 0~3 个短标签（每个不超过 8 字），用于检索归类。

只输出 JSON，不要任何解释或代码块标记，格式：
{"memories":[{"content":"自包含的一条记忆","importance":3,"tags":["标签"]}]}
（memories 可以为空数组）`

// ExtractMemories 按规则从会话提取长期记忆：
// 取会话最近 lastN 条消息 -> 逐条启用规则调 LLM 提取 -> Remember 入库（内容级去重）。
// 规则级失败互相隔离（best-effort），不影响其他规则。
func (u *UseCase) ExtractMemories(ctx context.Context, sessionID, userID string, ruleIDs []uint64, lastN int) (*ExtractResult, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("memory: 会话 ID 不能为空")
	}
	if userID == "" {
		userID = "default"
	}
	if lastN <= 0 {
		lastN = 50
	}
	if lastN > 200 {
		lastN = 200
	}

	// 1. 会话消息：优先平台会话（MySQL），否则短期记忆（Redis）
	msgs, err := u.extractMessages(ctx, sessionID, userID, lastN)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("memory: 会话 %s 没有可提取的消息", sessionID)
	}

	// 2. 启用规则（可按 ruleIDs 过滤）
	rules, err := u.repo.ListRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory: 读取规则失败: %w", err)
	}
	filter := map[uint64]bool{}
	for _, id := range ruleIDs {
		filter[id] = true
	}
	selected := make([]*MemoryRule, 0, len(rules))
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if len(filter) > 0 && !filter[r.ID] {
			continue
		}
		selected = append(selected, r)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("memory: 没有启用的记忆规则")
	}

	res := &ExtractResult{SessionID: sessionID, Results: make([]RuleExtractResult, 0, len(selected))}
	transcript := renderTranscript(msgs)
	for _, r := range selected {
		rr := u.extractByRule(ctx, r, userID, transcript)
		res.Results = append(res.Results, rr)
		for _, m := range rr.Memories {
			if m.Error == "" {
				if m.Deduped {
					res.Deduped++
				} else {
					res.Written++
				}
			}
		}
	}
	return res, nil
}

// extractMessages 提取会话消息：先平台会话（conv），空则回源短期记忆。
//
// userID 同时是「要提取到谁名下」和「这个会话是谁的」：把别人的会话提取到
// 自己名下，等价于把对方对话内容搬进自己的长期记忆。
func (u *UseCase) extractMessages(ctx context.Context, sessionID, userID string, lastN int) ([]Message, error) {
	if u.conv != nil {
		if sms, err := u.conv.ListRecent(ctx, sessionID, userID, lastN); err == nil && len(sms) > 0 {
			msgs := make([]Message, 0, len(sms))
			for _, m := range sms {
				msgs = append(msgs, Message{Role: m.Role, Content: m.Content, At: m.At})
			}
			return msgs, nil
		}
	}
	hist, err := u.short.History(ctx, sessionID, lastN)
	if err != nil {
		return nil, fmt.Errorf("memory: 读取会话消息失败: %w", err)
	}
	return hist, nil
}

// extractByRule 单规则提取并入库
func (u *UseCase) extractByRule(ctx context.Context, r *MemoryRule, userID, transcript string) RuleExtractResult {
	out := RuleExtractResult{RuleID: r.ID, RuleName: r.Name, Memories: []ExtractedMemory{}}

	user := fmt.Sprintf("## 提取指令（%s）\n%s\n\n## 对话记录\n%s", r.Name, r.Instruction, transcript)
	raw, err := u.llm.ChatTemp(ctx, extractSystem, user, 0.2)
	if err != nil {
		out.Error = fmt.Sprintf("LLM 调用失败: %v", err)
		return out
	}
	items, err := parseExtractJSON(raw)
	if err != nil {
		out.Error = fmt.Sprintf("解析提取结果失败: %v", err)
		return out
	}

	for _, it := range items {
		content := strings.TrimSpace(it.Content)
		if content == "" {
			continue
		}
		em := ExtractedMemory{Content: clipText(content, 2000), Kind: r.Kind, Importance: it.Importance, Tags: it.Tags, RuleID: r.ID, RuleName: r.Name}
		if em.Importance <= 0 || em.Importance > 5 {
			em.Importance = 3
		}
		in := RememberInput{
			UserID:     userID,
			Kind:       r.Kind,
			Content:    content,
			Importance: em.Importance,
			Tags:       it.Tags,
			Source:     "规则提取 · " + r.Name,
			RuleName:   r.Name,
		}
		w, err := u.Remember(ctx, in)
		if err != nil {
			log.Warn("memory: 提取记忆写入失败", "rule", r.Name, "err", err)
			em.Error = err.Error()
		} else {
			em.MemoryID = w.Memory.ID
			em.Deduped = w.Deduped
		}
		out.Memories = append(out.Memories, em)
	}
	return out
}

// renderTranscript 会话消息 -> 文本记录（角色 + 内容，截断超长消息）
func renderTranscript(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = "unknown"
		}
		content := clipText(strings.TrimSpace(m.Content), 800)
		fmt.Fprintf(&b, "%s: %s\n", role, content)
	}
	return b.String()
}

// parseExtractJSON 从 LLM 输出中解析提取结果（容忍代码块与前后杂讯，括号配平定位）
func parseExtractJSON(raw string) ([]ExtractedItem, error) {
	s := strings.TrimSpace(raw)
	// 去掉 markdown 围栏
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
		s = strings.TrimSpace(s)
	}
	obj := locateJSON(s, '{', '}')
	if obj == "" {
		arr := locateJSON(s, '[', ']')
		if arr == "" {
			return nil, fmt.Errorf("未找到 JSON")
		}
		var items []ExtractedItem
		if err := json.Unmarshal([]byte(arr), &items); err != nil {
			return nil, err
		}
		return items, nil
	}
	var wrap struct {
		Memories []ExtractedItem `json:"memories"`
	}
	if err := json.Unmarshal([]byte(obj), &wrap); err != nil {
		return nil, err
	}
	return wrap.Memories, nil
}

// locateJSON 取 s 中第一个配平的 JSON 片段
func locateJSON(s string, open, close byte) string {
	start := strings.IndexByte(s, open)
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	var esc bool
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
