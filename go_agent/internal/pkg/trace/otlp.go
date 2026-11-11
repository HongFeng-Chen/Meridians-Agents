// otlp.go OTLP/gRPC 导出器构造。
//
// 单独成文件的原因：grpc 与 exporter 依赖只有配了端点时才需要，把它隔离出来
// 能一眼看出「追踪导出」的全部外部约束都在这 40 行里。
package trace

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials/insecure"
)

// newExporter 构造 OTLP/gRPC span 导出器（不阻塞：连接失败由 batcher 后台重试）。
func newExporter(cfg Config) (sdktrace.SpanExporter, error) {
	endpoint := normalizeEndpoint(cfg.Endpoint)
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()))
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return otlptracegrpc.New(ctx, opts...)
}

// normalizeEndpoint 容忍 http(s):// 前缀与尾部斜杠：
// 环境变量里写 URL 是人的本能，但 WithEndpoint 只认 host:port。
func normalizeEndpoint(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")
	return s
}
