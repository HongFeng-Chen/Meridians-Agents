package redis

import (
	"strings"
	"testing"

	"go_agent/internal/biz/rag"
	"go_agent/internal/data/redis/models"
)

// 缓存键由 biz 生成（领域语义），data 层按前缀批量失效。
// 两侧前缀一旦漂移，Invalidate 会「静默无效」——文档删除后问答缓存
// 依然命中旧答案，很难排查，所以这里把契约固定住。

func TestAnswerKeyPrefixMatchesModels(t *testing.T) {
	cases := []struct{ question, filter string }{
		{"", ""},
		{"Kratos 的路由是怎么生成的", "doc=1"},
		{"graph|谁维护 Milvus", ""},
	}
	for _, c := range cases {
		key := rag.AnswerCacheKey(c.question, c.filter)
		if !strings.HasPrefix(key, models.AnswerPrefix) {
			t.Errorf("AnswerCacheKey(%q,%q) = %q，缺少前缀 %q",
				c.question, c.filter, key, models.AnswerPrefix)
		}
		// 前缀之后必须是可被 SCAN 匹配的键，不能含通配符
		if strings.ContainsAny(strings.TrimPrefix(key, models.AnswerPrefix), "*?[]") {
			t.Errorf("键 %q 含通配符，SCAN 模式会误匹配", key)
		}
	}
}

func TestIngestLockPrefixMatchesModels(t *testing.T) {
	if key := rag.IngestLockKey("doc-1"); !strings.HasPrefix(key, models.IngestLockPrefix) {
		t.Errorf("IngestLockKey = %q，缺少前缀 %q", key, models.IngestLockPrefix)
	}
}
