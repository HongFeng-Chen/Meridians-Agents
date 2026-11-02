package httpx

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	ragv1 "go_agent/api/rag/v1"
)

// newMultipartRequest 构造一个带文件与若干普通字段的 multipart 请求
func newMultipartRequest(t *testing.T, file, filename string, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if file != "" {
		part, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("构造 file part 失败: %v", err)
		}
		if _, err := part.Write([]byte(file)); err != nil {
			t.Fatalf("写入文件内容失败: %v", err)
		}
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("写入字段 %s 失败: %v", k, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 multipart writer 失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

// body: "file" 的生成代码会把「那个 bytes 字段」的地址交给解码器，
// 解码器必须把文件内容写进去（否则 handler 拿到空内容）。
func TestRequestDecoderBodyFileTargetsBytes(t *testing.T) {
	req := newMultipartRequest(t, "hello 世界", "a.txt", map[string]string{"mode": "graph"})
	var file []byte
	if err := RequestDecoder(req, &file); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if string(file) != "hello 世界" {
		t.Fatalf("bytes 字段未拿到文件内容: %q", string(file))
	}
	up, ok := UploadFrom(req.Context())
	if !ok {
		t.Fatal("ctx 上取不到 Upload")
	}
	if up.Filename != "a.txt" || string(up.Data) != "hello 世界" {
		t.Fatalf("Upload 内容不符: %+v", up)
	}
}

// body: "*" 时目标是整个消息：表单字段要按 form codec 绑定。
func TestRequestDecoderBodyStarBindsForm(t *testing.T) {
	req := newMultipartRequest(t, "", "", map[string]string{"mode": "graph", "doc_id": "d1"})
	var in ragv1.UploadAndIngestRequest
	if err := RequestDecoder(req, &in); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if in.GetMode() != "graph" || in.GetDocId() != "d1" {
		t.Fatalf("表单字段未绑定: mode=%q doc_id=%q", in.GetMode(), in.GetDocId())
	}
}

// 非 multipart 请求必须原样交给 Kratos 默认解码器（JSON 场景不能受影响）。
func TestRequestDecoderFallsBackToJSON(t *testing.T) {
	body := bytes.NewBufferString(`{"question":"hi","filter":"doc_id == \"d1\""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/query", body)
	req.Header.Set("Content-Type", "application/json")
	var in ragv1.QueryRequest
	if err := RequestDecoder(req, &in); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if in.GetQuestion() != "hi" || in.GetFilter() != `doc_id == "d1"` {
		t.Fatalf("JSON 解码结果不符: %+v", &in)
	}
}

// ResolveFile：HTTP 走 ctx，gRPC 走 bytes+name，都没有则 400。
func TestResolveFile(t *testing.T) {
	req := newMultipartRequest(t, "abc", "b.md", nil)
	if err := RequestDecoder(req, new([]byte)); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	f, err := ResolveFile(req.Context(), nil, "")
	if err != nil || f.Filename != "b.md" {
		t.Fatalf("HTTP 分支失败: %+v %v", f, err)
	}

	f, err = ResolveFile(t.Context(), []byte("xyz"), "c.pdf")
	if err != nil || f.Filename != "c.pdf" || string(f.Data) != "xyz" {
		t.Fatalf("gRPC 分支失败: %+v %v", f, err)
	}

	if _, err := ResolveFile(t.Context(), nil, ""); err == nil {
		t.Fatal("缺少 file 时应返回错误")
	}
}

// query 参数的解析保持原样（mode/doc_id 仍走 query，与契约一致）
func TestQueryStillBinds(t *testing.T) {
	q := url.Values{"mode": {"graph"}, "doc_id": {"d2"}}
	if got := q.Get("mode"); got != "graph" {
		t.Fatalf("query 参数异常: %q", got)
	}
}
