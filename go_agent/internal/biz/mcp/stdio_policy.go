package mcp

// stdio 传输的执行策略。
//
// 背景（P0）：stdio 传输会在后端进程里 exec 一个 command。服务定义来自管理面——
// 一旦管理面失守（匿名可达，或任意登录用户可写），这就等于远程命令执行。
// 因此这里做第二道闸：**默认全部拒绝**，必须显式配置白名单才允许启用，
// 且即便记录已经写进库（历史数据 / 越权写入），Dail 前仍要再校验一次。
//
// 配置（环境变量，进程启动时读取一次，可用 SetStdioPolicy 覆盖）：
//
//	MCP_STDIO_ENABLED=true                  总开关，默认 false
//	MCP_STDIO_ALLOWED_COMMANDS=npx,uvx,node 允许的命令基名（逗号分隔）
//	MCP_STDIO_ALLOWED_DIRS=/opt/mcp/bin     允许的绝对路径目录（逗号分隔）
//	MCP_STDIO_ALLOW_SHELL=true              是否允许 sh/bash/cmd/powershell（默认 false）
//	MCP_STDIO_ALLOW_CODE_EXEC=true          是否允许 python -c / node -e 内联代码（默认 false）
//	MCP_STDIO_MAX_ARGS=32                   参数个数上限
//
// 设计取舍：白名单按「基名」而不是完整路径，避免把发行版差异（/usr/bin vs /usr/local/bin）
// 写死；需要精确控制时用绝对路径 + AllowedDirs。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// StdioPolicy stdio 执行策略
type StdioPolicy struct {
	Enabled         bool
	AllowedCommands map[string]bool
	AllowedDirs     []string
	AllowShell      bool
	AllowCodeExec   bool
	MaxArgs         int
	MaxEnv          int
}

// 解释器：这些命令的内联代码参数等价于直接执行任意代码
var interpreterNames = map[string]bool{
	"python": true, "python3": true, "python2": true,
	"node": true, "nodejs": true, "perl": true, "ruby": true, "php": true,
	"deno": true, "bun": true,
}

// shell 名字：必须显式开启 AllowShell 才放行
var shellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"cmd": true, "cmd.exe": true, "powershell": true, "powershell.exe": true, "pwsh": true,
}

// 会改变动态链接器 / 解释器行为的危险环境变量（可被用来在子进程里执行代码）
var dangerousEnvKeys = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
	"DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true,
	"PYTHONPATH": true, "PYTHONSTARTUP": true, "PYTHONHOME": true,
	"NODE_OPTIONS": true, "BASH_ENV": true, "ENV": true,
	"PERL5OPT": true, "RUBYOPT": true,
	"GIT_SSH_COMMAND": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true,
}

var (
	stdioPolicyMu sync.RWMutex
	stdioPolicy   = loadStdioPolicy()
)

// loadStdioPolicy 从环境变量加载策略（默认拒绝）
func loadStdioPolicy() StdioPolicy {
	p := StdioPolicy{
		Enabled:         envBool("MCP_STDIO_ENABLED", false),
		AllowedCommands: map[string]bool{},
		AllowedDirs:     envList("MCP_STDIO_ALLOWED_DIRS"),
		AllowShell:      envBool("MCP_STDIO_ALLOW_SHELL", false),
		AllowCodeExec:   envBool("MCP_STDIO_ALLOW_CODE_EXEC", false),
		MaxArgs:         envInt("MCP_STDIO_MAX_ARGS", 32),
		MaxEnv:          envInt("MCP_STDIO_MAX_ENV", 32),
	}
	for _, c := range envList("MCP_STDIO_ALLOWED_COMMANDS") {
		p.AllowedCommands[strings.ToLower(filepath.Base(c))] = true
	}
	for i, d := range p.AllowedDirs {
		p.AllowedDirs[i] = filepath.Clean(d)
	}
	return p
}

// SetStdioPolicy 覆盖策略（启动装配与单测使用）
func SetStdioPolicy(p StdioPolicy) {
	if p.AllowedCommands == nil {
		p.AllowedCommands = map[string]bool{}
	}
	if p.MaxArgs <= 0 {
		p.MaxArgs = 32
	}
	if p.MaxEnv <= 0 {
		p.MaxEnv = 32
	}
	stdioPolicyMu.Lock()
	stdioPolicy = p
	stdioPolicyMu.Unlock()
}

// CurrentStdioPolicy 读取当前策略快照
func CurrentStdioPolicy() StdioPolicy {
	stdioPolicyMu.RLock()
	defer stdioPolicyMu.RUnlock()
	return stdioPolicy
}

// ValidateStdio 校验一条 stdio 服务定义是否允许执行。
// Normalize（写入路径）与 newStdioClient（执行路径）都会调用，形成双保险。
func ValidateStdio(command string, args []string, env map[string]string) error {
	p := CurrentStdioPolicy()
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return fmt.Errorf("%w: stdio 模式必须填 command", ErrInvalidServer)
	}
	if !p.Enabled {
		return fmt.Errorf("%w: stdio 传输默认关闭（远程命令执行风险）；"+
			"需显式设置 MCP_STDIO_ENABLED=true 并配置 MCP_STDIO_ALLOWED_COMMANDS 白名单", ErrInvalidServer)
	}
	if hasControlChar(cmd) {
		return fmt.Errorf("%w: command 含控制字符", ErrInvalidServer)
	}

	base := strings.ToLower(filepath.Base(cmd))
	// 1) 命令来源：绝对路径（限定目录）或白名单基名
	if strings.ContainsAny(cmd, `/\`) {
		if !filepath.IsAbs(cmd) {
			return fmt.Errorf("%w: command 使用路径时必须是绝对路径（相对路径可被工作目录劫持）", ErrInvalidServer)
		}
		if !withinDirs(filepath.Clean(cmd), p.AllowedDirs) {
			return fmt.Errorf("%w: command 路径不在允许目录内；请配置 MCP_STDIO_ALLOWED_DIRS", ErrInvalidServer)
		}
	} else if !p.AllowedCommands[base] {
		return fmt.Errorf("%w: command %q 不在白名单；请配置 MCP_STDIO_ALLOWED_COMMANDS（逗号分隔）", ErrInvalidServer, base)
	}

	// 2) shell 与内联代码：两条独立的提权通道，默认都关
	if shellNames[base] && !p.AllowShell {
		return fmt.Errorf("%w: 不允许使用 shell（%s）；确需使用请设置 MCP_STDIO_ALLOW_SHELL=true", ErrInvalidServer, base)
	}
	if len(args) > p.MaxArgs {
		return fmt.Errorf("%w: 参数过多（%d > %d）", ErrInvalidServer, len(args), p.MaxArgs)
	}
	for _, a := range args {
		if hasControlChar(a) {
			return fmt.Errorf("%w: 参数含控制字符", ErrInvalidServer)
		}
		if len(a) > 512 {
			return fmt.Errorf("%w: 单个参数过长（>512）", ErrInvalidServer)
		}
		if !p.AllowCodeExec && isInlineCodeArg(base, a) {
			return fmt.Errorf("%w: 不允许内联代码参数 %q（%s）；确需使用请设置 MCP_STDIO_ALLOW_CODE_EXEC=true",
				ErrInvalidServer, a, base)
		}
	}

	// 3) 环境变量：注入 LD_PRELOAD / NODE_OPTIONS 等同样能执行代码
	if len(env) > p.MaxEnv {
		return fmt.Errorf("%w: 环境变量过多（%d > %d）", ErrInvalidServer, len(env), p.MaxEnv)
	}
	for k, v := range env {
		key := strings.TrimSpace(k)
		if key == "" || strings.ContainsAny(key, " =\x00") {
			return fmt.Errorf("%w: 环境变量名不合法 %q", ErrInvalidServer, k)
		}
		if dangerousEnvKeys[strings.ToUpper(key)] {
			return fmt.Errorf("%w: 禁止设置环境变量 %s（可被用于在子进程执行代码）", ErrInvalidServer, key)
		}
		if hasControlChar(v) {
			return fmt.Errorf("%w: 环境变量 %s 的值含控制字符", ErrInvalidServer, key)
		}
	}
	return nil
}

// isInlineCodeArg 判断参数是否是解释器的「内联代码」开关
func isInlineCodeArg(base, arg string) bool {
	if !interpreterNames[base] {
		// shell 的 -c 已在上面按 AllowShell 管控，这里不重复判断
		return false
	}
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "-c", "-e", "--eval", "-p", "--print", "-r", "--require":
		return true
	}
	return false
}

// withinDirs 判断绝对路径是否落在允许目录内（按路径边界，防 /opt/mcp-evil 绕过 /opt/mcp）
func withinDirs(path string, dirs []string) bool {
	for _, d := range dirs {
		d = filepath.Clean(strings.TrimSpace(d))
		if d == "" || d == "." {
			continue
		}
		if path == d {
			return true
		}
		prefix := d + string(os.PathSeparator)
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}
