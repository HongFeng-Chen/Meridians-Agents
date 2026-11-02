package repo

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMapUpdateKeysMatchModelColumns 静态回归测试：repo 层所有 Updates(map[string]any{...})
// 的 key 必须能匹配某个模型的 gorm column tag（或默认列名）。
// 背景：GORM map 更新不走 struct tag 映射，key 直接当列名——
// agentfactory.go 曾把 flow_json 误写成 "flow"，运行时报 MySQL 1054
// Unknown column 'flow' in 'field list'，编译期无法发现。
func TestMapUpdateKeysMatchModelColumns(t *testing.T) {
	// 1. 收集模型的列名（gorm column tag）+ GORM 默认列名
	colRe := regexp.MustCompile(`gorm:"[^"]*column:([a-z_0-9]+)`)
	cols := map[string]bool{
		"id": true, "created_at": true, "updated_at": true, "deleted_at": true,
	}
	modelDir := filepath.Join("..", "models")
	entries, err := os.ReadDir(modelDir)
	if err != nil {
		t.Fatalf("读模型目录失败: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := os.Open(filepath.Join(modelDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			for _, m := range colRe.FindAllStringSubmatch(sc.Text(), -1) {
				cols[m[1]] = true
			}
		}
		f.Close()
	}

	// 2. 扫 repo 目录所有 Updates(map...) 的 key
	keyRe := regexp.MustCompile(`"([a-z_0-9]+)"\s*:`)
	mapRe := regexp.MustCompile(`Updates\(map\[string\]any\{`)
	repoDir := "."
	entries, err = os.ReadDir(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := os.Open(filepath.Join(repoDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		lineNo := 0
		inMap := false
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if mapRe.MatchString(line) {
				inMap = true
				continue
			}
			if inMap {
				if strings.Contains(line, "})") {
					inMap = false
					continue
				}
				for _, m := range keyRe.FindAllStringSubmatch(line, -1) {
					if !cols[m[1]] {
						t.Errorf("%s:%d map 更新 key %q 不匹配任何模型列名（GORM map 更新不走 tag 映射，请核对 gorm column tag）", e.Name(), lineNo, m[1])
					}
				}
			}
		}
		f.Close()
	}
}
