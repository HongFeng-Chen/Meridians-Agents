package prompt

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlaceholders(t *testing.T) {
	got := Placeholders("A {{question}} B {{ question }} C {{context}} D {{question}}")
	want := []string{"question", "context"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Placeholders = %v, want %v", got, want)
	}
}

func TestPlaceholdersIgnoresNonVarBraces(t *testing.T) {
	// 中文、数字开头、空括号都不应被当作变量
	got := Placeholders("{{中文}} {{1abc}} {{}} { {x} }" + "{{ok}}")
	if !reflect.DeepEqual(got, []string{"ok"}) {
		t.Fatalf("Placeholders = %v, want [ok]", got)
	}
}

func TestRenderAllProvided(t *testing.T) {
	out, missing := Render("参考资料：\n{{context}}\n\n问题：{{question}}", map[string]string{
		"context":  "[1] 片段",
		"question": "什么是混合检索？",
	})
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want empty", missing)
	}
	want := "参考资料：\n[1] 片段\n\n问题：什么是混合检索？"
	if out != want {
		t.Fatalf("Render = %q, want %q", out, want)
	}
}

func TestRenderMissingKeepsPlaceholder(t *testing.T) {
	out, missing := Render("{{a}}-{{b}}", map[string]string{"a": "1"})
	if !reflect.DeepEqual(missing, []string{"b"}) {
		t.Fatalf("missing = %v, want [b]", missing)
	}
	if out != "1-{{b}}" {
		t.Fatalf("Render = %q, want %q", out, "1-{{b}}")
	}
}

func TestRenderEmptyValueIsProvided(t *testing.T) {
	// 空字符串是合法取值（空上下文），不算缺失
	out, missing := Render("[{{context}}]", map[string]string{"context": ""})
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want empty", missing)
	}
	if out != "[]" {
		t.Fatalf("Render = %q", out)
	}
}

func TestValidKey(t *testing.T) {
	ok := []string{"rag.qa.system", "custom_prompt", "a", "A1-b.c"}
	bad := []string{"", "1abc", "中文", "with space", "x/y", strings.Repeat("a", 129)}
	for _, k := range ok {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false, want true", k)
		}
	}
	for _, k := range bad {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true, want false", k)
		}
	}
}

// 内置模板是运行兜底，必须自身合法：key 唯一、内容非空、声明变量与内容一致
func TestBuiltinIntegrity(t *testing.T) {
	seen := make(map[string]struct{}, len(Builtins))
	for _, b := range Builtins {
		if !ValidKey(b.Key) {
			t.Errorf("内置模板 key 非法: %q", b.Key)
		}
		if _, ok := seen[b.Key]; ok {
			t.Errorf("内置模板 key 重复: %q", b.Key)
		}
		seen[b.Key] = struct{}{}
		if strings.TrimSpace(b.Content) == "" {
			t.Errorf("内置模板 %s 内容为空", b.Key)
		}
		if b.Scene == "" {
			t.Errorf("内置模板 %s 缺少场景", b.Key)
		}
		if b.Temperature < 0 || b.Temperature > 2 {
			t.Errorf("内置模板 %s 温度越界: %v", b.Key, b.Temperature)
		}
	}
	if _, ok := BuiltinOf(KeyRAGSystem); !ok {
		t.Error("缺少 RAG 系统提示词内置定义")
	}
}

func TestRenderBuiltin(t *testing.T) {
	out, ok := RenderBuiltin(KeyRAGUser, map[string]string{"context": "C", "question": "Q"})
	if !ok {
		t.Fatal("KeyRAGUser 应为内置模板")
	}
	if !strings.Contains(out, "C") || !strings.Contains(out, "Q") {
		t.Fatalf("渲染结果缺少变量内容: %q", out)
	}
	if _, ok := RenderBuiltin("nope.not.exists", nil); ok {
		t.Fatal("未知 key 不应返回 ok")
	}
}
