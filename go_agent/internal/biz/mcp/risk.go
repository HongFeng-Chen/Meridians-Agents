package mcp

// MCP 工具的风险分级。
//
// 为什么需要分级：工具的语义完全由远端决定——`delete_repository`、`execute_sql`、
// `drop_table` 这类工具一次调用就能造成不可逆损失，而平台此前把它们和
// `list_repos` 一样「一律放开」。管理面（/api/mcp/tools/{id}/call）是给人调试用的，
// 不该成为绕过审批的后门；Agent 运行期的审批闸门（AgentDef.require_approval）
// 也应该用同一套口径判断「这个能力是不是高危」。
//
// 分级来源（按可信度从高到低）：
//  1. 显式标签（tool.tags 里写 read/write/destructive）——人已经判断过，直接采信；
//  2. proxy 工具的路由方法（GET/HEAD → 读；PUT/PATCH → 写；DELETE → 高危）；
//  3. 工具名与描述里的动词关键词（delete/drop/truncate/exec…）。
//
// 默认值刻意取 write 而不是 read：未知工具里混着大量写操作，猜 read 等于放行。

import (
	"encoding/json"
	"strings"
)

// RiskLevel 工具风险等级
type RiskLevel string

const (
	// RiskRead 只读：查询、列表、详情。审批策略下可直接调用。
	RiskRead RiskLevel = "read"
	// RiskWrite 写：创建/修改/发送/上传。可回滚或有副作用，按策略可能需要审批。
	RiskWrite RiskLevel = "write"
	// RiskDestructive 高危：删除/覆盖/执行代码或 SQL/改权限。默认必须审批。
	RiskDestructive RiskLevel = "destructive"
)

// Rank 等级排序（阈值比较用）
func (r RiskLevel) Rank() int {
	switch r {
	case RiskRead:
		return 1
	case RiskWrite:
		return 2
	case RiskDestructive:
		return 3
	default:
		return 0
	}
}

// Valid 是否为已知等级
func (r RiskLevel) Valid() bool { return r.Rank() > 0 }

// ParseRiskLevel 解析等级字符串（大小写不敏感）
func ParseRiskLevel(s string) (RiskLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read", "readonly", "read_only", "r":
		return RiskRead, true
	case "write", "rw", "w":
		return RiskWrite, true
	case "destructive", "danger", "dangerous", "high", "d":
		return RiskDestructive, true
	}
	return "", false
}

// destructiveKeywords 高危动词。宁可多拦：误判的代价是「多点一次审批」，
// 漏判的代价是「一条 SQL 删掉一张表」。
var destructiveKeywords = []string{
	"delete", "remove", "destroy", "drop", "truncate", "purge", "wipe", "erase",
	"reset", "revoke", "kill", "terminate", "shutdown", "disable",
	"exec", "execute", "shell", "command", "run_command", "eval", "spawn",
	"deploy", "rollback", "restore", "overwrite", "format",
	"删除", "清空", "销毁", "重置", "执行", "回滚", "覆盖",
}

// writeKeywords 写操作动词
var writeKeywords = []string{
	"create", "add", "insert", "update", "set", "put", "patch", "modify", "edit",
	"write", "upload", "import", "send", "post", "publish", "submit", "apply",
	"enable", "approve", "grant", "assign", "move", "copy", "rename", "merge",
	"创建", "新增", "修改", "更新", "写入", "上传", "发送", "提交", "发布", "启用",
}

// readonlyKeywords 只读动词（用于反向确认，避免 "get_or_create" 这类复合名判错）
var readonlyKeywords = []string{
	"get", "list", "search", "query", "read", "fetch", "describe", "show",
	"find", "lookup", "count", "stat", "stats", "status", "info", "detail",
	"查询", "列表", "获取", "搜索", "详情",
}

// ClassifyToolRisk 判定工具风险等级。
func ClassifyToolRisk(t Tool) RiskLevel {
	// 1) 显式标签优先
	for _, tag := range t.Tags {
		if lv, ok := ParseRiskLevel(tag); ok {
			return lv
		}
	}

	// 2) proxy 工具：HTTP 方法是最可靠的语义信号
	if method := proxyRouteMethod(t.Route); method != "" {
		switch method {
		case "GET", "HEAD", "OPTIONS":
			// GET 也可能被远端实现成「有副作用的查询」，再用名称兜一层
			if hitAny(t.ToolName, destructiveKeywords) {
				return RiskDestructive
			}
			return RiskRead
		case "DELETE":
			return RiskDestructive
		case "PUT", "PATCH":
			if hitAny(t.ToolName, destructiveKeywords) {
				return RiskDestructive
			}
			return RiskWrite
		default: // POST 及未知方法
			if hitAny(t.ToolName, destructiveKeywords) {
				return RiskDestructive
			}
			return RiskWrite
		}
	}

	// 3) 名称 + 描述关键词
	name := strings.ToLower(t.ToolName + " " + t.UniqueName + " " + t.DisplayName)
	if hitAny(name, destructiveKeywords) {
		return RiskDestructive
	}
	// 复合名（get_or_create）：只读动词在前、且没有其余写动词时仍判 write，
	// 因为它确实可能写入；这里只在「纯只读」时才降级
	if hitAny(name, writeKeywords) {
		return RiskWrite
	}
	if hitAny(name, readonlyKeywords) {
		return RiskRead
	}
	if desc := strings.ToLower(t.Description); desc != "" {
		if hitAny(desc, destructiveKeywords) {
			return RiskDestructive
		}
	}
	// 4) 未知：按写操作处理（猜 read 等于放行）
	return RiskWrite
}

// proxyRouteMethod 读取 proxy 工具的 route JSON 里的 method
func proxyRouteMethod(route string) string {
	raw := strings.TrimSpace(route)
	if raw == "" {
		return ""
	}
	var r struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(r.Method))
}

// hitAny 任一关键词命中即返回 true（按子串匹配，兼容 delete_user / userDelete）
func hitAny(s string, keywords []string) bool {
	if s == "" {
		return false
	}
	s = strings.ToLower(s)
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
