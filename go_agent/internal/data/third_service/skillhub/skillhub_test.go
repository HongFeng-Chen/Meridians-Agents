package skillhub

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go_agent/internal/biz/skill"
	"go_agent/internal/conf"
)

func hubOf(url string) skill.Hub {
	return skill.Hub{Name: "test", BaseURL: url, Enabled: true}
}

func newClient() *Client {
	return New(&conf.Data{Skill: &conf.Data_Skill{}})
}

func TestSearchJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/skills" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("q") != "pdf" {
			t.Fatalf("q 未透传: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"skills": []map[string]any{
			{"name": "pdf-tool", "version": "1.2.0", "title": "PDF 工具", "tags": []string{"pdf"}, "downloads": 42},
			{"name": "", "version": "x"},
		}})
	}))
	defer srv.Close()

	list, err := newClient().Search(context.Background(), hubOf(srv.URL), "pdf", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "pdf-tool" || list[0].Downloads != 42 {
		t.Fatalf("搜索结果异常: %+v", list)
	}
}

func TestFetchJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/skills/pdf-tool/versions/1.2.0/download" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tkn" {
			t.Fatalf("token 未透传: %s", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "pdf-tool", "version": "1.2.0", "title": "PDF",
			"files": []map[string]string{{"path": "scripts/run.py", "content": "print(1)"}},
		})
	}))
	defer srv.Close()

	h := hubOf(srv.URL)
	h.Token = "tkn"
	s, err := newClient().Fetch(context.Background(), h, "pdf-tool", "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "pdf-tool" || len(s.Files) != 1 || s.Files[0].Path != "scripts/run.py" {
		t.Fatalf("技能包解析异常: %+v", s)
	}
}

// zip 包：带顶层目录 + manifest.json，应去前缀并读取元信息
func TestFetchZip(t *testing.T) {
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	files := map[string]string{
		"pdf-tool/SKILL.md":       "# PDF Tool",
		"pdf-tool/manifest.json":  `{"name":"pdf-tool","version":"2.0.0","title":"PDF 工具","category":"document"}`,
		"pdf-tool/scripts/run.py": "print('hi')",
	}
	for name, content := range files {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	payload := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	s, err := newClient().Fetch(context.Background(), hubOf(srv.URL), "pdf-tool", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "pdf-tool" || s.Version != "2.0.0" || s.Title != "PDF 工具" {
		t.Fatalf("manifest 元信息未生效: %+v", s)
	}
	if !strings.Contains(s.Content, "PDF Tool") {
		t.Fatalf("SKILL.md 未解析: %q", s.Content)
	}
	if len(s.Files) != 1 || s.Files[0].Path != "scripts/run.py" {
		t.Fatalf("文件路径未去顶层目录: %+v", s.Files)
	}
}

func TestFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := newClient().Fetch(context.Background(), hubOf(srv.URL), "nope", ""); err == nil {
		t.Fatal("404 应返回错误")
	}
}

// —— SkillHub（skillhub.cn）专有形态 ——

func TestNormalizeBase(t *testing.T) {
	cases := []struct {
		in   string
		want string
		hub  bool
	}{
		{"https://www.skillhub.cn", "https://api.skillhub.cn", true},
		{"https://skillhub.cn/", "https://api.skillhub.cn", true},
		{"https://api.skillhub.cn/api/v1", "https://api.skillhub.cn", true},
		{"https://registry.example.com/", "https://registry.example.com", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, hub := normalizeBase(c.in)
		if got != c.want || hub != c.hub {
			t.Errorf("normalizeBase(%q) = (%q,%v), want (%q,%v)", c.in, got, hub, c.want, c.hub)
		}
	}
}

// 真实 SkillHub 列表响应（截自 api.skillhub.cn，字段保持原样）
const skillHubListJSON = `{
  "code": 0,
  "message": "success",
  "data": {
    "total": 2537,
    "skills": [
      {
        "slug": "ppt-demo",
        "name": "日常办公ppt",
        "version": "1.0.1",
        "description": "Generate pptx",
        "description_zh": "根据需求生成可交付的 PPT 演示文稿",
        "category": "office-efficiency",
        "downloads": 2968,
        "namespace": {"canonicalName": "@user_9e9c86d2/ppt-demo", "publicSlug": "ppt-demo"},
        "subCategories": [{"key": "office-ppt", "name": "PPT 生成"}]
      },
      {
        "slug": "",
        "name": "无 slug 条目",
        "namespace": {"publicSlug": "fallback-slug"}
      },
      {
        "slug": "",
        "name": "完全没标识"
      }
    ]
  }
}`

func TestParseSkillHubList(t *testing.T) {
	list, err := parseSkillHubList([]byte(skillHubListJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 usable entries, got %d", len(list))
	}
	first := list[0]
	if first.Name != "ppt-demo" {
		t.Errorf("Name = %q, want slug ppt-demo", first.Name)
	}
	if first.Title != "日常办公ppt" {
		t.Errorf("Title = %q", first.Title)
	}
	if !strings.Contains(first.Description, "PPT 演示文稿") {
		t.Errorf("Description 应优先中文: %q", first.Description)
	}
	if first.Version != "1.0.1" || first.Downloads != 2968 || first.Category != "office-efficiency" {
		t.Errorf("字段映射不对: %+v", first)
	}
	if len(first.Tags) != 1 || first.Tags[0] != "PPT 生成" {
		t.Errorf("Tags = %v", first.Tags)
	}
	// 没有 slug 时回退到 namespace.publicSlug
	if list[1].Name != "fallback-slug" {
		t.Errorf("fallback Name = %q", list[1].Name)
	}
}

func TestParseSkillHubListEdge(t *testing.T) {
	list, err := parseSkillHubList([]byte(`{"code":0,"data":{"skills":[],"total":0}}`))
	if err != nil {
		t.Fatalf("空结果不应报错: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("want empty, got %v", list)
	}

	if _, err := parseSkillHubList([]byte(`<!DOCTYPE html><html><body>SPA</body></html>`)); err == nil {
		t.Fatal("HTML 响应应报错")
	}
	if _, err := parseSkillHubList([]byte(`{"code":40001,"message":"bad token"}`)); err == nil {
		t.Fatal("非 0 code 应报错")
	}
	if _, err := parseSkillHubList([]byte(`{"skills":[]}`)); err == nil {
		t.Fatal("缺少 data 字段应报错")
	}
}

func TestDecodeSkillListShapes(t *testing.T) {
	shapes := map[string]string{
		"array":       `[{"name":"a"}]`,
		"skills":      `{"skills":[{"name":"a"}]}`,
		"data.skills": `{"data":{"skills":[{"name":"a"}]}}`,
		"data.array":  `{"data":[{"name":"a"}]}`,
		"items":       `{"items":[{"skill_name":"a"}]}`,
	}
	for name, raw := range shapes {
		list, ok := decodeSkillList([]byte(raw))
		if !ok || len(list) != 1 {
			t.Errorf("%s: ok=%v list=%v", name, ok, list)
		}
	}
	if _, ok := decodeSkillList([]byte(`<html></html>`)); ok {
		t.Error("HTML 不应被解析成功")
	}
	if _, ok := decodeSkillList(nil); ok {
		t.Error("空响应不应被解析成功")
	}
}

func TestGenericSearchToleratesDataWrapper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/skills" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "pptx" {
			t.Errorf("keyword = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"skills":[{"name":"pptx","title":"做PPT","version":"1.2.0","downloads":7}]}}`))
	}))
	defer srv.Close()

	list, err := newClient().Search(context.Background(), hubOf(srv.URL), "pptx", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(list) != 1 || list[0].Name != "pptx" || list[0].Title != "做PPT" || list[0].Downloads != 7 {
		t.Fatalf("unexpected list: %+v", list)
	}
}

func TestGenericSearchHTMLGivesActionableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><title>SkillHub</title></html>`))
	}))
	defer srv.Close()

	_, err := newClient().Search(context.Background(), hubOf(srv.URL), "", 5)
	if err == nil {
		t.Fatal("期望报错")
	}
	msg := err.Error()
	for _, want := range []string{"搜索结果无法解析", "HTML", "Content-Type=text/html"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺少 %q: %s", want, msg)
		}
	}
}

// SkillHub zip 包：SKILL.md 在根目录，带 _meta.json 与子目录文件
func TestFetchZipSkipsMetaFile(t *testing.T) {
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	files := map[string]string{
		"SKILL.md":        "# demo",
		"_meta.json":      `{"slug":"demo","version":"1.0.0"}`,
		"references/a.md": "参考",
		"scripts/run.py":  "print(1)",
	}
	for name, content := range files {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	payload := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	s, err := newClient().Fetch(context.Background(), hubOf(srv.URL), "demo", "1.0.0")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(s.Content, "# demo") {
		t.Errorf("SKILL.md 未写入 content: %q", s.Content)
	}
	if s.Name != "demo" || s.Version != "1.0.0" {
		t.Errorf("Name/Version = %q/%q", s.Name, s.Version)
	}
	for _, f := range s.Files {
		if strings.EqualFold(f.Path, "_meta.json") {
			t.Errorf("_meta.json 不应作为技能文件: %+v", s.Files)
		}
	}
	if len(s.Files) != 2 {
		t.Errorf("want 2 files, got %+v", s.Files)
	}
}

func TestDescribeBody(t *testing.T) {
	if got := describeBody("text/html", []byte("<!DOCTYPE html><html></html>")); !strings.Contains(got, "HTML") {
		t.Errorf("HTML 未被识别: %s", got)
	}
	if got := describeBody("application/json", []byte(`{"a":1}`)); !strings.Contains(got, "字段不匹配") {
		t.Errorf("JSON 未被识别: %s", got)
	}
	if got := describeBody("", nil); !strings.Contains(got, "空") {
		t.Errorf("空响应未被识别: %s", got)
	}
	if got := describeBody("application/zip", []byte("PK\x03\x04xx")); !strings.Contains(got, "zip") {
		t.Errorf("zip 未被识别: %s", got)
	}
}

// 不同命名空间存在同名 slug（真实数据里出现过），安装按 slug 定位，展示侧应去重
func TestParseSkillHubListDedup(t *testing.T) {
	raw := `{"code":0,"data":{"total":2,"skills":[
		{"slug":"ppt-master","name":"PPT可编辑大师","namespace":{"canonicalName":"@a/ppt-master"}},
		{"slug":"ppt-master","name":"另一个同名技能","namespace":{"canonicalName":"@b/ppt-master"}}
	]}}`
	list, err := parseSkillHubList([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("同名 slug 未去重: %+v", list)
	}
	if list[0].Title != "PPT可编辑大师" {
		t.Errorf("应保留首个条目: %+v", list[0])
	}
}

// —— 技能包解压限额：zip 炸弹 / 路径穿越 ——

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for name, content := range files {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseZipRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../evil.md", "a/../../evil.md", "/etc/passwd", `C:/Windows/evil.md`} {
		raw := zipOf(t, map[string]string{"SKILL.md": "# demo", name: "x"})
		if _, err := parseZip(raw); err == nil {
			t.Fatalf("路径 %q 应被拒绝", name)
		} else if !strings.Contains(err.Error(), "非法路径") && !strings.Contains(err.Error(), "可疑条目") {
			t.Fatalf("路径 %q 的报错信息应说明原因，实际 %v", name, err)
		}
	}
}

func TestParseZipRejectsTooManyEntries(t *testing.T) {
	files := map[string]string{"SKILL.md": "# demo"}
	for i := 0; i < maxZipEntries+5; i++ {
		files[fmt.Sprintf("refs/f%03d.md", i)] = "x"
	}
	raw := zipOf(t, files)
	if _, err := parseZip(raw); err == nil {
		t.Fatal("条目数超限应被拒绝")
	} else if !strings.Contains(err.Error(), "文件数超过上限") {
		t.Fatalf("应提示文件数超限，实际 %v", err)
	}
}

func TestParseZipRejectsOversizeEntry(t *testing.T) {
	big := strings.Repeat("A", maxZipEntrySize+1)
	raw := zipOf(t, map[string]string{"SKILL.md": "# demo", "big.md": big})
	if _, err := parseZip(raw); err == nil {
		t.Fatal("单文件超限应被拒绝，而不是截断后收下")
	} else if !strings.Contains(err.Error(), "超过") && !strings.Contains(err.Error(), "可疑条目") {
		t.Fatalf("应提示超限，实际 %v", err)
	}
}

// 压缩比异常（高度可压缩的大文件）按炸弹处理
func TestSafeZipEntryRatio(t *testing.T) {
	raw := zipOf(t, map[string]string{"bomb.md": strings.Repeat("0", 4<<20)})
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range reader.File {
		if safeZipEntry(f) {
			t.Fatalf("压缩比 %d:1 的条目应被判定为可疑", f.UncompressedSize64/f.CompressedSize64)
		}
	}
	// 正常文本仍放行
	ok := zipOf(t, map[string]string{"SKILL.md": "# demo"})
	rd, _ := zip.NewReader(bytes.NewReader(ok), int64(len(ok)))
	for _, f := range rd.File {
		if !safeZipEntry(f) {
			t.Fatalf("正常条目不应被拒：%s", f.Name)
		}
	}
}

// 市场 JSON 形态的文件路径同样要校验（不只是 zip）
func TestParseJSONRejectsBadPath(t *testing.T) {
	raw := []byte(`{"name":"demo","files":[{"path":"../evil.md","content":"x"}]}`)
	if _, err := parseJSON(raw); err == nil {
		t.Fatal("JSON 形态的非法路径应被拒绝")
	}
	ok := []byte(`{"name":"demo","files":[{"path":"references/a.md","content":"x"}]}`)
	s, err := parseJSON(ok)
	if err != nil {
		t.Fatalf("合法路径不应报错: %v", err)
	}
	if len(s.Files) != 1 || s.Files[0].Path != "references/a.md" {
		t.Fatalf("文件路径应保留，实际 %+v", s.Files)
	}
}
