package harness

// ref_test.go 评测运行的成本归属键：必须能穿透 agentfactory 的调用元数据重建。

import (
	"context"
	"testing"

	"go_agent/internal/biz/llm"
)

func TestWithRunRefCarriesIntoLLMMeta(t *testing.T) {
	ctx := WithRunRef(context.Background(), 12)
	if got := RunRefFrom(ctx); got != 12 {
		t.Fatalf("私有 key 应可读回: %d", got)
	}
	meta := llm.MetaFrom(ctx)
	if meta.RefKind != RefKindRun || meta.RefID != "12" {
		t.Fatalf("归属键应写入 llm 元数据: %+v", meta)
	}

	// agent: 目标进入 runGraph 后会用 llm.WithMeta 重新设置任务维度：
	// 合并语义下 ref 必须保留，否则被测 Agent 的成本全部脱离本次评测。
	inner := llm.WithMeta(ctx, llm.CallMeta{
		TaskID:   900,
		AgentKey: "writer",
		Purpose:  "agentfactory.react",
	})
	if m := llm.MetaFrom(inner); m.RefKind != RefKindRun || m.RefID != "12" || m.TaskID != 900 {
		t.Fatalf("任务维度的设置不应冲掉归属键: %+v", m)
	}
}

// runID 为 0（不在评测里）时不能污染普通调用的元数据
func TestWithRunRefZeroIsNoop(t *testing.T) {
	ctx := WithRunRef(context.Background(), 0)
	if id := RunRefFrom(ctx); id != 0 {
		t.Fatalf("runID=0 应视为无归属: %d", id)
	}
	if m := llm.MetaFrom(ctx); m.RefKind != "" || m.RefID != "" {
		t.Fatalf("普通调用不应被写入归属键: %+v", m)
	}
}
