package conf

import (
	"os"
	"path/filepath"
	"testing"
)

// ${NAME:default} 应按环境变量优先、默认值兜底展开
func TestExpandEnv(t *testing.T) {
	t.Setenv("RAG_TEST_ADDR", "10.0.0.1:3306")
	t.Setenv("RAG_TEST_EMPTY", "")

	in := []byte(`
dsn: ${RAG_TEST_ADDR:127.0.0.1:3306}
empty: ${RAG_TEST_EMPTY:fallback}
missing: ${RAG_TEST_NOT_SET:默认值}
plain: no-placeholder
`)
	got := string(ExpandEnv(in))

	wantSubstrings := []string{
		"dsn: 10.0.0.1:3306",
		"empty: fallback", // 变量为空时使用默认值
		"missing: 默认值",
		"plain: no-placeholder",
	}
	for _, want := range wantSubstrings {
		if !contains(got, want) {
			t.Errorf("展开结果缺少 %q，实际：\n%s", want, got)
		}
	}
}

// 真实配置目录应能加载并解析出关键字段
func TestLoadConfigDir(t *testing.T) {
	dir := filepath.Join("..", "..", "configs")
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Skip("跳过：找不到 configs/config.yaml")
	}
	t.Setenv("MILVUS_COLLECTION", "test_collection")
	t.Setenv("REDIS_ADDR", "127.0.0.1:6399")

	bc, cleanup, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer cleanup()

	if got := bc.GetData().GetMilvus().GetCollection(); got != "test_collection" {
		t.Errorf("collection = %q, want test_collection（环境变量未生效）", got)
	}
	if got := bc.GetData().GetRedis().GetAddr(); got != "127.0.0.1:6399" {
		t.Errorf("redis addr = %q, want 127.0.0.1:6399", got)
	}
	if bc.GetData().GetEmbedding().GetDimension() != 1024 {
		t.Errorf("embedding dim = %d, want 1024", bc.GetData().GetEmbedding().GetDimension())
	}
	if bc.GetData().GetRag().GetMaxChunk() != 600 {
		t.Errorf("maxChunk = %d, want 600", bc.GetData().GetRag().GetMaxChunk())
	}
	if bc.GetServer().GetHttp().GetAddr() == "" {
		t.Error("http addr 为空")
	}
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
