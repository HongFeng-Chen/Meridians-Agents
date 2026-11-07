package agentfactory

// AgentFactory：把「造 Agent」做成能力（元编程）。
//
// 两条入口汇合同一份定义存储：
//   - 人工低代码配置（CreateDef）
//   - LLM 调用 create_agent 能力 / create-by-llm 接口（Factory.Create）
//
// 四个硬护栏（对应设计文档「元编程的风险」一节）：
//   1. 能力白名单授权：capabilities 必须落在平台已注册能力集合内
//   2. 禁止无限自举：LLM 创建的 Agent 不能携带 create_agent（本工厂在 LLM 来源下直接拒绝）
//   3. 配额：Agent 总数受 max_agents 限制
//   4. 沙箱灰度：LLM 创建的 Agent 默认 sandbox=true（禁止委派他人、不能再用工厂），
//      人工验证后通过 toggle 关闭沙箱

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/biz/llm"
)

// factoryArgs create_agent 能力入参（LLM 生成）
type factoryArgs struct {
	Name         string   `json:"name"`
	AgentKey     string   `json:"agent_key"`
	Description  string   `json:"description"`
	System       string   `json:"system"`
	GraphType    string   `json:"graph_type"`
	Capabilities []string `json:"capabilities"`
	MaxTurns     int      `json:"max_turns"`
	// 意图直传字段：需求里写了周期性/编排语义时由 LLM 填写，缺省即保持旧行为
	AutoCompose   bool   `json:"auto_compose"`
	Orchestration string `json:"orchestration"`
	AutoRun       bool   `json:"auto_run"`
	CronExpr      string `json:"cron_expr"`
	CronInput     string `json:"cron_input"`
}

// toAgentInput 工厂入参 → AgentInput（factoryCapability 与 CreateByLLM 共用，
// 新增意图字段时两边自动生效）
func (fa factoryArgs) toAgentInput(sandbox bool) AgentInput {
	return AgentInput{
		Name:          fa.Name,
		AgentKey:      fa.AgentKey,
		Description:   fa.Description,
		System:        fa.System,
		GraphType:     fa.GraphType,
		Capabilities:  fa.Capabilities,
		MaxTurns:      fa.MaxTurns,
		AutoCompose:   fa.AutoCompose,
		Orchestration: fa.Orchestration,
		AutoRun:       fa.AutoRun,
		CronExpr:      fa.CronExpr,
		CronInput:     fa.CronInput,
		Sandbox:       sandbox,
	}
}

// splitDelegateCaps 沙箱 Agent 不可委派：把 agent:* 能力从白名单里剔除。
// LLM 来源创建的 Agent 一律 sandbox=true，若模型仍选了委派能力，
// 直接剔除而非整体失败（否则候选集与沙箱护栏自相矛盾，用户永远建不出来）。
// 返回保留项与被剔除项（被剔除项用于提示 / 日志）。
func splitDelegateCaps(caps []string) (kept, dropped []string) {
	for _, c := range caps {
		if strings.HasPrefix(strings.TrimSpace(c), CapAgentPrefix) {
			dropped = append(dropped, c)
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

// factoryCapability create_agent 能力实现：LLM 给出定义 → 校验 → 注册
func (u *UseCase) factoryCapability(ctx context.Context, args map[string]any) (string, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("create_agent 参数不合法: %w", err)
	}
	var fa factoryArgs
	if err := json.Unmarshal(raw, &fa); err != nil {
		return "", fmt.Errorf("create_agent 参数不合法: %w", err)
	}
	caps, dropped := splitDelegateCaps(fa.Capabilities)
	if len(dropped) > 0 {
		u.warn("沙箱 Agent 已剔除委派能力", "agent_key", fa.AgentKey, "dropped", dropped)
	}
	fa.Capabilities = caps
	// 与 CreateByLLM 同一套意图兜底：工具调用方漏填 auto_run/auto_compose 时，
	// 从描述与角色提示词里推导（周期语义 → cron，协同语义 → 自动装配）
	in := fa.toAgentInput(true)
	note := sanitizeFactoryIntent(u, &in, fa.Description+" "+fa.System)
	def, err := u.Factory().Create(ctx, SourceLLM, in)
	if err != nil {
		return "", err
	}
	if pre := u.precomposeCreated(ctx, def, fa.Description+" "+fa.System); pre != "" {
		if note != "" {
			note += "；" + pre
		} else {
			note = pre
		}
	}
	return marshalPayload(map[string]any{
		"agent_id":     def.ID,
		"agent_key":    def.AgentKey,
		"name":         def.Name,
		"sandbox":      def.Sandbox,
		"auto_run":     def.AutoRun,
		"cron_expr":    def.CronExpr,
		"auto_compose": def.AutoCompose,
		"members":      def.Members,
		"note":         note,
		"hint":         "Agent 已注册，可通过 agent:" + def.AgentKey + " 能力委派任务（沙箱内不允许再委派）",
	}), nil
}

// Factory 返回工厂门面
func (u *UseCase) Factory() *AgentFactory { return &AgentFactory{u: u} }

// AgentFactory 创建 Agent 定义的工厂门面（CreateDef / create_agent / create-by-llm 共用）
type AgentFactory struct{ u *UseCase }

// Create 校验 + 落库 + 向量注册 + 缓存。source 决定护栏强度。
func (f *AgentFactory) Create(ctx context.Context, source string, in AgentInput) (*AgentDef, error) {
	u := f.u
	if err := in.Normalize(u.opt.DefaultMaxTurns); err != nil {
		return nil, err
	}

	// 护栏 3：配额
	total, err := u.repo.CountDefs(ctx)
	if err != nil {
		return nil, err
	}
	if total >= int64(u.opt.MaxAgents) {
		return nil, fmt.Errorf("%w: 已有 %d 个 Agent，上限 %d", ErrQuotaExceeded, total, u.opt.MaxAgents)
	}

	// 护栏 2：防无限自举
	if source == SourceLLM {
		for _, c := range in.Capabilities {
			if c == CapFactory {
				return nil, ErrBootStrapDeny
			}
		}
	}

	// 护栏 1：能力白名单授权
	for _, c := range in.Capabilities {
		ok, err := u.catalog.Known(ctx, c)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrCapabilityDeny, c)
		}
	}
	// 角色引用校验：成员 / 监察 Agent / 风险评估 Agent 必须存在且角色匹配
	if err := u.validateRoleRefs(ctx, in); err != nil {
		return nil, err
	}
	// 护栏 4（沙箱不可委派）已在 in.Normalize → ValidateSandboxCaps 内统一校验

	// 唯一性
	if exists, err := u.repo.GetDefByKey(ctx, in.AgentKey); err != nil {
		return nil, err
	} else if exists != nil {
		return nil, fmt.Errorf("%w: 标识 %q 已被占用", ErrDefExists, in.AgentKey)
	}

	def := &AgentDef{
		AgentKey:             in.AgentKey,
		Name:                 in.Name,
		Description:          in.Description,
		System:               in.System,
		LLMModel:             in.LLMModel,
		LLMSource:            in.LLMSource,
		GraphType:            in.GraphType,
		AutoCompose:          in.AutoCompose,
		Capabilities:         in.Capabilities,
		Middlewares:          in.Middlewares,
		MaxTurns:             in.MaxTurns,
		Sandbox:              in.Sandbox,
		Role:                 in.Role,
		Orchestration:        in.Orchestration,
		Members:              in.Members,
		Flow:                 in.Flow,
		SupervisorKey:        in.SupervisorKey,
		SupervisionScope:     in.SupervisionScope,
		RiskAgentKey:         in.RiskAgentKey,
		RequireApproval:      in.RequireApproval,
		ApprovalTimeoutHours: in.ApprovalTimeoutHours,
		RiskPauseLevel:       in.RiskPauseLevel,
		AutoRun:              in.AutoRun,
		CronExpr:             in.CronExpr,
		CronInput:            in.CronInput,
		Source:               source,
		Version:              1,
		Enabled:              true,
	}
	if def.AutoRun {
		def.NextRunAt = nextRunTime(def.CronExpr, time.Now())
	}
	if err := u.repo.CreateDef(ctx, def); err != nil {
		return nil, err
	}
	// LLM 创建的 Agent：把角色提示词沉淀到提示词工程（key = agent.<key>.system）。
	// 失败只记 Warning，不影响 Agent 创建本身。
	if source == SourceLLM && u.prompts != nil {
		if err := u.prompts.SaveAgentPrompt(ctx, def.AgentKey, def.Name, def.System); err != nil {
			u.warn("Agent 角色提示词沉淀到提示词工程失败", "agent_key", def.AgentKey, "err", err)
		}
	}
	if err := u.pushDefVectors(ctx, def); err != nil {
		// 向量化失败不阻塞创建（元数据已保存），只记 Warning
		u.warn("Agent 向量化失败", "key", def.AgentKey, "err", err)
	}
	u.cache.SetDef(ctx, def)
	return def, nil
}

// CreateByLLM 元编程入口：把需求交给 LLM 生成 Agent 定义，走同一套护栏。
func (f *AgentFactory) CreateByLLM(ctx context.Context, requirement string) (*AgentDef, string, error) {
	u := f.u
	requirement = strings.TrimSpace(requirement)
	if requirement == "" {
		return nil, "", fmt.Errorf("%w: 需求描述必填", ErrInvalidDef)
	}
	if len([]rune(requirement)) > 2000 {
		return nil, "", fmt.Errorf("%w: 需求描述过长（≤2000 字）", ErrInvalidDef)
	}
	if u.chat == nil {
		return nil, "", ErrNoChatter
	}

	schemas, err := u.catalog.Schemas(ctx)
	if err != nil {
		return nil, "", err
	}
	avail := make([]string, 0, len(schemas))
	for _, s := range schemas {
		// LLM 来源禁止 create_agent，候选集里不出现，从源头防自举
		if s.Name == CapFactory {
			continue
		}
		// LLM 创建的 Agent 强制沙箱，沙箱内不允许委派：候选集里也不列 agent:*，
		// 否则模型很可能选中、随后被 ValidateSandboxCaps 判死（自相矛盾的候选集）
		if strings.HasPrefix(s.Name, CapAgentPrefix) {
			continue
		}
		avail = append(avail, s.Name+"（"+s.Type+"）： "+s.Description)
	}

	system := `你是 Agent 平台的 AgentFactory：根据用户需求设计一个新 Agent 的定义，输出严格 JSON（不要输出其他文字）：
{"name":"中文名称","agent_key":"小写字母数字连字符标识","description":"给 LLM 看的职责描述，50字以内","system":"角色提示词，包含工作方法与边界","graph_type":"执行图，见下方可选值","capabilities":["从下方可用能力里选择"],"max_turns":10,"auto_compose":false,"orchestration":"auto","auto_run":false,"cron_expr":"","cron_input":""}

规则：
1. capabilities 按需求逐项挑选，只选与需求领域真正相关的能力（如 K8s 巡检选 mcp:*k8s* 工具），
   宁缺毋滥，不能编造；不需要能力就留空数组
2. graph_type 可选：simple（单轮直答）/ react（思考-行动-观察）/ plan_execute（先计划再执行）/
   reflexion（执行-评估-反思重试）/ router（意图路由到技能）/ self_ask（拆子问题逐个回答）/
   function_call（无思考、直接调工具）/ state_machine（固定状态流转）/
   orchestrator（协调多成员）/ hierarchical（分层规划执行）/ debate（正反辩论）/ blackboard（共享黑板）
3. 简单问答用 simple；需要多步推理与工具用 react；长任务且步骤可审计用 plan_execute；要求高质量可自纠用 reflexion
4. max_turns 取 5~20
5. description 是语义召回的依据，要写清楚这个 Agent 擅长什么
6. 新 Agent 处于沙箱灰度，不允许委派其他 Agent：可用能力清单已排除 agent:* 项，不要编造；
   如需求是「委派/复用已有 Agent」，由人工验证后手动补充委派能力；
   如需求是「组建新团队协同」，用 auto_compose（见规则 8），不要写成单 Agent
7. orchestration 可选 auto / serial / parallel：需求写明固定步骤依次执行用 serial，
   明确要求并行用 parallel，其余一律 auto
8. auto_compose 自动装配：需求出现「协同/协作/团队/分工/编排/多个子 Agent/自动拆解」
   等语义时必须 true（首次任务运行时会自动规划架构并创建子 Agent）；单 Agent 任务保持 false
9. 后台自动运行：需求出现「每小时/每天 X 点/定时/周期性/自动巡检」等周期执行语义时，
   必须设 auto_run=true，并给出：
   - cron_expr：5 段 cron（分 时 日 月 周）。每小时=0 * * * *；每30分钟=*/30 * * * *；
     每天 9 点=0 9 * * *；工作日 9 点=0 9 * * 1-5；每周一 9 点=0 9 * * 1
   - cron_input：定时触发时提交给 Agent 的固定任务输入，从需求提炼（一句话任务指令）
   需求没有周期执行语义时保持 auto_run=false，cron_expr 与 cron_input 留空字符串

可用能力清单：
` + strings.Join(avail, "\n")

	// JSON 模式（不支持自动降级）：定义输出是结构化 JSON，强制合法省自纠轮
	output, err := u.chat.ChatOpts(llm.WithMeta(ctx, llm.CallMeta{Purpose: "agentfactory.factory"}), llm.ChatOptions{
		System:      system,
		User:        requirement,
		Temperature: 0.2,
		JSONMode:    true,
	})
	if err != nil {
		return nil, output, fmt.Errorf("LLM 生成定义失败: %w", err)
	}
	var def factoryArgs
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end <= start {
		return nil, output, fmt.Errorf("LLM 输出中没有 JSON 定义")
	}
	if err := json.Unmarshal([]byte(output[start:end+1]), &def); err != nil {
		return nil, output, fmt.Errorf("LLM 定义解析失败: %w", err)
	}

	// 防御性剔除：模型仍可能凭记忆编出 agent:*（候选集已排除），沙箱下直接剔除而非整体失败
	caps, dropped := splitDelegateCaps(def.Capabilities)
	if len(dropped) > 0 {
		u.warn("LLM 造 Agent 时已剔除委派能力（沙箱不可委派）", "agent_key", def.AgentKey, "dropped", dropped)
	}
	def.Capabilities = caps

	in := def.toAgentInput(true)
	if note := sanitizeFactoryIntent(u, &in, requirement); note != "" {
		output += "\n[factory] " + note
	}

	created, err := f.Create(ctx, SourceLLM, in)
	if err != nil {
		return nil, output, err
	}
	if note := u.precomposeCreated(ctx, created, requirement); note != "" {
		output += "\n[factory] " + note
	}
	return created, output, nil
}

// precomposeCreated 创建期预装配：auto_compose（或 auto 图/多 Agent 图缺成员）的根 Agent
// 创建后立即规划一次架构与成员并落库——编排画布、Agent 列表的父子关系马上可见，
// 不用等第一次任务运行。失败不阻断创建（返回说明文本），运行时 ensureOrchestration 会重试。
func (u *UseCase) precomposeCreated(ctx context.Context, def *AgentDef, input string) string {
	if def == nil || def.Role != RoleRoot || !u.needsAutoCompose(def) {
		return ""
	}
	plan, err := u.planOrchestration(ctx, def, input)
	if err != nil {
		u.warn("创建期自动装配预规划失败（首次任务运行时会重试）", "agent_key", def.AgentKey, "err", err)
		return "创建期自动装配预规划失败（首次运行任务时会自动重试）：" + err.Error()
	}
	plan = normalizePlan(plan, def)
	keys, created, reused := u.materializeMembers(ctx, def, plan, nil)
	if spec, multi := multiAgentSpec(plan.GraphType); multi && len(keys) < spec.min {
		u.warn("创建期自动装配成员不足，交由首次任务运行重试", "agent_key", def.AgentKey,
			"graph_type", plan.GraphType, "members", len(keys))
		return fmt.Sprintf("自动装配成员不足（%s 至少需要 %d 个，实际 %d 个），首次运行任务时会自动重试",
			plan.GraphType, spec.min, len(keys))
	}
	out := applyPlan(def, plan.GraphType, keys)
	u.persistPlan(ctx, def, out)
	*def = *out
	if len(created) == 0 && len(reused) == 0 {
		return ""
	}
	return fmt.Sprintf("创建期自动装配完成：架构 %s，新建成员 %v，复用成员 %v，成员列表 %v——编排画布已可见",
		out.GraphType, created, reused, keys)
}

// sanitizeFactoryIntent 对 LLM 给出的意图字段做兜底（不因幻觉让整次创建失败）：
//   - orchestration 非法 → 降级为 auto；
//   - 模型漏填 auto_run/cron 但需求含周期语义 → 按需求原文推导排期（intent.go）；
//   - 模型漏填 auto_compose 但需求含协同/编排语义 → 开启自动装配；
//   - cron 表达式非法 → 关闭后台自动运行并提示人工补配；
//   - auto_run 开启但缺 cron_input → 用需求原文补齐。
//
// 有降级/推导动作时返回提示文本（拼到 raw 输出里给前端展示），无动作时返回空串。
func sanitizeFactoryIntent(u *UseCase, in *AgentInput, requirement string) string {
	var notes []string

	in.Orchestration = strings.ToLower(strings.TrimSpace(in.Orchestration))
	if in.Orchestration == "" {
		in.Orchestration = OrchestrationAuto
	}
	if !ValidOrchestration(in.Orchestration) {
		u.warn("LLM 生成的编排方式不合法，已降级为 auto", "orchestration", in.Orchestration)
		in.Orchestration = OrchestrationAuto
		notes = append(notes, "LLM 生成的编排方式不合法，已降级为 auto")
	}

	// 模型漏填时的服务端意图推导：提示词只是「请求」，这里才是「保证」——
	// 用户写了「每小时巡检」「多 Agent 协同」却得到一个手动单 Agent，属于意图丢失。
	if !in.AutoRun {
		if cron, why := deriveCronFromRequirement(requirement); cron != "" {
			in.AutoRun, in.CronExpr = true, cron
			u.warn("需求含周期语义但 LLM 漏填定时字段，已按需求推导排期",
				"cron_expr", cron, "依据", why)
			notes = append(notes, "需求含周期执行语义（"+why+"）但 LLM 未设置后台自动运行，"+
				"已自动排期 cron="+cron+"；定时输入取需求原文，可在编辑页调整")
		}
	}
	if !in.AutoCompose && requirementImpliesCollab(requirement) {
		in.AutoCompose = true
		u.warn("需求含协同语义但 LLM 漏填自动装配，已开启 auto_compose")
		notes = append(notes, "需求含多 Agent 协同/编排语义，已开启自动装配：创建时即自动规划架构"+
			"并创建子 Agent（编排画布可见），后续任务按需调整成员")
	}

	if !in.AutoRun {
		return strings.Join(notes, "；")
	}

	in.CronExpr = strings.TrimSpace(in.CronExpr)
	in.CronInput = strings.TrimSpace(in.CronInput)
	if err := ValidateCron(in.CronExpr); err != nil {
		u.warn("LLM 生成的 cron 表达式不合法，已关闭后台自动运行（可人工补配）",
			"cron_expr", in.CronExpr, "err", err)
		in.AutoRun, in.CronExpr, in.CronInput = false, "", ""
		notes = append(notes, "LLM 生成的 cron 表达式不合法，已自动关闭后台自动运行；"+
			"Agent 已创建，可在编辑页手动勾选后台自动运行并填写 cron")
	} else if in.CronInput == "" {
		in.CronInput = truncateRunes(strings.TrimSpace(requirement), 4000)
	}
	return strings.Join(notes, "；")
}
