package aiplatform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	biz "go_agent/internal/biz/aiplatform"
)

func newTestClient() *Client { return &Client{http: &http.Client{}} }

func qwen(base string) biz.Platform {
	return biz.Platform{
		Name:           "qwen",
		Kind:           biz.KindQwen,
		BaseURL:        base,
		APIKey:         "sk-test",
		AuthScheme:     biz.AuthBearer,
		CatalogPath:    "/api/v1/mcps",
		StreamablePath: "/api/v1/mcps/{code}/mcp",
	}
}

func TestSearchDecodesDataMcpsShape(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query().Get("keyword")
		if r.URL.Path != "/api/v1/mcps" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"total":2,"mcps":[
			{"mcpCode":"WebSearch","displayName":"联网搜索","description":"实时搜索","toolsCount":"3","type":"streamableHttp","tags":["search"]},
			{"server_code":"amap-maps","title":"高德地图","summary":"地图能力","tool_count":7,"type":"sse"}
		]}}`))
	}))
	defer srv.Close()

	items, err := newTestClient().Search(context.Background(), qwen(srv.URL), "搜索", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotQuery != "搜索" {
		t.Fatalf("keyword = %q", gotQuery)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Code != "WebSearch" || items[0].Name != "联网搜索" || items[0].ToolCount != 3 || items[0].Transport != biz.TransportHTTP {
		t.Fatalf("item0 = %+v", items[0])
	}
	if items[1].Code != "amap-maps" || items[1].Transport != biz.TransportSSE || items[1].ToolCount != 7 {
		t.Fatalf("item1 = %+v", items[1])
	}
	if len(items[0].Tags) != 1 || items[0].Tags[0] != "search" {
		t.Fatalf("tags = %v", items[0].Tags)
	}
}

func TestSearchDecodesBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"slug":"weather","name":"天气","tool_count":2}]`))
	}))
	defer srv.Close()

	items, err := newTestClient().Search(context.Background(), qwen(srv.URL), "", 5)
	if err != nil || len(items) != 1 || items[0].Code != "weather" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}

func TestSearchCatalogMissing(t *testing.T) {
	p := qwen("https://example.com")
	p.CatalogPath = ""
	if _, err := newTestClient().Search(context.Background(), p, "", 5); !errors.Is(err, biz.ErrCatalogUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestSearch404MapsToUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := newTestClient().Search(context.Background(), qwen(srv.URL), "", 5)
	if !errors.Is(err, biz.ErrCatalogUnsupported) {
		t.Fatalf("err = %v，期望 ErrCatalogUnsupported", err)
	}
}

func TestSearchHTMLMapsToUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>console</body></html>`))
	}))
	defer srv.Close()

	_, err := newTestClient().Search(context.Background(), qwen(srv.URL), "", 5)
	if !errors.Is(err, biz.ErrCatalogUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "HTML") {
		t.Fatalf("错误信息应提示是 HTML 页面: %v", err)
	}
}

func TestSearchAuthFailureMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer srv.Close()

	_, err := newTestClient().Search(context.Background(), qwen(srv.URL), "", 5)
	if err == nil || !strings.Contains(err.Error(), "鉴权失败") {
		t.Fatalf("err = %v", err)
	}
	// 401/403 必须带 sentinel，否则 service 层只能映射成 500
	if !errors.Is(err, biz.ErrAuthFailed) {
		t.Fatalf("err 未包裹 ErrAuthFailed: %v", err)
	}
}

func TestCatalogURLTemplate(t *testing.T) {
	p := qwen("https://dashscope.aliyuncs.com/")
	p.CatalogPath = "/api/v1/mcps?q={q}&limit={limit}"
	got, err := catalogURL(p, p.CatalogPath, "地图", 5)
	if err != nil {
		t.Fatalf("catalogURL: %v", err)
	}
	want := "https://dashscope.aliyuncs.com/api/v1/mcps?q=%E5%9C%B0%E5%9B%BE&limit=5"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestFindListHandlesWrappedShapes(t *testing.T) {
	cases := []string{
		`{"result":{"items":[{"code":"a"}]}}`,
		`{"data":{"list":[{"code":"a"}]}}`,
		`{"servers":[{"code":"a"}]}`,
	}
	for _, raw := range cases {
		items, ok := decodeList([]byte(raw))
		if !ok || len(items) != 1 || items[0]["code"] != "a" {
			t.Fatalf("shape %s 解码失败: %+v ok=%v", raw, items, ok)
		}
	}
}
