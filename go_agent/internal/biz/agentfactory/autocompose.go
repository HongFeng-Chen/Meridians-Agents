package agentfactory

// 自动编排（auto compose）：把「建 Agent」这件事从人工前置动作变成运行时决策。
//
// 触发条件（仅根 Agent）：
//   - graph_type=auto：不让用户选架构，交给 LLM 按任务判定；
//   - graph_type 是多 Agent 图（orchestrator / hierarchical / debate / blackboard）但 members 为空；
//   - auto_compose=true：每次任务都重新评估（成员集合变化时才回写定义）。
//
// 一次规划 = 一次 JSON 模式 LLM 调用，产出「用哪种图 + 需要哪些成员 + 各自职责」，
// 随后复用同名子 Agent、缺失的按最小权限创建（role=sub、sandbox=true、能力继承根的白名单），
// 最后把结果回写到根 Agent 定义（控制台可见、可手工改）。
//
// 护栏：
//   - 成员数量按图裁剪（debate 恰好 2 个，其余 2~5）；
//   - 单 Agent 图忽略模型给的 members；
//   - 子 Agent 不能携带 create_agent / agent:*，能力只做根 Agent 的子集（防提权）；
//   - 规划失败不阻断任务：回落现有配置（auto 图回落 react）并落 error 事件。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/llm"
)

const (
	autoPlanPurpose = "agentfactory.autocompose"
	// autoSystemMax 自动创建成员时角色提示词上限（字符），防止一次规划灌进上万 token
	autoSystemMax = 1600
	// autoCatalogMax 喂给规划器的可复用子 Agent 数量上限
	autoCatalogMax = 30
	// autoPlanTimeout 规划调用的独立超时：某个数据源在 JSON 模式下假死时，
	// 最多等这么久就回落现有配置，不把整个任务拖到上层 300s deadline。
	autoPlanTimeout = 60 * time.Second
)

// memberSpec 多 Agent 图的成员数量约束
type memberSpec struct {
	min int
	max int
}

var memberSpecs = map[string]memberSpec{
	GraphOrchestrator: {min: 2, max: 5},
	GraphHierarchical: {min: 2, max: 5},
	GraphDebate:       {min: 2, max: 2}, // 顺序敏感：members[0]=正方，members[1]=反方
	GraphBlackboard:   {min: 2, max: 4},
}

// autoMember LLM 规划出的成员规格
type autoMember struct {
	AgentKey    string `json:"agent_key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Duty        string `json:"duty"`
	System      string `json:"system"`
}

// autoPlan LLM 的编排决策
type autoPlan struct {
	GraphType string       `json:"graph_type"`
	Reason    string       `json:"reason"`
	Members   []autoMember `json:"members"`
}

// multiAgentSpec 该图是否属于多 Agent 协作图
func multiAgentSpec(graphType string) (memberSpec, bool) {
	spec, ok := memberSpecs[graphType]
	return spec, ok
}

// flowNodeCount 手绘 DAG 的节点数（>1 说明用户在编排画布上明确连过线）
func flowNodeCount(flow string) int {
	if g := ParseFlowGraph(flow); g != nil {
		return len(g.Nodes)
	}
	return 0
}

// needsAutoCompose 本次任务是否需要先做编排规划
func (u *UseCase) needsAutoCompose(def *AgentDef) bool {
	if def == nil || def.Role != RoleRoot {
		return false
	}
	// 手绘 DAG 优先：用户在画布上排好的流程不被自动规划覆盖（auto 图除外）
	if def.GraphType != GraphAuto && flowNodeCount(def.Flow) > 1 {
		return false
	}
	if def.GraphType == GraphAuto || def.AutoCompose {
		return true
	}
	_, multi := multiAgentSpec(def.GraphType)
	return multi && len(u.memberKeys(def)) == 0
}

// ensureOrchestration 执行图前置：按需规划架构与成员，返回本次实际使用的定义副本。
// 失败一律降级——自动编排是增强能力，不该让一个规划请求失败拖垮整个任务。
func (u *UseCase) ensureOrchestration(ctx context.Context, def *AgentDef, input string, rec *Recorder) *AgentDef {
	if !u.needsAutoCompose(def) {
		return def
	}
	rec.setProgress(8)

	plan, err := u.planOrchestration(ctx, def, input)
	if err != nil {
		u.warn("自动编排规划失败，沿用现有配置", "agent_key", def.AgentKey, "err", err)
		rec.event(EventError, rec.turn, map[string]any{"message": "自动编排失败，沿用现有配置：" + err.Error()})
		return u.fallbackAutoGraph(def)
	}
	plan = normalizePlan(plan, def)

	keys, created, reused := u.materializeMembers(ctx, def, plan, rec)
	if spec, multi := multiAgentSpec(plan.GraphType); multi && len(keys) < spec.min {
		msg := fmt.Sprintf("自动编排未能准备足够的 %s 成员（需要 %d 个，实际 %d 个）", plan.GraphType, spec.min, len(keys))
		rec.event(EventError, rec.turn, map[string]any{"message": msg})
		return u.fallbackAutoGraph(def)
	}

	rec.event(EventOrchestrate, rec.turn, map[string]any{
		"graph_type": plan.GraphType,
		"reason":     plan.Reason,
		"members":    planMembersPayload(plan.Members),
		"created":    created,
		"reused":     reused,
	})
	rec.setProgress(12)

	out := applyPlan(def, plan.GraphType, keys)
	u.persistPlan(ctx, def, out)
	return out
}

// planOrchestration 一次 JSON 模式调用：让模型决定架构与成员
func (u *UseCase) planOrchestration(ctx context.Context, def *AgentDef, input string) (*autoPlan, error) {
	if u.chat == nil {
		return nil, ErrNoChatter
	}
	system := autoPlannerSystem
	if catalog := u.subAgentCatalog(ctx); catalog != "" {
		system += "\n\n## 可复用的已有子 Agent（能复用就沿用其 agent_key）\n" + catalog
	}
	user := fmt.Sprintf("## 根 Agent\n名称：%s\n职责：%s\n角色提示词（节选）：%s\n\n## 本次任务\n%s",
		def.Name, def.Description, truncateRunes(def.System, 600), truncateRunes(input, 4000))

	planCtx, cancel := context.WithTimeout(ctx, autoPlanTimeout)
	defer cancel()
	out, err := u.chat.ChatOpts(llm.WithMeta(planCtx, llm.CallMeta{Purpose: autoPlanPurpose}), llm.ChatOptions{
		System:      system,
		User:        user,
		Source:      def.LLMSource, // 跟随根 Agent 的数据源：某个源不支持 JSON 时可整体切走
		Temperature: 0.2,
		JSONMode:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("LLM 规划失败: %w", err)
	}
	var plan autoPlan
	if err := decodeJSONInto(out, &plan); err != nil {
		return nil, fmt.Errorf("规划结果解析失败: %w（原始输出：%s）", err, truncateRunes(out, 300))
	}
	return &plan, nil
}

// subAgentCatalog 已有子 Agent 清单（供规划器优先复用，避免每次任务都新建同类成员）
func (u *UseCase) subAgentCatalog(ctx context.Context) string {
	defs, err := u.repo.ListDefs(ctx, DefFilter{Limit: autoCatalogMax * 3})
	if err != nil {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, d := range defs {
		if d.Role != RoleSub || !d.Enabled {
			continue
		}
		if n >= autoCatalogMax {
			break
		}
		n++
		fmt.Fprintf(&b, "- %s：%s（%s）\n", d.AgentKey, d.Name, truncateRunes(d.Description, 80))
	}
	return b.String()
}

// normalizePlan 规划结果的防御性规范化：图白名单、成员数量裁剪、key 清洗
func normalizePlan(p *autoPlan, def *AgentDef) *autoPlan {
	gt := strings.ToLower(strings.TrimSpace(p.GraphType))
	if !ValidGraphType(gt) || gt == GraphAuto {
		// 模型给了非法图：优先保留用户显式配置，否则回落 react
		if def.GraphType != "" && def.GraphType != GraphAuto && ValidGraphType(def.GraphType) {
			gt = def.GraphType
		} else {
			gt = GraphReact
		}
	}
	out := &autoPlan{GraphType: gt, Reason: strings.TrimSpace(p.Reason)}
	spec, multi := multiAgentSpec(gt)
	if !multi {
		return out // 单 Agent 架构不认成员
	}

	members := p.Members
	if len(members) > spec.max {
		members = members[:spec.max]
	}
	seen := make(map[string]bool, len(members))
	for i, m := range members {
		key := normalizeAgentKey(m.AgentKey)
		if key == "" {
			key = fmt.Sprintf("%s-member-%d", gt, i+1)
		}
		if key == def.AgentKey || seen[key] {
			continue
		}
		seen[key] = true
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = key
		}
		desc := strings.TrimSpace(m.Description)
		if desc == "" {
			desc = strings.TrimSpace(m.Duty)
		}
		system := strings.TrimSpace(m.System)
		if system == "" {
			system = fmt.Sprintf("你是「%s」。%s\n直接输出你的结论，不要客套。", name, desc)
		}
		out.Members = append(out.Members, autoMember{
			AgentKey:    key,
			Name:        truncateRunes(name, 40),
			Description: truncateRunes(desc, 200),
			Duty:        truncateRunes(m.Duty, 120),
			System:      truncateRunes(system, autoSystemMax),
		})
	}
	return out
}

// materializeMembers 复用同名子 Agent，缺失的按最小权限创建；返回最终成员 key 列表
func (u *UseCase) materializeMembers(ctx context.Context, def *AgentDef, plan *autoPlan, rec *Recorder) (keys, created, reused []string) {
	caps := memberCapabilities(def)
	for _, m := range plan.Members {
		if existing, err := u.repo.GetDefByKey(ctx, m.AgentKey); err == nil && existing != nil {
			keys = append(keys, existing.AgentKey)
			reused = append(reused, existing.AgentKey)
			continue
		}
		d, err := u.Factory().Create(ctx, SourceLLM, AgentInput{
			AgentKey:     m.AgentKey,
			Name:         m.Name,
			Description:  m.Description,
			System:       m.System,
			GraphType:    GraphReact,
			Role:         RoleSub,
			MaxTurns:     4,
			LLMSource:    def.LLMSource, // 成员跟随根 Agent 数据源，避免默认源不可用时整棵树卡住
			Sandbox:      true, // 护栏：自动创建的成员先进沙箱，不允许再委派
			Capabilities: caps,
		})
		if err != nil {
			u.warn("自动创建编排成员失败", "agent_key", m.AgentKey, "err", err)
			rec.event(EventError, rec.turn, map[string]any{"message": fmt.Sprintf("创建成员 %s 失败：%v", m.AgentKey, err)})
			continue
		}
		keys = append(keys, d.AgentKey)
		created = append(created, d.AgentKey)
	}
	return keys, created, reused
}

// memberCapabilities 成员能力 = 根 Agent 能力白名单的子集：
// 去掉 create_agent（防自举）与 agent:*（沙箱不可委派 + 防链式提权）。
func memberCapabilities(def *AgentDef) []string {
	out := make([]string, 0, len(def.Capabilities))
	for _, c := range def.Capabilities {
		c = strings.TrimSpace(c)
		if c == "" || c == CapFactory || strings.HasPrefix(c, CapAgentPrefix) || containsStr(out, c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// applyPlan 生成写入运行时的定义副本（不改原指针，避免污染缓存）
func applyPlan(def *AgentDef, graphType string, members []string) *AgentDef {
	out := *def
	out.GraphType = graphType
	if _, multi := multiAgentSpec(graphType); multi {
		out.Members = members
	} else {
		out.Members = nil
	}
	return &out
}

// fallbackAutoGraph auto 图规划失败时回落 react；其他图保持原样（下游会给出缺成员的明确报错）
func (u *UseCase) fallbackAutoGraph(def *AgentDef) *AgentDef {
	if def.GraphType != GraphAuto {
		return def
	}
	out := *def
	out.GraphType = GraphReact
	return &out
}

// persistPlan 回写定义：只有架构或成员集合真的变了才写库，避免每次任务都推进版本号。
//
// graph_type=auto 的定义保持 auto 不变（语义 = 每次任务自适应），只把本次选出的成员
// 写回去供下次复用；其他定义则固化为本次选定的图与成员，控制台可直接看到规划结果。
func (u *UseCase) persistPlan(ctx context.Context, base, def *AgentDef) {
	write := *def
	if base.GraphType == GraphAuto {
		write.GraphType = GraphAuto
		if _, multi := multiAgentSpec(def.GraphType); !multi {
			write.Members = base.Members // 单 Agent 结果不清空既有成员
		}
	}
	if old, err := u.repo.GetDef(ctx, def.ID); err == nil && old != nil &&
		old.GraphType == write.GraphType && sameStringSet(old.Members, write.Members) {
		return
	} else if err == nil && old != nil {
		write.Version = old.Version + 1
	}
	if err := u.repo.UpdateDef(ctx, &write); err != nil {
		u.warn("自动编排结果回写失败（本次任务仍按新配置执行）", "agent_key", def.AgentKey, "err", err)
		return
	}
	u.cache.SetDef(ctx, &write)
	if err := u.pushDefVectors(ctx, &write); err != nil {
		u.warn("自动编排成员回写后向量刷新失败", "agent_key", def.AgentKey, "err", err)
	}
}

func planMembersPayload(members []autoMember) []map[string]any {
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		out = append(out, map[string]any{
			"agent_key": m.AgentKey,
			"name":      m.Name,
			"duty":      firstNonEmpty(m.Duty, m.Description),
		})
	}
	return out
}

// normalizeAgentKey 清洗成小写字母/数字/连字符（≤40），不符合规则的字符转连字符
func normalizeAgentKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := true // 去掉开头的连字符
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.' || r == ' ' || r == '/' || r == ':':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	return out
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		if seen[v] == 0 {
			return false
		}
		seen[v]--
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// autoPlannerSystem 编排规划器提示词：架构选择 + 成员设计 + 数量约束
const autoPlannerSystem = `你是多 Agent 编排规划器。根据「根 Agent 的职责」与「本次任务」，决定这次该用哪种执行图，以及需要哪些子 Agent 分工。

只输出一个 JSON 对象，不要输出其他文字：
{"graph_type":"执行图","reason":"一句话说明为什么选它","members":[{"agent_key":"小写连字符标识","name":"中文名","description":"给 LLM 看的职责描述（50 字内）","duty":"在本任务里负责什么（30 字内）","system":"角色提示词：职责、工作方法、输出格式、边界"}]}

## 可选执行图
- simple：一次回答即可，无工具、无多步。
- react：需要多步推理或调用工具（检索、计算、查询）。
- plan_execute：长任务、步骤可审计，先出计划再逐步执行。
- reflexion：质量要求高、需要自我评审后重做（写作、代码、方案）。
- router：意图分类后分发给技能，不做自由推理。
- self_ask：把问题拆成子问题逐个求解再汇总。
- function_call：直接选工具执行，不需要思考文本。
- state_machine：强合规固定流程，只能按状态流转。
- orchestrator：多个专长角色分头完成不同子任务，最后汇总。
- hierarchical：跨领域大任务，先拆子目标，各角色再细化执行。
- debate：方案/结论需要正反对抗校验（正方出方案、反方挑错、多轮修订）。
- blackboard：多角色围绕同一份共享材料反复推演（方案推演、创意、评审）。

## 成员规则
1. 只有多 Agent 图（orchestrator / hierarchical / debate / blackboard）才给 members；其余图 members 必须是空数组。
2. debate 必须正好 2 个成员，顺序固定：第 1 个是正方（出方案），第 2 个是反方（挑错）。正方的 system 要求输出完整可落地内容；反方的 system 要求只挑问题、给证据与改进建议，不写方案。
3. orchestrator / hierarchical 给 2~5 个成员，按任务需要的专长拆分；blackboard 给 2~4 个。
4. agent_key 只用小写字母、数字、连字符，≤40 字符，体现角色（如 solution-author / risk-critic），不要与根 Agent 同名。
5. system 要写清「负责什么、不负责什么、输出什么结构」，≤1200 字；不要写入具体任务的临时数据。
6. 优先复用下面列出的已有子 Agent；确实缺角色时才新建。
7. 任务简单就别硬凑多 Agent：一个 Agent 能做完就用 simple / react。`
