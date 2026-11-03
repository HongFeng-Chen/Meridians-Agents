package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeOK(t *testing.T) {
	Reset()
	Register("mysql", func(ctx context.Context) error { return nil })
	Register("redis", func(ctx context.Context) error { return nil })
	ok, rep := Probe(context.Background(), time.Second)
	if !ok || rep.Status != "ok" || len(rep.Checks) != 2 {
		t.Fatalf("unexpected: ok=%v rep=%+v", ok, rep)
	}
}

func TestProbeDegraded(t *testing.T) {
	Reset()
	Register("mysql", func(ctx context.Context) error { return nil })
	Register("milvus", func(ctx context.Context) error { return errors.New("connect refused") })
	ok, rep := Probe(context.Background(), time.Second)
	if ok || rep.Status != "degraded" {
		t.Fatalf("应降级: ok=%v rep=%+v", ok, rep)
	}
	if rep.Checks["milvus"].Error == "" {
		t.Fatal("失败项应带错误原因")
	}
}

func TestHandlerStatusCode(t *testing.T) {
	Reset()
	Register("mysql", func(ctx context.Context) error { return nil })
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("健康时应 200，得到 %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("unexpected body: %v", body)
	}

	Reset()
	Register("mysql", func(ctx context.Context) error { return errors.New("down") })
	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("不健康时应 503，得到 %d", rec.Code)
	}
}

func TestProbeTimeout(t *testing.T) {
	Reset()
	Register("slow", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	})
	begin := time.Now()
	ok, _ := Probe(context.Background(), 50*time.Millisecond)
	if ok {
		t.Fatal("超时应判为不健康")
	}
	if time.Since(begin) > time.Second {
		t.Fatal("探活必须受超时约束，不能挂住")
	}
}
