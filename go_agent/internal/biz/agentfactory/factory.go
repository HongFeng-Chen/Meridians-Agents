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
	def, err := u.Factory().Create(ctx, SourceLLM, AgentInput{
		Name:         fa.Name,
		AgentKey:     fa.AgentKey,
		Description:  fa.Description,
		System:       fa.System,
		GraphType:    fa.GraphType,
		Capabilities: caps,
		MaxTurns:     fa.MaxTurns,
		Sandbox:      true, // 护栏 4：LLM 创建的 Agent 一律先进沙箱灰度
	})
	if err != nil {
		return "", err
	}
	return marshalPayload(map[string]any{
		"agent_id":  def.ID,
		"agent_key": def.AgentKey,
		"name":      def.Name,
		"sandbox":   def.Sandbox,
		"hint":      "Agent 已注册，可通过 agent:" + def.AgentKey + " 能力委派任务（沙箱内不允许再委派）",
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
		AgentKey:         in.AgentKey,
		Name:             in.Name,
		Description:      in.Description,
		System:           in.System,
		LLMModel:         in.LLMModel,
		LLMSource:        in.LLMSource,
		GraphType:        in.GraphType,
		AutoCompose:      in.AutoCompose,
		Capabilities:     in.Capabilities,
		Middlewares:      in.Middlewares,
		MaxTurns:         in.MaxTurns,
		Sandbox:          in.Sandbox,
		Role:             in.Role,
		Orchestration:    in.Orchestration,
		Members:          in.Members,
		Flow:             in.Flow,
		SupervisorKey:    in.SupervisorKey,
		SupervisionScope: in.SupervisionScope,
		RiskAgentKey:     in.RiskAgentKey,
		AutoRun:          in.AutoRun,
		CronExpr:         in.CronExpr,
		CronInput:        in.CronInput,
		Source:           source,
		Version:          1,
		Enabled:          true,
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
{"name":"中文名称","agent_key":"小写字母数字连字符标识","description":"给 LLM 看的职责描述，50字以内","system":"角色提示词，包含工作方法与边界","graph_type":"执行图，见下方可选值","capabilities":["从下方可用能力里选择"],"max_turns":10}

规则：
1. capabilities 只能从可用能力清单中选择，不能编造；不需要能力就留空数组
2. graph_type 可选：simple（单轮直答）/ react（思考-行动-观察）/ plan_execute（先计划再执行）/
   reflexion（执行-评估-反思重试）/ router（意图路由到技能）/ self_ask（拆子问题逐个回答）/
   function_call（无思考、直接调工具）/ state_machine（固定状态流转）/
   orchestrator（协调多成员）/ hierarchical（分层规划执行）/ debate（正反辩论）/ blackboard（共享黑板）
3. 简单问答用 simple；需要多步推理与工具用 react；长任务且步骤可审计用 plan_execute；要求高质量可自纠用 reflexion
4. max_turns 取 5~20
5. description 是语义召回的依据，要写清楚这个 Agent 擅长什么
6. 新 Agent 处于沙箱灰度，不允许委派其他 Agent：可用能力清单已排除 agent:* 项，不要编造；
   如需求涉及多 Agent 协作，请按「单 Agent + 多步骤」设计（把协作方法写进 system），
   由人工验证后手动补充委派能力

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

	created, err := f.Create(ctx, SourceLLM, AgentInput{
		Name:         def.Name,
		AgentKey:     def.AgentKey,
		Description:  def.Description,
		System:       def.System,
		GraphType:    def.GraphType,
		Capabilities: caps,
		MaxTurns:     def.MaxTurns,
		Sandbox:      true, // 护栏 4：LLM 创建默认沙箱灰度
	})
	if err != nil {
		return nil, output, err
	}
	return created, output, nil
}
