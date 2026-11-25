package mcp

// ApplyConfig 单测：config.yaml 的 mcp.stdio / mcp.remote 段正确映射到进程内策略。
// 核心场景：终端没有环境变量时，配置缺省值（如 allowPrivateHosts=127.0.0.1）也生效。

import (
	"strings"
	"testing"

	"go_agent/internal/conf"
	"go_agent/internal/pkg/egress"
)

func TestApplyConfigRemote(t *testing.T) {
	// 快照当前策略，测试后恢复，避免污染其他用例
	orig := egress.CurrentPolicy()
	defer egress.SetPolicy(orig)

	ApplyConfig(&conf.Data{
		Mcp: &conf.Data_Mcp{
			Remote: &conf.Data_Mcp_Remote{
				AllowedHosts:      "mcp.example.com, .corp.internal",
				DeniedHosts:       "evil.example.com",
				AllowPrivate:      false,
				AllowPrivateHosts: "127.0.0.1, .svc.cluster.local",
				AllowedPorts:      "443, 8443, abc", // 非法项忽略
				MaxRedirects:      3,
			},
		},
	})

	p := egress.CurrentPolicy()
	if len(p.AllowedHosts) != 2 || p.AllowedHosts[0] != "mcp.example.com" {
		t.Fatalf("AllowedHosts 映射错误: %v", p.AllowedHosts)
	}
	if len(p.DeniedHosts) != 1 || p.DeniedHosts[0] != "evil.example.com" {
		t.Fatalf("DeniedHosts 映射错误: %v", p.DeniedHosts)
	}
	if p.AllowPrivate {
		t.Fatalf("AllowPrivate 应为 false")
	}
	// 核心断言：配置缺省值路径——回环主机进放行名单后 ValidateURL 通过
	if len(p.AllowPrivateHosts) != 2 || p.AllowPrivateHosts[0] != "127.0.0.1" {
		t.Fatalf("AllowPrivateHosts 映射错误: %v", p.AllowPrivateHosts)
	}
	// 用白名单内端口 443 验证主机闸门（8080 会被端口白名单拦，那是另一个闸门的正确行为）
	if err := egress.ValidateURL("http://127.0.0.1:443/mcp"); err != nil {
		t.Fatalf("放行名单内回环应放行: %v", err)
	}
	if err := egress.ValidateURL("http://10.0.0.5:443/mcp"); err == nil {
		t.Fatalf("名单外的内网仍应拒绝")
	}
	if len(p.AllowedPorts) != 2 || p.AllowedPorts[0] != 443 {
		t.Fatalf("AllowedPorts 应只收合法项: %v", p.AllowedPorts)
	}
	if p.MaxRedirects != 3 {
		t.Fatalf("MaxRedirects 应为 3，实际 %d", p.MaxRedirects)
	}

	// 链路本地段即使在放行名单内也必须拒绝
	ApplyConfig(&conf.Data{Mcp: &conf.Data_Mcp{Remote: &conf.Data_Mcp_Remote{
		AllowPrivateHosts: "169.254.169.254,127.0.0.1",
	}}})
	if err := egress.ValidateURL("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Fatalf("链路本地段应始终拒绝")
	}
}

func TestApplyConfigStdio(t *testing.T) {
	orig := CurrentStdioPolicy()
	defer SetStdioPolicy(orig)

	ApplyConfig(&conf.Data{
		Mcp: &conf.Data_Mcp{
			Studio: &conf.Data_Mcp_Studio{
				Enabled:         true,
				AllowedCommands: "npx, /opt/mcp/bin/server.exe",
				AllowedDirs:     "/opt/mcp/bin, C:\\mcp",
				AllowShell:      false,
				MaxArgs:         16,
			},
		},
	})

	p := CurrentStdioPolicy()
	if !p.Enabled {
		t.Fatalf("Enabled 应为 true")
	}
	// 命令按基名收进白名单；绝对路径条目也按基名记录
	if !p.AllowedCommands["npx"] || !p.AllowedCommands["server.exe"] {
		t.Fatalf("AllowedCommands 应含 npx/server.exe: %v", p.AllowedCommands)
	}
	if len(p.AllowedDirs) != 2 {
		t.Fatalf("AllowedDirs 应有 2 项: %v", p.AllowedDirs)
	}
	if p.MaxArgs != 16 {
		t.Fatalf("MaxArgs 应为 16，实际 %d", p.MaxArgs)
	}
	if p.AllowShell {
		t.Fatalf("AllowShell 应为 false")
	}
}

func TestApplyConfigNilSectionsKeepsPolicy(t *testing.T) {
	orig := egress.CurrentPolicy()
	defer egress.SetPolicy(orig)

	// 配置里没写 mcp 段：策略保持现状（环境变量结果），不被清空
	egress.SetPolicy(egress.Policy{AllowPrivate: true, AllowPrivateHosts: []string{"10.0.0.1"}})
	ApplyConfig(&conf.Data{})

	p := egress.CurrentPolicy()
	if !p.AllowPrivate || len(p.AllowPrivateHosts) != 1 {
		t.Fatalf("nil 配置段不应改动现有策略: %+v", p)
	}
	if strings.Join(p.AllowPrivateHosts, ",") != "10.0.0.1" {
		t.Fatalf("AllowPrivateHosts 应保持原值: %v", p.AllowPrivateHosts)
	}
}
