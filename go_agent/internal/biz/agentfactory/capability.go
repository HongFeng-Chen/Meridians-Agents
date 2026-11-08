package agentfactory

// 能力目录（CapabilityRegistry）：统一 Capability 抽象下的三类能力
//
//	factory  create_agent        —— AgentFactory 元编程入口（LLM 自造 Agent）
//	agent    agent:<agent_key>   —— 委派给另一个已注册 Agent（A2A 多 Agent 协作）
//	mcp      mcp:<unique_name>   —— MCP 工具
//
// 一个 Agent 本身也是能力：Runtime 执行的 Agent 可以被上层 Agent 当作能力调用，
// 实现「LLM 自己招人组队」。目录是动态的：MCP 工具与 Agent 定义实时拉取，
// 不做本地缓存副本，避免白名单过期。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/skill"
)

// Catalog 能力目录
type Catalog struct {
	tools  ToolPort
	agents func(ctx context.Context) ([]AgentDef, error) // 已启用 Agent 定义（除自身外都可委派）
	skills SkillPort                                     // 启用技能（调用 = 加载 SKILL.md 到上下文）
}

func NewCatalog(tools ToolPort, agents func(context.Context) ([]AgentDef, error), skills SkillPort) *Catalog {
	return &Catalog{tools: tools, agents: agents, skills: skills}
}

// Schemas 列出当前全部可用能力（LLM 提示词与前端展示共用）
func (c *Catalog) Schemas(ctx context.Context) ([]CapabilitySchema, error) {
	schemas := make([]CapabilitySchema, 0, 16)

	// 工厂能力（元编程）
	schemas = append(schemas, CapabilitySchema{
		Name: CapFactory,
		Type: "factory",
		Description: "创建一个新的 Agent（元编程）：给出名称、职责描述、所需能力白名单、执行图，" +
			"以及可选的后台自动运行（auto_run+cron_expr+cron_input）、自动装配（auto_compose）与编排方式（orchestration）。" +
			"工厂校验后注册，新 Agent 立即可被委派。注意：新 Agent 不能携带 create_agent 能力（防无限自举）；" +
			"新 Agent 进入沙箱灰度，不能携带委派能力（agent:*，会被自动剔除），人工验证后可解除沙箱。" +
			"能力白名单按需求领域逐项挑选（如 K8s 巡检选 mcp:*k8s* 工具）。",
		ArgsExample: `{"name":"K8s集群健康巡检","description":"定期巡检K8s集群健康状态","system":"你是K8s运维专家…","capabilities":["mcp:k8s-mcp__list_pods","mcp:k8s-mcp__get_events"],"graph_type":"plan_execute","max_turns":12,"auto_compose":false,"orchestration":"auto","auto_run":true,"cron_expr":"0 * * * *","cron_input":"执行一次 K8s 集群健康巡检并输出报告"}`,
	})

	// Agent 委派能力
	defs, err := c.agents(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出 Agent 定义失败: %w", err)
	}
	for _, d := range defs {
		schemas = append(schemas, CapabilitySchema{
			Name:        CapAgentPrefix + d.AgentKey,
			Type:        "agent",
			Description: fmt.Sprintf("委派任务给 Agent「%s」（%s）", d.Name, truncateRunes(d.Description, 120)),
			ArgsExample: `{"input":"需要委派的任务描述"}`,
		})
	}

	// MCP 工具能力
	if c.tools != nil {
		tools, err := c.tools.ListTools(ctx, mcp.ToolFilter{Enabled: boolPtr(true), Limit: 500})
		if err != nil {
			return nil, fmt.Errorf("列出 MCP 工具失败: %w", err)
		}
		for _, t := range tools {
			schemas = append(schemas, CapabilitySchema{
				Name:        CapToolPrefix + t.UniqueName,
				Type:        "mcp",
				Description: truncateRunes(t.Description, 200),
				ArgsExample: t.InputSchema,
			})
		}
	}

	// 技能能力：只列启用技能；技能库异常不阻断整个目录（技能是增强不是必需）
	if c.skills != nil {
		if sks, err := c.skills.List(ctx, skill.Filter{Enabled: boolPtr(true), Limit: 200}); err == nil {
			for _, sk := range sks {
				desc := sk.Description
				if desc == "" {
					desc = sk.Title
				}
				schemas = append(schemas, CapabilitySchema{
					Name:        CapSkillPrefix + sk.Name,
					Type:        "skill",
					Description: fmt.Sprintf("加载技能「%s」的操作说明/工作流到上下文（%s）", sk.Title, truncateRunes(desc, 160)),
					ArgsExample: `{"purpose":"使用该技能要完成的事"}`,
				})
			}
		}
	}

	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas, nil
}

// Known 判断能力名是否在当前目录内
func (c *Catalog) Known(ctx context.Context, name string) (bool, error) {
	if name == CapFactory {
		return true, nil
	}
	schemas, err := c.Schemas(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range schemas {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// InvokeDispatch 能力调用分发：授权 → 解析能力名 → 路由到对应实现。
//
// 授权是第一步，不是最后一步的装饰：模型输出的 action.name 完全不可信，
// 白名单必须在分发入口强制生效（见 authorize.go），否则「只读 Agent」只是
// 提示词上的一个说法。
//
// rec 用于 trace（delegate 事件需要落子任务链路），同时是授权依据（rec.def）。
func (u *UseCase) InvokeDispatch(ctx context.Context, name string, args map[string]any, rec *Recorder) (string, error) {
	if err := u.authorizeCapability(ctx, rec, name); err != nil {
		return "", err
	}
	switch {
	case name == CapFactory:
		return u.factoryCapability(ctx, args)
	case strings.HasPrefix(name, CapAgentPrefix):
		return u.delegateCapability(ctx, strings.TrimPrefix(name, CapAgentPrefix), args, rec)
	case strings.HasPrefix(name, CapToolPrefix):
		return u.toolCapability(ctx, strings.TrimPrefix(name, CapToolPrefix), args, rec)
	case strings.HasPrefix(name, CapSkillPrefix):
		return u.skillCapability(ctx, strings.TrimPrefix(name, CapSkillPrefix), args)
	default:
		return "", fmt.Errorf("%w: %s", ErrCapabilityUnset, name)
	}
}

// toolCapability 调用 MCP 工具，结果压成文本喂回 LLM。
// 配置了风险评估 Agent 时，调用前先评估风险：high 直接拦截（不执行副作用），
// medium 放行但把风险提示带回观察结果，low 正常放行。
// Agent 配置 risk_pause_level=high 时，高危操作不拦截而是转人工：任务挂起（paused），
// 批准后从 checkpoint 续跑（已批准能力不再重复评估）。
func (u *UseCase) toolCapability(ctx context.Context, uniqueName string, args map[string]any, rec *Recorder) (string, error) {
	if u.tools == nil {
		return "", fmt.Errorf("MCP 工具供给未接线")
	}
	ability := CapToolPrefix + uniqueName
	riskNote := ""
	alreadyApproved := rec != nil && rec.st != nil && rec.st.approved(ability)
	if !alreadyApproved && rec != nil && rec.def != nil && rec.def.RiskAgentKey != "" {
		if riskDef := u.loadRiskDef(ctx, rec.def.RiskAgentKey); riskDef != nil {
			if v := u.assessRisk(ctx, rec.task, riskDef, uniqueName, args, rec); v != nil {
				if !v.Allow {
					// 高危转人工（O3）：落 checkpoint + paused 事件，以哨兵错误上抛
					if strings.EqualFold(rec.def.RiskPauseLevel, "high") && v.Level == "high" && rec.st != nil {
						rec.st.Cursor = approvalCursor(ability)
						rec.checkpoint(rec.st.Cursor)
						rec.event(EventPaused, rec.turn, map[string]any{
							"ability": ability, "args": args,
							"reason": "risk_assess_high", "risk_reason": truncateRunes(v.Reason, 300),
						})
						return "", ErrApprovalNeeded
					}
					return "", fmt.Errorf("操作被风险评估拦截（%s）：%s。请改用更安全的只读方式，或交由人工处理", v.Level, truncateRunes(v.Reason, 200))
				}
				if v.Level == "medium" {
					riskNote = fmt.Sprintf("[风险提示] 本次操作评估为 medium：%s\n", truncateRunes(v.Reason, 200))
				}
			}
		}
	}
	res, err := u.tools.CallTool(ctx, 0, uniqueName, args)
	if err != nil {
		return "", fmt.Errorf("调用工具 %s 失败: %w", uniqueName, err)
	}
	return riskNote + callResultText(res), nil
}

// skillCapability 调用技能：读取 SKILL.md 说明文本喂回 LLM（技能是指令包，不是可执行程序）。
// args.purpose 可选，说明本次用技能要做什么，帮助模型按需裁剪工作流。
func (u *UseCase) skillCapability(ctx context.Context, name string, args map[string]any) (string, error) {
	if u.catalog == nil || u.catalog.skills == nil {
		return "", fmt.Errorf("技能供给未接线")
	}
	sk, err := u.catalog.skills.Get(ctx, name)
	if err != nil {
		return "", fmt.Errorf("读取技能 %s 失败: %w", name, err)
	}
	if !sk.Enabled {
		return "", fmt.Errorf("技能 %s 未启用，请先到技能管理启用", name)
	}
	head := fmt.Sprintf("[技能 %s v%s 已加载]", sk.Name, sk.Version)
	if purpose, _ := args["purpose"].(string); purpose != "" {
		head += "\n使用目的：" + purpose
	}
	body := sk.Content
	if body == "" {
		body = "（该技能没有 SKILL.md 说明文本）"
	}
	return head + "\n\n" + body, nil
}

func callResultText(r *mcp.CallResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	if r.Structured != "" {
		b.WriteString(r.Structured)
	}
	for _, p := range r.Content {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if p.Text != "" {
			b.WriteString(p.Text)
		} else if p.Data != "" {
			b.WriteString(fmt.Sprintf("[%s 内容 %d 字，已省略]", p.Type, len(p.Data)))
		}
	}
	out := strings.TrimSpace(b.String())
	if r.IsError {
		out = "[工具返回错误] " + out
	}
	return truncateRunes(out, 6000)
}

// delegateCapability 委派子 Agent：创建子任务（parent 链路），内联执行，返回其最终答案
func (u *UseCase) delegateCapability(ctx context.Context, agentKey string, args map[string]any, rec *Recorder) (string, error) {
	// 护栏 4：沙箱灰度 —— 沙箱 Agent 不允许委派其他 Agent
	if rec != nil && rec.def != nil && rec.def.Sandbox {
		return "", ErrSandboxDeny
	}
	input, _ := args["input"].(string)
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("委派参数需要 {\"input\": \"任务描述\"}")
	}
	parent := rec.task
	child, answer, err := u.runChildTask(ctx, parent, agentKey, input, rec)
	if err != nil {
		return "", err
	}
	_ = child
	return answer, nil
}

func boolPtr(b bool) *bool { return &b }

func truncateRunes(s string, n int) string {
	if utf8Len(s) <= n {
		return s
	}
	rs := []rune(s)
	if n <= 3 {
		return string(rs[:n])
	}
	return string(rs[:n-3]) + "..."
}

func utf8Len(s string) int { return len([]rune(s)) }

// marshalPayload 事件 payload 序列化（失败降级为错误占位，不影响主流程）
func marshalPayload(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return `{"error":"payload 序列化失败"}`
	}
	return string(raw)
}
