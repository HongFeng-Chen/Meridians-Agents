package history

import (
	"context"
	"testing"
)

// WithConv 后再 WithTask：任务身份写入，会话身份必须保留（读改写，不整体覆盖）
func TestWithTaskKeepsConv(t *testing.T) {
	ctx := WithConv(context.Background(), "conv-42")
	ctx = WithTask(ctx, 7, "calc-agent")

	conv := ConvFrom(ctx)
	if conv != "conv-42" {
		t.Fatalf("WithTask 丢弃了会话身份: conv_id=%q", conv)
	}
	taskID, agentKey := MetaFrom(ctx)
	if taskID != 7 || agentKey != "calc-agent" {
		t.Fatalf("任务身份错误: task=%d agent=%q", taskID, agentKey)
	}
}

// WithTask 后再 WithConv：会话身份写入，任务身份必须保留（WithConv 本就是读改写）
func TestWithConvKeepsTask(t *testing.T) {
	ctx := WithTask(context.Background(), 9, "worker-agent")
	ctx = WithConv(ctx, "conv-7")

	conv := ConvFrom(ctx)
	if conv != "conv-7" {
		t.Fatalf("会话身份错误: conv_id=%q", conv)
	}
	taskID, agentKey := MetaFrom(ctx)
	if taskID != 9 || agentKey != "worker-agent" {
		t.Fatalf("WithConv 丢弃了任务身份: task=%d agent=%q", taskID, agentKey)
	}
}

// 空上下文：无任何身份，取值为零值（= 控制台手动调用 / 会话外）
func TestMetaFromEmpty(t *testing.T) {
	ctx := context.Background()
	if conv := ConvFrom(ctx); conv != "" {
		t.Fatalf("空 ctx 不应有会话身份: %q", conv)
	}
	if taskID, agentKey := MetaFrom(ctx); taskID != 0 || agentKey != "" {
		t.Fatalf("空 ctx 不应有任务身份: task=%d agent=%q", taskID, agentKey)
	}
}

// 同一 ctx 反复注入：后写的会话覆盖前值，任务同理
func TestStackedOverrides(t *testing.T) {
	ctx := WithConv(context.Background(), "conv-a")
	ctx = WithConv(ctx, "conv-b")
	if conv := ConvFrom(ctx); conv != "conv-b" {
		t.Fatalf("会话身份应取最后写入: %q", conv)
	}
	ctx = WithTask(ctx, 1, "a-agent")
	ctx = WithTask(ctx, 2, "b-agent")
	if conv := ConvFrom(ctx); conv != "conv-b" {
		t.Fatalf("两次 WithTask 后会话身份仍应保留: %q", conv)
	}
	if taskID, agentKey := MetaFrom(ctx); taskID != 2 || agentKey != "b-agent" {
		t.Fatalf("任务身份应取最后写入: task=%d agent=%q", taskID, agentKey)
	}
}
