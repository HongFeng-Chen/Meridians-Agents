package conf

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
)

// envPattern 匹配 ${NAME} 或 ${NAME:default} 形式的占位符。
// Kratos 的 file source 本身不做环境变量展开，这里在交给它之前先替换掉，
// 否则 `source: ${DATABASE_SOURCE:...}` 会被当成字面量，DSN 无法使用。
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::([^}]*))?\}`)

// ExpandEnv 展开文本中的 ${NAME:default} 占位符。
// 未设置环境变量时使用默认值；没有默认值则替换为空串。
func ExpandEnv(raw []byte) []byte {
	out := envPattern.ReplaceAllFunc(raw, func(m []byte) []byte {
		groups := envPattern.FindSubmatch(m)
		name := string(groups[1])
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return []byte(v)
		}
		if len(groups) > 2 {
			return groups[2]
		}
		return nil
	})
	return out
}

// Load 加载配置目录：读取其中的 yaml 文件，展开 ${ENV:default} 后解析为 Bootstrap。
// 返回的 cleanup 负责关闭 config 并清理临时目录。
func Load(dir string) (*Bootstrap, func(), error) {
	tmpDir, cleanupTmp, err := materialize(dir)
	if err != nil {
		return nil, nil, err
	}

	c := config.New(
		config.WithSource(
			file.NewSource(tmpDir),
			env.NewSource("KRATOS"),
		),
	)
	cleanup := func() {
		_ = c.Close()
		cleanupTmp()
	}
	if err := c.Load(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("加载配置 %s: %w", dir, err)
	}

	var bc Bootstrap
	if err := c.Scan(&bc); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("解析配置 %s: %w", dir, err)
	}
	return &bc, cleanup, nil
}

// materialize 把 dir 下的 yaml 文件展开环境变量后写入临时目录，返回临时目录路径。
func materialize(dir string) (string, func(), error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, fmt.Errorf("读取配置目录 %s: %w", dir, err)
	}
	tmpDir, err := os.MkdirTemp("", "go-agent-conf-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	found := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			cleanup()
			return "", nil, err
		}
		if err := os.WriteFile(filepath.Join(tmpDir, e.Name()), ExpandEnv(raw), 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
		found = true
	}
	if !found {
		cleanup()
		return "", nil, fmt.Errorf("配置目录 %s 下没有 yaml/json 文件", dir)
	}
	return tmpDir, cleanup, nil
}
