package mcp

// 配置文件（config.yaml mcp.stdio / mcp.remote 段）→ 进程内策略 的启动装配。
//
// 背景策略本身（egress / stdio_policy）只认环境变量、进程启动时读取一次；
// 本文件把它们接到统一配置上：main.go 加载配置后调用 ApplyConfig，
// 策略立即生效，后续所有 MCP 客户端（含 Manager 常驻连接）都走新策略。
//
// 优先级语义：config.yaml 里的值用 ${ENV:default} 占位符书写，
// 环境变量优先、缺省值兜底——所以「配置文件有默认值」就意味着
// 即使终端没带环境变量（如新开的终端、服务方式启动），策略也有合理缺省。
//
// 兜底规则：配置段缺失（nil）时不动当前策略，保持 loadPolicy()
// 从环境变量读到的结果，向后兼容纯环境变量部署。

import (
	"strconv"
	"strings"

	"go_agent/internal/conf"
	"go_agent/internal/pkg/egress"
)

// ApplyConfig 应用配置文件里的 MCP 策略段（stdio + remote 出站）。
func ApplyConfig(c *conf.Data) {
	if c == nil {
		return
	}
	applyStdioConfig(c.GetMcp().GetStudio())
	applyRemoteConfig(c.GetMcp().GetRemote())
}

// applyStdioConfig mcp.stdio 段 → StdioPolicy
func applyStdioConfig(s *conf.Data_Mcp_Studio) {
	if s == nil {
		return
	}
	p := StdioPolicy{
		Enabled:         s.GetEnabled(),
		AllowedCommands: map[string]bool{},
		AllowedDirs:     splitCSV(s.GetAllowedDirs()),
		AllowShell:      s.GetAllowShell(),
		AllowCodeExec:   s.GetAllowCodeExec(),
		MaxArgs:         int(s.GetMaxArgs()),
		MaxEnv:          int(s.GetMaxEnv()),
	}
	for _, cmd := range splitCSV(s.GetAllowedCommands()) {
		p.AllowedCommands[strings.ToLower(baseName(cmd))] = true
	}
	SetStdioPolicy(p)
}

// applyRemoteConfig mcp.remote 段 → egress.Policy
func applyRemoteConfig(r *conf.Data_Mcp_Remote) {
	if r == nil {
		return
	}
	p := egress.Policy{
		AllowedHosts:      splitCSV(r.GetAllowedHosts()),
		DeniedHosts:       splitCSV(r.GetDeniedHosts()),
		AllowPrivate:      r.GetAllowPrivate(),
		AllowPrivateHosts: splitCSV(r.GetAllowPrivateHosts()),
		MaxRedirects:      int(r.GetMaxRedirects()),
	}
	for _, s := range splitCSV(r.GetAllowedPorts()) {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 65535 {
			p.AllowedPorts = append(p.AllowedPorts, n)
		}
	}
	egress.SetPolicy(p)
}

// splitCSV 逗号分隔列表 → 去空白、去空项
func splitCSV(raw string) []string {
	out := make([]string, 0, 8)
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// baseName 提取命令基名（跨平台路径分隔符）
func baseName(cmd string) string {
	if i := strings.LastIndexAny(cmd, `/\`); i >= 0 {
		return cmd[i+1:]
	}
	return cmd
}
