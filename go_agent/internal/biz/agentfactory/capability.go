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
	"strconv"
	"strings"

	"go_agent/internal/biz/audit"
	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/skill"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/trace"
)

// Catalog 能力目录
type Catalog struct {
	tools  ToolPort
	agents func(ctx context.Context) ([]AgentDef, error) // 已启用 Agent 定义（除自身外都可委派）
	skills SkillPort                                     // 启用技能（local 加载 SKILL.md；remote/async 走运行时）
	// skillRT 技能运行时（可执行技能）。未接线时为 nil：可执行技能会明确报错而不是静默降级，
	// 避免「配置了 remote 却按 local 把说明当结果」这种假装成功。
	skillRT SkillRuntimePort
	media   MediaPort // 媒体生成（可为 nil：最小部署未装配）
}

func NewCatalog(tools ToolPort, agents func(context.Context) ([]AgentDef, error), skills SkillPort, media MediaPort) *Catalog {
	c := &Catalog{tools: tools, agents: agents, skills: skills, media: media}
	if rt, ok := skills.(SkillRuntimePort); ok {
		c.skillRT = rt
	}
	return c
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
	//
	// 两种技能在提示词里的表述必须不同，否则模型会把「远程能力」当成说明文档去读：
	//   local  —— 加载操作说明到上下文（后续按说明自己做）
	//   remote —— 调用远程能力，参数按 input_schema（模型不该关心 Worker 在哪）
	//   async  —— 提交异步任务，拿 execution_id 后用 skill:status 查
	if c.skills != nil {
		hasAsync := false
		if sks, err := c.skills.List(ctx, skill.Filter{Enabled: boolPtr(true), Limit: 200}); err == nil {
			for _, sk := range sks {
				desc := sk.Description
				if desc == "" {
					desc = sk.Title
				}
				spec := sk.Execution.Normalized()
				schema := CapabilitySchema{
					Name: CapSkillPrefix + sk.Name,
					Type: "skill",
				}
				switch spec.Mode {
				case skill.ModeRemote:
					schema.Description = fmt.Sprintf("调用远程技能「%s」并等待结果（%s）。参数必须符合 input_schema。",
						sk.Title, truncateRunes(desc, 120))
					schema.ArgsExample = skillArgsExample(spec)
				case skill.ModeAsync:
					hasAsync = true
					schema.Description = fmt.Sprintf("提交异步技能「%s」，立即返回执行 ID；"+
						"之后用 %s 查结果（%s）。", sk.Title, CapSkillStatus, truncateRunes(desc, 100))
					schema.ArgsExample = skillArgsExample(spec)
				default:
					schema.Description = fmt.Sprintf("加载技能「%s」的操作说明/工作流到上下文（%s）",
						sk.Title, truncateRunes(desc, 160))
					schema.ArgsExample = `{"purpose":"使用该技能要完成的事"}`
				}
				schemas = append(schemas, schema)
			}
		}
		// 异步技能的状态查询能力：只在有异步技能时才出现，避免给模型一个用不上的工具
		if hasAsync && c.skillRT != nil {
			schemas = append(schemas, CapabilitySchema{
				Name:        CapSkillStatus,
				Type:        "skill",
				Description: "查询技能异步执行的进度/结果：传提交时返回的 execution_id，返回 queued/running/succeeded/failed 与输出。",
				ArgsExample: `{"execution_id":"exec_1a2b3c4d"}`,
			})
		}
	}

	// 媒体生成能力：图像同步生成；视频异步提交后用 llm:video_status 轮询。
	// 模型清单进描述是 best effort：目录接口失败只降级为通用描述，不阻断能力选择。
	schemas = append(schemas, c.mediaSchemas(ctx)...)

	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas, nil
}

// mediaSchemas 组装媒体生成能力描述（llm:image / llm:video / llm:video_status）
func (c *Catalog) mediaSchemas(ctx context.Context) []CapabilitySchema {
	imgHint, vidHint := "", ""
	if c.media != nil {
		if imgs, vids, err := c.media.MediaCatalog(ctx); err == nil {
			if len(imgs) > 0 {
				imgHint = "可用图像模型：" + mediaCatalogText(imgs)
			} else {
				imgHint = "平台尚未配置图像生成数据源（需在数据源里填写 image models 后使用）"
			}
			if len(vids) > 0 {
				vidHint = "可用视频模型：" + mediaCatalogText(vids)
			} else {
				vidHint = "平台尚未配置视频生成数据源（需在数据源里填写 video models 后使用）"
			}
		}
	}
	return []CapabilitySchema{
		{
			Name: CapImage,
			Type: "media",
			Description: "生成图片（文生图 / 图生图）：把图片画出来，返回本地文件地址（/api/files/）。" +
				"需要画图、海报、示意图、配图等场景使用。同步等待，通常 10~60 秒。" + imgHint,
			ArgsExample: `{"prompt":"水墨风格的城市黄昏插画，暖色调","size":"1024x1024","count":1}`,
		},
		{
			Name: CapVideo,
			Type: "media",
			Description: "提交视频生成任务（文生视频 / 图生视频）：异步执行，返回记录 ID，" +
				"用 " + CapVideoStatus + " 轮询进度，成功后拿到本地文件地址。通常 1~10 分钟。" + vidHint,
			ArgsExample: `{"prompt":"一只柯基在草地上奔跑，镜头缓慢推进","size":"1280x720","duration_seconds":5}`,
		},
		{
			Name: CapVideoStatus,
			Type: "media",
			Description: "查询视频生成任务状态：传入提交时返回的记录 ID。" +
				"返回 queued（排队）/ running（生成中）/ succeeded（含视频地址）/ failed（含原因）。",
			ArgsExample: `{"id":123}`,
		},
	}
}

// mediaCatalogText 压缩模型目录为一行文本（provider(模型1/模型2)，多项分号分隔）
func mediaCatalogText(list []llm.MediaModelInfo) string {
	parts := make([]string, 0, len(list))
	for _, m := range list {
		models := m.Models
		if len(models) > 4 {
			models = append(models[:4], fmt.Sprintf("等 %d 个", len(m.Models)))
		}
		parts = append(parts, m.Provider+"（"+strings.Join(models, "/")+"）")
	}
	return truncateRunes(strings.Join(parts, "；"), 240)
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

// InvokeDispatch 能力调用分发：授权 → 审批闸门 → 解析能力名 → 路由到对应实现。
//
// 授权与审批都是入口处的强制步骤，不是最后一步的装饰：模型输出的 action.name
// 完全不可信，白名单必须在分发入口强制生效（见 authorize.go），否则「只读 Agent」
// 只是提示词上的一个说法；审批同理，挂在入口才是三条执行路径的公共必经点。
//
// rec 用于 trace（delegate 事件需要落子任务链路），同时是授权依据（rec.def）。
func (u *UseCase) InvokeDispatch(ctx context.Context, name string, args map[string]any, rec *Recorder) (string, error) {
	if err := u.authorizeCapability(ctx, rec, name); err != nil {
		return "", err
	}
	// 运行时二次授权：白名单只回答「这个 Agent 有没有这项能力」，
	// 这里再问一次「当前主体此刻还能不能用这个资源」（快照 ≠ 永久权限）。
	if err := u.authorizeCapabilityRuntime(ctx, rec, name); err != nil {
		return "", err
	}
	if err := u.gateApproval(ctx, rec, name, args); err != nil {
		return "", err
	}
	switch {
	case name == CapFactory:
		return u.factoryCapability(ctx, args)
	case name == CapSkillStatus:
		return u.skillStatusCapability(ctx, args, rec)
	case strings.HasPrefix(name, CapAgentPrefix):
		return u.delegateCapability(ctx, strings.TrimPrefix(name, CapAgentPrefix), args, rec)
	case strings.HasPrefix(name, CapToolPrefix):
		return u.toolCapability(ctx, strings.TrimPrefix(name, CapToolPrefix), args, rec)
	case strings.HasPrefix(name, CapSkillPrefix):
		return u.skillCapability(ctx, strings.TrimPrefix(name, CapSkillPrefix), args, rec)
	case name == CapImage:
		return u.imageCapability(ctx, args)
	case name == CapVideo:
		return u.videoCapability(ctx, args)
	case name == CapVideoStatus:
		return u.videoStatusCapability(ctx, args)
	default:
		return "", fmt.Errorf("%w: %s", ErrCapabilityUnset, name)
	}
}

// toolCapability 调用 MCP 工具，结果压成文本喂回 LLM。
// 配置了风险评估 Agent 时，调用前先评估风险：high 直接拦截（不执行副作用），
// medium 放行但把风险提示带回观察结果，low 正常放行。
// Agent 配置 risk_pause_level=high 时，高危操作不拦截而是转人工：任务挂起（paused），
// 批准后从 checkpoint 续跑（已批准这一次具体调用不再重复评估）。
func (u *UseCase) toolCapability(ctx context.Context, uniqueName string, args map[string]any, rec *Recorder) (string, error) {
	if u.tools == nil {
		return "", fmt.Errorf("MCP 工具供给未接线")
	}
	ability := CapToolPrefix + uniqueName
	riskNote := ""
	alreadyApproved := false
	if rec != nil {
		_, alreadyApproved = rec.st.approvedCall(ability, args)
	}
	if !alreadyApproved && rec != nil && rec.def != nil && rec.def.RiskAgentKey != "" {
		if riskDef := u.loadRiskDef(ctx, rec.def.RiskAgentKey); riskDef != nil {
			v, rerr := u.assessRisk(ctx, rec.task, riskDef, uniqueName, args, rec)
			switch {
			case rerr != nil:
				// 评估器不可用 ≠ 没有风险结论。旧实现返回 nil，调用方按「未启用评估」
				// 处理 → 评估服务一挂，全部高危调用直接放行（fail-open）。
				// 现在：有闸门配置就转人工；没有闸门就只能阻止本次调用。
				audit.Denied(ctx, "agentfactory.risk.assess_failed", "capability", ability, rerr.Error())
				rec.event(EventRiskAssess, rec.turn, map[string]any{"tool": uniqueName, "unavailable": true, "error": rerr.Error()})
				if humanGate(rec.def) {
					return "", u.pauseForApproval(ctx, rec, ability, args, "risk_assess_failed", toolHighRisk(ability))
				}
				return "", fmt.Errorf("风险评估服务不可用（%s），本次调用已阻止：该 Agent 未配置人工审批（risk_pause_level=high 或 require_approval），无法转人工。请稍后重试或改用只读方式",
					truncateRunes(rerr.Error(), 200))
			case v != nil && !v.Allow:
				// 高危转人工（O3）：落 checkpoint + paused 事件，以哨兵错误上抛
				if strings.EqualFold(rec.def.RiskPauseLevel, "high") {
					return "", u.pauseForApproval(ctx, rec, ability, args, "risk_assess_high", true)
				}
				return "", fmt.Errorf("操作被风险评估拦截（%s）：%s。请改用更安全的只读方式，或交由人工处理", v.Level, truncateRunes(v.Reason, 200))
			case v != nil && v.Level == "medium":
				riskNote = fmt.Sprintf("[风险提示] 本次操作评估为 medium：%s\n", truncateRunes(v.Reason, 200))
			}
		}
	}
	// 幂等键：任务 + 轮次 + 工具 + 参数摘要。任务重试（失联回收 / 审批续跑）
	// 恢复到同一轮次后 LLM 重发同一调用时，mcp 层会回放上次的成功结果，
	// 拦住重复副作用（语义与边界见 mcp/idem.go）。
	callCtx := ctx
	if rec.task != nil && rec.task.ID > 0 {
		callCtx = mcp.WithIdemKey(ctx, mcp.TaskCallKey(rec.task.ID, rec.turn, uniqueName, args))
	}
	res, err := u.tools.CallTool(callCtx, 0, uniqueName, args)
	if err != nil {
		return "", fmt.Errorf("调用工具 %s 失败: %w", uniqueName, err)
	}
	return riskNote + callResultText(res), nil
}

// skillCapability 调用技能：按技能的执行模式分发。
//
//	local  —— 读取 SKILL.md 说明文本喂回 LLM（技能是指令包，不是可执行程序）
//	remote —— 交给 Skill Runtime 调度 Worker，等结果
//	async  —— 交给 Skill Runtime 入队，返回 execution_id（用 skill:status 查）
//
// 参数约定：`purpose` 是给模型的说明字段，其余字段整体作为技能 input，
// 这样 input_schema 可以直接描述业务参数，模型不需要多包一层 {"input": ..}。
func (u *UseCase) skillCapability(ctx context.Context, name string, args map[string]any, rec *Recorder) (out string, err error) {
	// 技能是本地读库，耗时通常在毫秒级；仍然埋点：这样「这一轮到底调了什么」
	// 在 trace 树里是完整的，不会出现「只有 MCP 有节点、技能凭空消失」。
	ctx, span := trace.Start(ctx, "skill.run",
		trace.String(trace.AttrKind, trace.KindTool),
		trace.String(trace.AttrToolName, CapSkillPrefix+name))
	defer func() { span.End(err) }()

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

	// 可执行技能：交给运行时（local 以外都需要 Worker）
	if sk.Executable() {
		if u.catalog.skillRT == nil {
			return "", fmt.Errorf("技能 %s 配置为 %s，但技能运行时未接线", name, sk.Mode())
		}
		req := skill.RuntimeRequest{
			SkillName: name,
			Input:     skillInput(args),
			Purpose:   purposeOf(args),
		}
		if rec != nil && rec.task != nil {
			req.TaskID = taskIDText(rec.task.ID)
			req.TenantID = rec.task.TenantID
			req.UserID = rec.task.UserID
		}
		if rec != nil && rec.def != nil {
			req.AgentKey = rec.def.AgentKey
		}
		res, err := u.catalog.skillRT.Execute(ctx, req)
		if err != nil {
			return "", fmt.Errorf("执行技能 %s 失败: %w", name, err)
		}
		span.Set("skill.execution_id", res.ExecutionID)
		span.Set("skill.status", res.Status)
		return res.Text, nil
	}

	head := fmt.Sprintf("[技能 %s v%s 已加载]", sk.Name, sk.Version)
	if purpose := purposeOf(args); purpose != "" {
		head += "\n使用目的：" + purpose
	}
	body := sk.Content
	if body == "" {
		body = "（该技能没有 SKILL.md 说明文本）"
	}
	return head + "\n\n" + body, nil
}

// skillStatusCapability 查询异步技能执行状态（Agent 侧唯一的轮询入口）
func (u *UseCase) skillStatusCapability(ctx context.Context, args map[string]any, rec *Recorder) (string, error) {
	if u.catalog == nil || u.catalog.skillRT == nil {
		return "", fmt.Errorf("技能运行时未接线")
	}
	id, _ := args["execution_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("需要 execution_id 参数")
	}
	exec, err := u.catalog.skillRT.GetExecution(ctx, id)
	if err != nil {
		return "", fmt.Errorf("查询执行 %s 失败: %w", id, err)
	}
	// 执行记录里的 tenant 必须与当前上下文一致：execution_id 可被猜到/泄露，
	// 不能靠「不知道别人的 ID」当隔离。任务恢复时 ctx 里的身份可能已换成
	// 平台身份，因此优先用任务自身的租户（它就是提交时的真实归属）。
	tenant := identity.TenantID(ctx)
	if rec != nil && rec.task != nil && rec.task.TenantID != "" {
		tenant = rec.task.TenantID
	}
	if exec.TenantID != "" && exec.TenantID != tenant {
		return "", fmt.Errorf("执行记录 %s 不存在", id)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[技能 %s 执行 %s] status=%s mode=%s", exec.SkillName, exec.ID, exec.Status, exec.Mode)
	if exec.Attempts > 1 {
		fmt.Fprintf(&b, " 尝试=%d", exec.Attempts)
	}
	b.WriteString("\n")
	if exec.Output != "" {
		b.WriteString(truncateRunes(exec.Output, 4000))
		b.WriteString("\n")
	}
	if exec.Error != "" {
		b.WriteString("原因：" + truncateRunes(exec.Error, 600))
	}
	return strings.TrimSpace(b.String()), nil
}

// skillInput 从能力参数里剥出技能入参：去掉 purpose（说明字段）与 execution_id
func skillInput(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		switch k {
		case "purpose", "execution_id":
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func purposeOf(args map[string]any) string {
	s, _ := args["purpose"].(string)
	return strings.TrimSpace(s)
}

func taskIDText(id uint64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatUint(id, 10)
}

// skillArgsExample 远程技能的参数示例：优先用技能声明的 input_schema
func skillArgsExample(spec skill.ExecutionSpec) string {
	if raw := strings.TrimSpace(spec.InputSchema); raw != "" {
		return truncateRunes(raw, 400)
	}
	return `{"purpose":"说明本次调用要做什么；其余字段作为技能参数"}`
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
