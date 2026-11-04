// Package agentfactory 实现「Agent 低代码工厂」：
//
//   - Agent 是一份数据（AgentDef 定义），不是代码：MySQL 存定义，Milvus 存向量做语义召回，Redis 做热缓存；
//   - AgentFactory 把「造 Agent」本身做成能力（create_agent），LLM 可以按需自造 Agent（元编程）；
//   - AgentRuntime 读定义实例化，执行图驱动（react/simple），Agent 本身也是能力（agent:<key>），可被上层委派，实现多 Agent 协作；
//   - 任务（Task）异步执行：Redis 队列 + worker 池，事件流（TaskEvent）记录思考/调用/委派/最终答案，支持链路检索与实时状态。
//
// 护栏见 factory.go：能力白名单、防无限自举、配额、沙箱灰度。
package agentfactory

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// —— 常量 ——

const (
	// SourceManual 人工低代码配置
	SourceManual = "manual"
	// SourceLLM 由 LLM 通过 create_agent 元编程创建
	SourceLLM = "llm"

	// GraphReact ReAct 执行图：思考 → 行动 → 观察 循环
	GraphReact = "react"
	// GraphSimple 单轮直答：一次 LLM 调用，不带工具
	GraphSimple = "simple"

	// —— 单 Agent 推理架构 ——
	// GraphPlanExecute 规划-执行分离：先一次性出完整计划，再逐步执行，步骤失败触发重规划
	GraphPlanExecute = "plan_execute"
	// GraphReflexion 反思型：执行 → 评估打分 → 反思教训 → 带教训重试
	GraphReflexion = "reflexion"
	// GraphRouter 路由分发：先判断意图并路由到技能/成员，不做自由推理循环
	GraphRouter = "router"
	// GraphSelfAsk 自问自答：拆子问题 → 逐个回答 → 汇总
	GraphSelfAsk = "self_ask"
	// GraphFuncCall 原生工具调用：无思考文本，直接选工具并作答（单步 function call）
	GraphFuncCall = "function_call"
	// GraphStateMachine 状态机：固定状态流转，模型只负责状态判断
	GraphStateMachine = "state_machine"

	// —— 多 Agent 架构 ——
	// GraphOrchestrator 协调器：总指挥把子任务派给不同成员并汇总（AutoGen 风格）
	GraphOrchestrator = "orchestrator"
	// GraphHierarchical 分层：规划器拆子目标，成员各自细化执行
	GraphHierarchical = "hierarchical"
	// GraphDebate 辩论：生成方 vs 挑错方多轮对抗，最后裁决收敛
	GraphDebate = "debate"
	// GraphBlackboard 黑板：成员围绕同一块共享状态板读写推进
	GraphBlackboard = "blackboard"

	// GraphAuto 自动编排：任务执行前由 LLM 判定该用哪种执行图、需要几个成员、各自身份，
	// 需要时自动创建子 Agent（role=sub）后再按选定架构执行。
	GraphAuto = "auto"

	// CapFactory create_agent 能力名（LLM 元编程入口）
	CapFactory = "create_agent"
	// CapAgentPrefix 委派 Agent 能力的前缀，全名 agent:<agent_key>
	CapAgentPrefix = "agent:"
	// CapToolPrefix MCP 工具能力前缀，全名 mcp:<unique_name>
	CapToolPrefix = "mcp:"
	// CapSkillPrefix 技能能力前缀，全名 skill:<name>（调用 = 加载技能说明到上下文）
	CapSkillPrefix = "skill:"

	// MiddlewareAudit 审计中间件：能力调用全量落 trace（当前唯一实现，恒开启）
	MiddlewareAudit = "audit"

	DefaultNamespace = "default" // 预留
)

// GraphTypes 全部可用的执行图（架构模式），顺序即前端下拉顺序
var GraphTypes = []string{
	GraphAuto,
	GraphSimple,
	GraphReact,
	GraphPlanExecute,
	GraphReflexion,
	GraphRouter,
	GraphSelfAsk,
	GraphFuncCall,
	GraphStateMachine,
	GraphOrchestrator,
	GraphHierarchical,
	GraphDebate,
	GraphBlackboard,
}

// ValidGraphType 执行图是否受支持
func ValidGraphType(gt string) bool {
	for _, g := range GraphTypes {
		if gt == g {
			return true
		}
	}
	return false
}

// —— Agent 角色 ——

const (
	// RoleRoot 根 Agent：可手动提交 / cron 定时触发，负责编排成员
	RoleRoot = "root"
	// RoleSub 子 Agent：只作为被委派方或被编排成员
	RoleSub = "sub"
	// RoleSupervisor 监察 Agent：审查其他 Agent 的产出，按监察范围生效
	RoleSupervisor = "supervisor"
	// RoleRisk 风险评估 Agent：仅在 MCP 工具调用前评估操作风险
	RoleRisk = "risk"
)

// ValidRole 角色是否合法
func ValidRole(r string) bool {
	switch r {
	case RoleRoot, RoleSub, RoleSupervisor, RoleRisk:
		return true
	}
	return false
}

// —— 成员编排方式 ——

const (
	// OrchestrationAuto 自动：由 ReAct 循环自行决定是否委派（默认，最灵活）
	OrchestrationAuto = "auto"
	// OrchestrationSerial 串行：按 members 顺序逐个执行，后一个可见前一个结果
	OrchestrationSerial = "serial"
	// OrchestrationParallel 并行：members 同时执行，全部完成后汇总
	OrchestrationParallel = "parallel"
)

// ValidOrchestration 编排方式是否合法
func ValidOrchestration(o string) bool {
	switch o {
	case OrchestrationAuto, OrchestrationSerial, OrchestrationParallel:
		return true
	}
	return false
}

// ScopeAll 监察范围通配：监察全部成员
const ScopeAll = "*"

// —— 任务状态 ——

const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	// TaskPaused 挂起：命中 require_approval / 高危风险转人工，等待审批。
	// 审批通过（approve）→ 置回 pending 从 checkpoint 续跑；驳回（reject）→ failed。
	// 挂起任务留在任务池（不归档），RequeueInterrupted 不会误捞（只捞 running）。
	TaskPaused    = "paused"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskCanceled  = "canceled"
)

// ValidTaskStatus 判断任务状态是否合法
func ValidTaskStatus(s string) bool {
	switch s {
	case TaskPending, TaskRunning, TaskPaused, TaskSucceeded, TaskFailed, TaskCanceled:
		return true
	}
	return false
}

// IsTerminalStatus 终态任务（任务池 → 任务历史表的迁移条件）
func IsTerminalStatus(s string) bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCanceled:
		return true
	}
	return false
}

// —— 事件类型（trace 链路） ——

const (
	EventStatus     = "status"      // 状态流转，payload: {"from":"","to":""}
	EventThought    = "thought"     // 一轮思考，payload: {"turn":n,"thought":""}
	EventToolCall   = "tool_call"   // 调用能力，payload: {"turn":n,"name":"","args":{}}
	EventToolResult = "tool_result" // 能力结果，payload: {"turn":n,"elapsed_ms":n,"result":"","is_error":false}
	EventDelegate   = "delegate"    // 委派子 Agent，payload: {"turn":n,"agent":"","child_task_id":n}
	EventSupervise  = "supervise"   // 监察审查，payload: {"agent":"","supervisor":"","passed":bool,"issues":""}
	EventRiskAssess = "risk_assess" // 风险评估，payload: {"tool":"","level":"","allow":bool,"reason":""}
	EventNode       = "node"        // 编排图节点，payload: {"node":"","kind":"","agent":"","status":"","output":"","branch":""}
	EventPlan       = "plan"        // 计划（plan_execute / self_ask / 多 Agent），payload: {"steps":[...]} 或 {"questions":[...]}
	EventEvaluate   = "evaluate"    // 评估打分（reflexion），payload: {"round":n,"pass":bool,"score":n,"issues":""}
	EventReflect    = "reflect"     // 反思教训（reflexion），payload: {"round":n,"lesson":""}
	EventRoute      = "route"       // 路由决策（router），payload: {"target":"","args":{},"reason":""}
	EventOrchestrate = "orchestrate" // 自动编排：LLM 选定架构与成员，payload: {"graph_type":"","reason":"","members":[...],"created":[...],"reused":[...]}
	EventFinal      = "final"       // 最终答案，payload: {"answer":""}
	EventError      = "error"       // 错误，payload: {"message":""}
	// EventPaused 人工审批挂起：payload: {"ability":"","args":{},"reason":""}
	EventPaused = "paused"
	// EventResume 从 checkpoint 续跑：payload: {"seq":n,"cursor":""}
	EventResume = "resume"
	// EventApproval 审批结论：payload: {"decision":"approve|reject","by":"","note":""}
	EventApproval = "approval"
)

// —— 领域错误 ——

var (
	ErrDefNotFound     = fmt.Errorf("Agent 定义不存在")
	ErrDefExists       = fmt.Errorf("Agent 定义已存在")
	ErrInvalidDef      = fmt.Errorf("Agent 定义不合法")
	ErrCapabilityDeny  = fmt.Errorf("能力不在白名单内")
	ErrBootStrapDeny   = fmt.Errorf("禁止自举：LLM 创建的 Agent 不能携带 create_agent 能力")
	ErrQuotaExceeded   = fmt.Errorf("Agent 配额已满")
	ErrTaskNotFound    = fmt.Errorf("任务不存在")
	ErrInvalidTask     = fmt.Errorf("任务不合法")
	ErrTaskStateDeny   = fmt.Errorf("任务当前状态不允许该操作")
	ErrTaskLockBusy    = fmt.Errorf("任务正在其他节点执行（分布式锁未获取）")
	ErrShuttingDown    = fmt.Errorf("服务正在停止，暂不接收新任务")
	ErrNoChatter       = fmt.Errorf("没有可用的 LLM 数据源")
	ErrCapabilityUnset = fmt.Errorf("能力不存在或已停用")
	ErrSandboxDeny     = fmt.Errorf("沙箱 Agent 不允许委派其他 Agent")
	// ErrApprovalNeeded 哨兵错误：能力调用命中人工审批闸门，任务转入 paused。
	// 上层（worker / 父任务）必须用 errors.Is 识别，不得当作普通失败处理。
	ErrApprovalNeeded = fmt.Errorf("等待人工审批")
)

// —— Agent 定义（DO） ——

// AgentDef Agent 的可配置「定义」——Agent 是数据，不是代码。
type AgentDef struct {
	ID uint64 `json:"id"`
	// AgentKey 全局唯一标识（如 research-agent），委派能力名即 agent:<agent_key>
	AgentKey string `json:"agent_key"`
	Name     string `json:"name"`
	// Description 给 LLM 看的能力描述（语义召回的向量化来源）
	Description string `json:"description"`
	// System 角色提示词（人设/规则），会拼进执行图的 system 段
	System string `json:"system"`
	// LLMModel 指定模型，空 = 使用平台默认数据源
	LLMModel string `json:"llm_model"`
	// LLMSource 默认模型数据源（llm 数据源名，空 = 平台默认）；该源调用失败自动回退 default
	LLMSource string `json:"llm_source"`
	// GraphType 执行图（架构模式）：auto | simple | react | plan_execute | reflexion | router |
	// self_ask | function_call | state_machine | orchestrator | hierarchical | debate | blackboard
	GraphType string `json:"graph_type"`
	// AutoCompose 自动装配：每次任务执行前由 LLM 判定架构与成员，必要时自动创建子 Agent。
	// graph_type=auto 时恒为真；多 Agent 图但 members 为空时也会自动触发一次。
	AutoCompose bool `json:"auto_compose"`
	// Role 角色：root | sub | supervisor | risk
	Role string `json:"role"`
	// Orchestration 成员编排方式：auto | serial | parallel（仅 root 生效）
	Orchestration string `json:"orchestration"`
	// Members 编排成员（agent_key 列表）：serial 按数组顺序执行，parallel 并发执行
	Members []string `json:"members"`
	// Flow 编排画布布局（JSON 字符串，仅前端渲染用）：
	// {"positions":{"<agent_key>":{"x":120,"y":112}}}
	Flow string `json:"flow"`
	// SupervisorKey 监察 Agent（role=supervisor 的 agent_key），空 = 不监察
	SupervisorKey string `json:"supervisor_key"`
	// SupervisionScope 监察范围（仅 supervisor 角色使用）：
	// 要审查的 agent_key 列表；空或包含 "*" = 全部成员
	SupervisionScope []string `json:"supervision_scope"`
	// RiskAgentKey 风险评估 Agent（role=risk 的 agent_key）：
	// 仅在该 Agent 调用 MCP 工具前触发，空 = 不做风险评估
	RiskAgentKey string `json:"risk_agent_key"`
	// RequireApproval 需人工审批的能力前缀（如 "mcp:mail_send"、"mcp:payment_*"）：
	// 命中的能力调用前任务暂停（paused），批准后从 checkpoint 续跑；空 = 不需要审批
	RequireApproval []string `json:"require_approval"`
	// ApprovalTimeoutHours 审批超时（小时），超时自动驳回；0 = 用默认 72h
	ApprovalTimeoutHours int `json:"approval_timeout_hours"`
	// RiskPauseLevel 风险评估转人工阈值：none（默认，沿用拦截/放行）| high（高危转人工审批）
	RiskPauseLevel string `json:"risk_pause_level"`
	// AutoRun 是否后台自动运行（仅 root 生效，需配合 CronExpr）
	AutoRun bool `json:"auto_run"`
	// CronExpr 5 段 cron 表达式（分 时 日 月 周）
	CronExpr string `json:"cron_expr"`
	// CronInput 定时运行使用的固定输入（AutoRun 时必填）
	CronInput string `json:"cron_input"`
	// NextRunAt 下次自动运行时间（调度器维护）
	NextRunAt time.Time `json:"next_run_at"`
	// LastRunAt 上次自动运行时间
	LastRunAt time.Time `json:"last_run_at"`
	// Capabilities 能力白名单（权限边界）：create_agent / agent:<key> / mcp:<unique_name>
	Capabilities []string `json:"capabilities"`
	// Middlewares 中间件：audit（恒开启，字段为前向兼容）
	Middlewares []string `json:"middlewares"`
	// MaxTurns 执行图最大轮数
	MaxTurns int `json:"max_turns"`
	// Sandbox 沙箱灰度：不允许委派其他 Agent，需人工验证后关闭
	Sandbox bool `json:"sandbox"`
	// Source 来源：manual | llm
	Source string `json:"source"`
	// Version 定义版本号，每次更新 +1（为回滚预留）
	Version int `json:"version"`
	// Enabled 启用状态，停用后不可被委派/召回
	Enabled bool `json:"enabled"`
	// Vectorized 是否已写入向量注册表
	Vectorized bool `json:"vectorized"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EmbeddingText 语义召回的向量化文本
func (d AgentDef) EmbeddingText() string {
	var b strings.Builder
	b.WriteString("Agent: ")
	b.WriteString(d.Name)
	b.WriteString("\n标识: ")
	b.WriteString(d.AgentKey)
	b.WriteString("\n描述: ")
	b.WriteString(d.Description)
	b.WriteString("\n执行图: ")
	b.WriteString(d.GraphType)
	if d.Role != "" {
		b.WriteString("\n角色: ")
		b.WriteString(d.Role)
	}
	if len(d.Members) > 0 {
		b.WriteString("\n协作成员: ")
		b.WriteString(strings.Join(d.Members, ", "))
	}
	if d.SupervisorKey != "" {
		b.WriteString("\n监察: ")
		b.WriteString(d.SupervisorKey)
	}
	if len(d.Capabilities) > 0 {
		b.WriteString("\n能力: ")
		b.WriteString(strings.Join(d.Capabilities, ", "))
	}
	return b.String()
}

// AgentInput 创建/更新定义的入参
type AgentInput struct {
	AgentKey     string   `json:"agent_key"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	System       string   `json:"system"`
	LLMModel     string   `json:"llm_model"`
	LLMSource    string   `json:"llm_source"`
	GraphType    string   `json:"graph_type"`
	// AutoCompose 自动装配（见 AgentDef.AutoCompose）
	AutoCompose bool     `json:"auto_compose"`
	Capabilities []string `json:"capabilities"`
	Middlewares  []string `json:"middlewares"`
	MaxTurns     int      `json:"max_turns"`
	Sandbox      bool     `json:"sandbox"`

	// 角色与编排
	Role             string   `json:"role"`
	Orchestration    string   `json:"orchestration"`
	Members          []string `json:"members"`
	Flow             string   `json:"flow"`
	SupervisorKey    string   `json:"supervisor_key"`
	SupervisionScope []string `json:"supervision_scope"`
	RiskAgentKey     string   `json:"risk_agent_key"`
	// RequireApproval 需人工审批的能力前缀（支持尾部 * 通配）
	RequireApproval []string `json:"require_approval"`
	// ApprovalTimeoutHours 审批超时（小时），0 = 默认 72h
	ApprovalTimeoutHours int `json:"approval_timeout_hours"`
	// RiskPauseLevel 风险转人工阈值：none | high
	RiskPauseLevel string `json:"risk_pause_level"`

	// 后台自动运行
	AutoRun   bool   `json:"auto_run"`
	CronExpr  string `json:"cron_expr"`
	CronInput string `json:"cron_input"`
}

// Normalize 格式化 + 基础校验（白名单校验在 UseCase 层，需要注册表）
func (in *AgentInput) Normalize(defaultMaxTurns int) error {
	in.AgentKey = strings.TrimSpace(strings.ToLower(in.AgentKey))
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.System = strings.TrimSpace(in.System)
	in.LLMModel = strings.TrimSpace(in.LLMModel)
	in.LLMSource = strings.TrimSpace(in.LLMSource)
	if utf8.RuneCountInString(in.LLMSource) > 64 {
		return fmt.Errorf("%w: llm_source 过长（≤64 字符）", ErrInvalidDef)
	}
	in.Flow = strings.TrimSpace(in.Flow)
	if utf8.RuneCountInString(in.Flow) > 40000 {
		return fmt.Errorf("%w: 编排图过大（≤40000 字符）", ErrInvalidDef)
	}
	if g := ParseFlowGraph(in.Flow); g != nil {
		if err := g.Validate(); err != nil {
			return err
		}
	}
	in.GraphType = strings.TrimSpace(strings.ToLower(in.GraphType))
	if in.GraphType == "" {
		in.GraphType = GraphReact
	}
	if !ValidGraphType(in.GraphType) {
		return fmt.Errorf("%w: 执行图 %q 不支持（可选 %s）", ErrInvalidDef, in.GraphType, strings.Join(GraphTypes, " / "))
	}
	// 自动图意味着「运行时由 LLM 决定架构」，自动装配开关恒为真
	if in.GraphType == GraphAuto {
		in.AutoCompose = true
	}
	if in.AgentKey == "" && in.Name != "" {
		in.AgentKey = SlugKey(in.Name)
	}
	if !validKey(in.AgentKey) {
		return fmt.Errorf("%w: 标识 %q 需匹配小写字母开头，仅含小写字母/数字/-/_，2~64 位", ErrInvalidDef, in.AgentKey)
	}
	if in.Name == "" {
		return fmt.Errorf("%w: 名称必填", ErrInvalidDef)
	}
	if utf8.RuneCountInString(in.Name) > 64 {
		return fmt.Errorf("%w: 名称过长（≤64 字）", ErrInvalidDef)
	}
	if utf8.RuneCountInString(in.Description) > 1024 {
		return fmt.Errorf("%w: 描述过长（≤1024 字）", ErrInvalidDef)
	}
	if utf8.RuneCountInString(in.System) > 6000 {
		return fmt.Errorf("%w: 角色提示词过长（≤6000 字）", ErrInvalidDef)
	}
	if utf8.RuneCountInString(in.LLMModel) > 128 {
		return fmt.Errorf("%w: 模型名过长", ErrInvalidDef)
	}
	in.Capabilities = cleanStrings(in.Capabilities)
	if len(in.Capabilities) > 64 {
		return fmt.Errorf("%w: 能力白名单过多（≤64 项）", ErrInvalidDef)
	}
	in.Middlewares = cleanStrings(in.Middlewares)
	for _, m := range in.Middlewares {
		if m != MiddlewareAudit {
			return fmt.Errorf("%w: 未知中间件 %q", ErrInvalidDef, m)
		}
	}
	// 护栏 4：沙箱 Agent 不允许委派能力（权限最小化，防绕过隔离）
	if err := in.ValidateSandboxCaps(); err != nil {
		return err
	}

	// —— 角色 ——
	in.Role = strings.ToLower(strings.TrimSpace(in.Role))
	if in.Role == "" {
		in.Role = RoleRoot
	}
	if !ValidRole(in.Role) {
		return fmt.Errorf("%w: 角色 %q 不支持（可选 root / sub / supervisor / risk）", ErrInvalidDef, in.Role)
	}

	// —— 编排方式 ——
	in.Orchestration = strings.ToLower(strings.TrimSpace(in.Orchestration))
	if in.Orchestration == "" {
		in.Orchestration = OrchestrationAuto
	}
	if !ValidOrchestration(in.Orchestration) {
		return fmt.Errorf("%w: 编排方式 %q 不支持（可选 auto / serial / parallel）", ErrInvalidDef, in.Orchestration)
	}
	in.Members = lowerKeys(cleanStrings(in.Members))
	if len(in.Members) > 20 {
		return fmt.Errorf("%w: 协作成员过多（≤20 个）", ErrInvalidDef)
	}
	for _, m := range in.Members {
		if m == in.AgentKey {
			return fmt.Errorf("%w: 协作成员不能包含自身（%s）", ErrInvalidDef, m)
		}
	}
	in.SupervisorKey = strings.ToLower(strings.TrimSpace(in.SupervisorKey))
	in.RiskAgentKey = strings.ToLower(strings.TrimSpace(in.RiskAgentKey))
	in.SupervisionScope = lowerKeys(cleanStrings(in.SupervisionScope))
	// 只有根 Agent 具备「编排 / 定时」语义；其他角色忽略这些字段，避免误配
	if in.Role != RoleRoot {
		in.Members = nil
		in.AutoRun = false
		in.CronExpr = ""
		in.CronInput = ""
	}
	if in.SupervisorKey != "" && in.SupervisorKey == in.AgentKey {
		return fmt.Errorf("%w: 监察 Agent 不能是自己", ErrInvalidDef)
	}
	if in.RiskAgentKey != "" && in.RiskAgentKey == in.AgentKey {
		return fmt.Errorf("%w: 风险评估 Agent 不能是自己", ErrInvalidDef)
	}

	// —— 人工审批（O3）——
	in.RequireApproval = cleanStrings(in.RequireApproval)
	if in.ApprovalTimeoutHours < 0 {
		in.ApprovalTimeoutHours = 0
	}
	if in.ApprovalTimeoutHours > 24*30 {
		return fmt.Errorf("%w: 审批超时不能超过 720 小时", ErrInvalidDef)
	}
	in.RiskPauseLevel = strings.ToLower(strings.TrimSpace(in.RiskPauseLevel))
	switch in.RiskPauseLevel {
	case "", "none", "high":
	default:
		return fmt.Errorf("%w: risk_pause_level 仅支持 none / high", ErrInvalidDef)
	}

	// —— 后台自动运行（cron）——
	in.CronExpr = strings.TrimSpace(in.CronExpr)
	in.CronInput = strings.TrimSpace(in.CronInput)
	if in.AutoRun {
		if in.CronExpr == "" {
			return fmt.Errorf("%w: 开启后台自动运行需填写 cron 表达式（如 0 9 * * *）", ErrInvalidDef)
		}
		if err := ValidateCron(in.CronExpr); err != nil {
			return fmt.Errorf("%w: cron 表达式不合法: %v", ErrInvalidDef, err)
		}
		if in.CronInput == "" {
			return fmt.Errorf("%w: 开启后台自动运行需填写定时运行的输入内容", ErrInvalidDef)
		}
		if utf8.RuneCountInString(in.CronInput) > 4000 {
			return fmt.Errorf("%w: 定时输入过长（≤4000 字）", ErrInvalidDef)
		}
	}

	if in.MaxTurns <= 0 {
		in.MaxTurns = defaultMaxTurns
	}
	if in.MaxTurns > 40 {
		in.MaxTurns = 40
	}
	return nil
}

// ValidateSandboxCaps 沙箱能力边界校验：沙箱 Agent 的白名单里不能出现
// 委派能力（agent:<agent_key>），否则沙箱内 Agent 可以借委派绕开隔离。
// 报错给出两条可执行出路，避免用户卡在保存按钮上。
func (in AgentInput) ValidateSandboxCaps() error {
	if !in.Sandbox {
		return nil
	}
	for _, c := range in.Capabilities {
		if strings.HasPrefix(c, CapAgentPrefix) {
			return fmt.Errorf("%w: 沙箱 Agent 不能包含委派能力 %s（取消勾选「沙箱灰度」后可委派，或从能力白名单移除该项）", ErrInvalidDef, c)
		}
	}
	return nil
}

// SlugKey 把名称转成合法 key（用于 LLM 起名不规范时兜底）
func SlugKey(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func validKey(k string) bool {
	if n := len(k); n < 2 || n > 64 {
		return false
	}
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func cleanStrings(list []string) []string {
	out := make([]string, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// lowerKeys 统一小写（agent_key 大小写不敏感）
func lowerKeys(list []string) []string {
	for i, s := range list {
		list[i] = strings.ToLower(strings.TrimSpace(s))
	}
	return list
}

// —— 定义列表过滤 ——

type DefFilter struct {
	Keyword string
	Source  string // manual | llm | 空
	Enabled *bool
	IDs     []uint64
	Limit   int
}

// —— 能力 ——

// CapabilitySchema 能力的自描述（喂给 LLM 选择能力 + 前端展示）
type CapabilitySchema struct {
	Name string `json:"name"` // create_agent / agent:<key> / mcp:<unique_name>
	Type string `json:"type"` // factory | agent | mcp
	// Description 给 LLM 的能力说明
	Description string `json:"description"`
	// ArgsExample 参数示例（JSON 对象字符串）
	ArgsExample string `json:"args_example"`
	// SandboxOnly 仅沙箱外的 Agent 可用（当前无此约束，预留）
	Disabled bool `json:"disabled"`
}

// —— 任务 ——

// Task 一次 Agent 执行（异步 worker 的基本单位）
type Task struct {
	ID         uint64 `json:"id"`
	AgentDefID uint64 `json:"agent_def_id"`
	AgentKey   string `json:"agent_key"`
	// Title 输入摘要（列表展示）
	Title string `json:"title"`
	// Input 用户输入
	Input string `json:"input"`
	// Status: pending | running | succeeded | failed | canceled
	Status string `json:"status"`
	// Progress 0~100
	Progress int    `json:"progress"`
	Result   string `json:"result"`
	ErrorMsg string `json:"error_msg"`
	// ParentTaskID 父任务（多 Agent 协作的链路树；0 = 根任务）
	ParentTaskID uint64 `json:"parent_task_id"`
	// Depth 委派深度（根=0）
	Depth int `json:"depth"`
	// ConvID 来源会话（会话 Chat 编排调用 Agent 时写入；空 = 控制台/独立任务）
	ConvID string `json:"conv_id"`
	// UserID 归属用户（隔离键）：由提交时的请求身份写入，跨用户不可见/不可操作
	UserID string `json:"user_id"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Archived 是否已归档到任务历史表（agent_task_history）：
	// 任务池只存 pending/running，终态任务落历史表，此处由仓储按来源表标注
	Archived bool `json:"archived"`
}

// TaskEvent 任务事件（trace 链路检索单元）
type TaskEvent struct {
	ID     uint64 `json:"id"`
	TaskID uint64 `json:"task_id"`
	Turn   int    `json:"turn"`
	Type   string `json:"type"`
	// Payload JSON 字符串
	Payload   string    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskCheckpoint 步骤级快照：每个"步骤边界"（计划步骤 / 波次 / 辩论轮次 / 工具循环轮 /
// 审批挂起点）写一条，任务恢复时取 seq 最大的一条回放 RunState。
// 只有多步图（plan_execute / orchestrator / workflow / debate / blackboard）与
// 开了人工审批的 Agent 才写，react 等单循环图不写（重放代价低于落盘代价）。
type TaskCheckpoint struct {
	ID     uint64 `json:"id"`
	TaskID uint64 `json:"task_id"`
	// Seq 任务内递增序号（恢复时取最大）
	Seq       int    `json:"seq"`
	GraphType string `json:"graph_type"`
	// Cursor 恢复点描述（JSON）：{"kind":"plan_step|wave|round|loop|approval","index":n,"ability":""}
	Cursor string `json:"cursor"`
	// State RunState JSON（执行状态即真相）
	State string `json:"state"`
	// Tokens 落盘时已消耗 token（记账观测用）
	Tokens    int64     `json:"tokens"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskFilter 任务列表过滤
type TaskFilter struct {
	AgentDefID uint64
	AgentKey   string
	Status     string
	Keyword    string
	// ParentTaskID >0 时只列出该父任务的直接子任务
	ParentTaskID uint64
	// RootOnly 只列出根任务（parent_task_id = 0）；任务中心列表用，
	// 子任务通过链路详情中的委派事件进入
	RootOnly bool
	// ConvID 按来源会话 ID 过滤（会话链路查询该会话编排产生的任务）
	ConvID string
	// UserID 归属用户过滤（空 = 不过滤，仅内部链路/运维查询使用）
	UserID string
	Limit  int
}

// TaskInput 提交任务的入参
type TaskInput struct {
	AgentDefID uint64 `json:"agent_def_id"`
	AgentKey   string `json:"agent_key"`
	Input      string `json:"input"`
	// ParentTaskID 由内部委派时填写，对外提交恒为 0
	ParentTaskID uint64 `json:"-"`
	// ConvID 来源会话 ID（会话编排调用 Agent 时填写，任务链路与会话链路共用）
	ConvID string `json:"-"`
}

// Normalize 校验任务入参
func (in *TaskInput) Normalize() error {
	in.AgentKey = strings.TrimSpace(strings.ToLower(in.AgentKey))
	in.Input = strings.TrimSpace(in.Input)
	if in.AgentDefID == 0 && in.AgentKey == "" {
		return fmt.Errorf("%w: agent_def_id 与 agent_key 至少填一个", ErrInvalidTask)
	}
	if in.Input == "" {
		return fmt.Errorf("%w: 输入内容必填", ErrInvalidTask)
	}
	if utf8.RuneCountInString(in.Input) > 8000 {
		return fmt.Errorf("%w: 输入内容过长（≤8000 字）", ErrInvalidTask)
	}
	return nil
}

// —— 向量过滤 ——

type VectorFilter struct {
	Enabled *bool
	Source  string
}

type VectorHit struct {
	AgentID uint64  `json:"agent_id"`
	Score   float64 `json:"score"`
}

// —— 统计 ——

type Stats struct {
	Agents        int64 `json:"agents"`
	EnabledAgents int64 `json:"enabled_agents"`
	LLMBorn       int64 `json:"llm_born"`
	Sandboxed     int64 `json:"sandboxed"`
	Vectorized    int64 `json:"vectorized"`
	Tasks         int64 `json:"tasks"`
	Pending       int64 `json:"pending"`
	Running       int64 `json:"running"`
	Succeeded     int64 `json:"succeeded"`
	Failed        int64 `json:"failed"`
	Canceled      int64 `json:"canceled"`
	Capabilities  int64 `json:"capabilities"`
}
