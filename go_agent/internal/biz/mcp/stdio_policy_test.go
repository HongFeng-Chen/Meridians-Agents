package mcp

import (
	"path/filepath"
	"testing"
)

// withPolicy 临时替换 stdio 策略，测试结束恢复
func withPolicy(t *testing.T, p StdioPolicy) {
	t.Helper()
	old := CurrentStdioPolicy()
	SetStdioPolicy(p)
	t.Cleanup(func() { SetStdioPolicy(old) })
}

// TestValidateStdioDefaultDeny stdio 默认关闭：未显式开启时任何 command 都应被拒绝。
func TestValidateStdioDefaultDeny(t *testing.T) {
	withPolicy(t, StdioPolicy{})
	if err := ValidateStdio("npx", nil, nil); err == nil {
		t.Fatal("默认配置下 stdio 必须被拒绝")
	}
}

// TestValidateStdioAllowlist 白名单基名放行；绝对路径必须落在允许目录内。
func TestValidateStdioAllowlist(t *testing.T) {
	base := t.TempDir() // 用临时目录构造绝对路径，兼容 Windows / Linux
	inside := filepath.Join(base, "bin", "npx")
	outside := filepath.Join(filepath.Dir(base), "npx-evil")
	withPolicy(t, StdioPolicy{
		Enabled:         true,
		AllowedCommands: map[string]bool{"npx": true},
		AllowedDirs:     []string{base},
	})
	if err := ValidateStdio("npx", []string{"-y", "@modelcontextprotocol/server-filesystem"}, nil); err != nil {
		t.Fatalf("白名单命令应放行: %v", err)
	}
	if err := ValidateStdio(inside, nil, nil); err != nil {
		t.Fatalf("允许目录内的绝对路径应放行: %v", err)
	}
	if err := ValidateStdio(outside, nil, nil); err == nil {
		t.Fatal("允许目录外的绝对路径必须拒绝")
	}
	if err := ValidateStdio(filepath.Join("bin", "npx"), nil, nil); err == nil {
		t.Fatal("相对路径必须拒绝（可被工作目录劫持）")
	}
	if err := ValidateStdio("rm", nil, nil); err == nil {
		t.Fatal("未在白名单的命令必须拒绝")
	}
}

// TestValidateStdioPathTraversal 路径穿越必须被 filepath.Clean + 目录边界挡住。
func TestValidateStdioPathTraversal(t *testing.T) {
	base := t.TempDir()
	withPolicy(t, StdioPolicy{Enabled: true, AllowedDirs: []string{base}})
	for _, cmd := range []string{
		filepath.Join(base, "..", "..", "bin", "sh"),
		base + "-evil" + string(filepath.Separator) + "tool",
		filepath.Join(base, ".", "..", "..", "bin", "bash"),
	} {
		if err := ValidateStdio(cmd, nil, nil); err == nil {
			t.Fatalf("路径穿越应被拒绝: %s", cmd)
		}
	}
}

// TestValidateStdioShellGate shell 是独立的提权通道，必须显式开启。
func TestValidateStdioShellGate(t *testing.T) {
	withPolicy(t, StdioPolicy{Enabled: true, AllowedCommands: map[string]bool{"bash": true}})
	if err := ValidateStdio("bash", []string{"-c", "id"}, nil); err == nil {
		t.Fatal("未开启 AllowShell 时 bash 必须拒绝")
	}
	withPolicy(t, StdioPolicy{Enabled: true, AllowedCommands: map[string]bool{"bash": true}, AllowShell: true})
	if err := ValidateStdio("bash", []string{"-lc", "echo ok"}, nil); err != nil {
		t.Fatalf("显式开启后应放行: %v", err)
	}
}

// TestValidateStdioInlineCode python -c / node -e 等价于直接执行代码。
func TestValidateStdioInlineCode(t *testing.T) {
	withPolicy(t, StdioPolicy{Enabled: true, AllowedCommands: map[string]bool{"python3": true, "node": true}})
	if err := ValidateStdio("python3", []string{"-c", "import os; os.system('id')"}, nil); err == nil {
		t.Fatal("内联代码参数必须拒绝")
	}
	if err := ValidateStdio("node", []string{"-e", "process.exit(0)"}, nil); err == nil {
		t.Fatal("node -e 必须拒绝")
	}
	withPolicy(t, StdioPolicy{
		Enabled:         true,
		AllowedCommands: map[string]bool{"python3": true},
		AllowCodeExec:   true,
	})
	if err := ValidateStdio("python3", []string{"-c", "print(1)"}, nil); err != nil {
		t.Fatalf("显式开启后应放行: %v", err)
	}
}

// TestValidateStdioDangerousEnv LD_PRELOAD / NODE_OPTIONS 等可劫持子进程。
func TestValidateStdioDangerousEnv(t *testing.T) {
	withPolicy(t, StdioPolicy{Enabled: true, AllowedCommands: map[string]bool{"npx": true}})
	for _, k := range []string{"LD_PRELOAD", "NODE_OPTIONS", "PYTHONPATH", "BASH_ENV"} {
		if err := ValidateStdio("npx", nil, map[string]string{k: "x"}); err == nil {
			t.Fatalf("危险环境变量 %s 必须拒绝", k)
		}
	}
	if err := ValidateStdio("npx", nil, map[string]string{"NODE_ENV": "production"}); err != nil {
		t.Fatalf("普通环境变量应放行: %v", err)
	}
}

// TestValidateStdioLimits 参数与环境变量数量/长度上限，防资源滥用。
func TestValidateStdioLimits(t *testing.T) {
	withPolicy(t, StdioPolicy{Enabled: true, AllowedCommands: map[string]bool{"npx": true}, MaxArgs: 2, MaxEnv: 1})
	if err := ValidateStdio("npx", []string{"a", "b", "c"}, nil); err == nil {
		t.Fatal("超出参数上限必须拒绝")
	}
	if err := ValidateStdio("npx", nil, map[string]string{"A": "1", "B": "2"}); err == nil {
		t.Fatal("超出环境变量上限必须拒绝")
	}
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateStdio("npx", []string{string(long)}, nil); err == nil {
		t.Fatal("超长参数必须拒绝")
	}
}
