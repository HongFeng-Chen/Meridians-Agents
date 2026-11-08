package agentfactory

// 编排 DAG（flow v2）：节点 + 有向边，天然支持串并行混排。
//
//	root ──▶ t1 ──┬──▶ t2 ──┐
//	              ├──▶ t3 ──┼──▶ 条件 ──(是)──▶ t5 ──▶ 输出
//	              └──▶ t4 ──┘         └──(否)──▶ t6 ──┘
//
// 语义：
//   · 一个节点有多条出边 = 扇出并行；多条入边 = 汇聚，等全部上游完成后一起执行
//   · 条件节点只激活命中的分支，未命中的分支整段跳过
//   · 入度为 0 的节点从任务输入开始执行（即 root 的入口）
//
// flow 里可能只有旧版的 {"positions":{...}}（画布坐标），此时 ParseFlowGraph 返回
// nil，运行时回落到 members + orchestration 的老编排路径。

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 节点类型
const (
	FlowNodeAgent     = "agent"     // 执行一个 Agent
	FlowNodeCondition = "condition" // 条件判断，按分支放行
	FlowNodeOutput    = "output"    // 汇总输出
)

// 条件判断方式
const (
	CondModeRule = "rule" // 规则匹配
	CondModeLLM  = "llm"  // 交给模型判断
)

// 规则运算符
const (
	CondOpContains    = "contains"
	CondOpNotContains = "not_contains"
	CondOpNotEmpty    = "not_empty"
	CondOpIsEmpty     = "is_empty"
	CondOpGt          = "gt"
	CondOpLt          = "lt"
)

// FlowCond 条件节点配置
type FlowCond struct {
	// Mode rule | llm
	Mode string `json:"mode"`
	// Op 规则运算符（Mode=rule 时生效）
	Op string `json:"op"`
	// Value 规则比较值（Op 需要时使用）
	Value string `json:"value"`
	// Question LLM 判断的问题（Mode=llm 时生效）
	Question string `json:"question"`
}

// FlowNode DAG 节点
type FlowNode struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	AgentKey string    `json:"agent_key,omitempty"`
	Label    string    `json:"label,omitempty"`
	X        float64   `json:"x"`
	Y        float64   `json:"y"`
	Cond     *FlowCond `json:"cond,omitempty"`
}

// FlowEdge 有向边。When 决定这条边在源节点什么状态下放行：
//
//	""/"success"  源节点成功（默认）
//	"true"/"false" 条件节点的命中 / 未命中分支
//	"failure"     源节点失败（on_error 分支）
//	"always"      无论成功失败都放行
//
// 失败不会默认流向下游：Agent 失败的产出是 "[执行失败] …" 文本，
// 让它继续流下去等于下游拿着报错当输入干活。
type FlowEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	When string `json:"when,omitempty"`
}

// FlowGraph 编排图
type FlowGraph struct {
	Nodes []FlowNode `json:"nodes"`
	Edges []FlowEdge `json:"edges"`
}

// ParseFlowGraph 解析 flow 字段：只有旧版坐标（无 nodes）时返回 nil
func ParseFlowGraph(raw string) *FlowGraph {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.Contains(raw, `"nodes"`) {
		return nil
	}
	var g FlowGraph
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		return nil
	}
	if len(g.Nodes) == 0 {
		return nil
	}
	return &g
}

// Validate 结构校验：环、悬空边、输出节点唯一、条件配置完整
func (g *FlowGraph) Validate() error {
	if len(g.Nodes) > 60 {
		return fmt.Errorf("%w: 编排节点过多（≤60）", ErrInvalidDef)
	}
	byID := make(map[string]*FlowNode, len(g.Nodes))
	outputs := 0
	for i := range g.Nodes {
		n := &g.Nodes[i]
		n.ID = strings.TrimSpace(n.ID)
		if n.ID == "" {
			return fmt.Errorf("%w: 编排节点缺少 id", ErrInvalidDef)
		}
		if _, dup := byID[n.ID]; dup {
			return fmt.Errorf("%w: 编排节点 id 重复：%s", ErrInvalidDef, n.ID)
		}
		byID[n.ID] = n
		switch n.Kind {
		case FlowNodeAgent, "":
			n.Kind = FlowNodeAgent
			if strings.TrimSpace(n.AgentKey) == "" {
				return fmt.Errorf("%w: 节点 %s 未指定 Agent", ErrInvalidDef, n.ID)
			}
		case FlowNodeCondition:
			if n.Cond == nil {
				return fmt.Errorf("%w: 条件节点 %s 缺少判断配置", ErrInvalidDef, n.ID)
			}
			if n.Cond.Mode == "" {
				n.Cond.Mode = CondModeRule
			}
		case FlowNodeOutput:
			outputs++
		default:
			return fmt.Errorf("%w: 未知节点类型 %q", ErrInvalidDef, n.Kind)
		}
	}
	if outputs == 0 {
		return fmt.Errorf("%w: 编排图缺少「输出」节点", ErrInvalidDef)
	}
	if outputs > 1 {
		return fmt.Errorf("%w: 编排图只能有一个「输出」节点", ErrInvalidDef)
	}

	// 边校验 + 邻接表
	out := make(map[string][]string, len(g.Nodes))
	indeg := make(map[string]int, len(g.Nodes))
	seen := make(map[string]bool, len(g.Edges))
	for i := range g.Edges {
		e := &g.Edges[i]
		if _, ok := byID[e.From]; !ok {
			return fmt.Errorf("%w: 连线起点不存在：%s", ErrInvalidDef, e.From)
		}
		if _, ok := byID[e.To]; !ok {
			return fmt.Errorf("%w: 连线终点不存在：%s", ErrInvalidDef, e.To)
		}
		if e.From == e.To {
			return fmt.Errorf("%w: 不允许自连（%s）", ErrInvalidDef, e.From)
		}
		if e.When = strings.ToLower(strings.TrimSpace(e.When)); e.When != "" {
			switch e.When {
			case "true", "false":
				// 条件分支：只允许出现在条件节点的出边
				if byID[e.From].Kind != FlowNodeCondition {
					return fmt.Errorf("%w: 节点 %s 不是条件节点，出边不能标 true/false", ErrInvalidDef, e.From)
				}
			case "success", "failure", "always":
				// 状态边：见 FlowEdge 注释
			default:
				return fmt.Errorf("%w: 出边 when 只能是 success / failure / always / true / false", ErrInvalidDef)
			}
		}
		key := e.From + "->" + e.To + "#" + e.When
		if seen[key] {
			return fmt.Errorf("%w: 重复连线 %s → %s", ErrInvalidDef, e.From, e.To)
		}
		seen[key] = true
		out[e.From] = append(out[e.From], e.To)
		indeg[e.To]++
	}
	if len(indeg) == 0 && len(g.Nodes) > 1 {
		return fmt.Errorf("%w: 编排图没有连线", ErrInvalidDef)
	}

	// 环检测（Kahn）
	queue := make([]string, 0, len(g.Nodes))
	deg := make(map[string]int, len(g.Nodes))
	for _, n := range g.Nodes {
		deg[n.ID] = indeg[n.ID]
		if deg[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	visited := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		visited++
		for _, to := range out[cur] {
			deg[to]--
			if deg[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	if visited != len(g.Nodes) {
		return fmt.Errorf("%w: 编排图存在环，请检查连线方向", ErrInvalidDef)
	}
	return nil
}

// evalCond 执行条件判断，返回是否命中「是」分支
func evalCond(c *FlowCond, text string) bool {
	if c == nil {
		return true
	}
	text = strings.TrimSpace(text)
	switch c.Mode {
	case CondModeLLM:
		// LLM 判断由调用方注入（需要模型能力），这里只做规则兜底
		return len([]rune(text)) > 0
	}
	switch c.Op {
	case CondOpContains:
		return strings.Contains(text, c.Value)
	case CondOpNotContains:
		return !strings.Contains(text, c.Value)
	case CondOpNotEmpty:
		return text != ""
	case CondOpIsEmpty:
		return text == ""
	case CondOpGt, CondOpLt:
		n, err := strconv.ParseFloat(strings.TrimSpace(firstNumber(text)), 64)
		if err != nil {
			return false
		}
		limit, err := strconv.ParseFloat(strings.TrimSpace(c.Value), 64)
		if err != nil {
			return false
		}
		if c.Op == CondOpGt {
			return n > limit
		}
		return n < limit
	default:
		return text != ""
	}
}

// firstNumber 取文本里第一个数字（用于 gt/lt 比较）
func firstNumber(s string) string {
	start := -1
	for i, r := range s {
		if (r >= '0' && r <= '9') || r == '-' || r == '.' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			return s[start:i]
		}
	}
	if start >= 0 {
		return s[start:]
	}
	return ""
}
