package llm

// cost_test.go 成本折算与记账（P4）：单价 → 微元 → usage_records。

import (
	"context"
	"testing"
	"time"
)

func TestCostMicros(t *testing.T) {
	// 1000 prompt × 2 元/M + 500 completion × 8 元/M = 2000 + 4000 = 6000 微元（0.006 元）
	if got := CostMicros(1000, 500, 2, 8); got != 6000 {
		t.Fatalf("CostMicros = %d, want 6000", got)
	}
	// 未配单价（0）：成本为 0，但 token 仍然要记（账单为 0 元不代表没消耗）
	if got := CostMicros(1000, 500, 0, 0); got != 0 {
		t.Fatalf("零单价应为 0，得到 %d", got)
	}
	// 负数防护
	if got := CostMicros(-1, -1, 2, 8); got != 0 {
		t.Fatalf("负数应归零，得到 %d", got)
	}
}

func TestPriceVersion(t *testing.T) {
	a := PriceVersion(2, 8)
	b := PriceVersion(2, 8)
	if a != b {
		t.Fatal("同单价应产生同版本")
	}
	if a == PriceVersion(3, 8) {
		t.Fatal("单价变了版本必须变，否则历史账单会被新价格重算")
	}
}

type fakeSink struct{ ch chan CostEvent }

func (f *fakeSink) RecordCost(ctx context.Context, ev CostEvent) error {
	f.ch <- ev
	return nil
}

// 记账要带上任务身份：这是「单任务成本」唯一的数据来源。
func TestChargeCarriesTaskIdentity(t *testing.T) {
	sink := &fakeSink{ch: make(chan CostEvent, 1)}
	m := &Manager{cost: sink, priceIn: 2, priceOut: 8}

	ctx := WithMeta(context.Background(), CallMeta{
		TaskID: 42, AgentKey: "writer", Purpose: "agentfactory.react",
	})
	m.record(ctx, "default", "qwen-max", "general", 0.3, "sys", "user", "out",
		Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, time.Second, nil)

	select {
	case ev := <-sink.ch:
		if ev.TaskID != 42 || ev.AgentKey != "writer" {
			t.Fatalf("任务身份丢失: %+v", ev)
		}
		if ev.PromptTokens != 1000 || ev.CompletionTokens != 500 || ev.TotalTokens != 1500 {
			t.Fatalf("token 丢失: %+v", ev)
		}
		if ev.CostMicros != 6000 {
			t.Fatalf("成本不对: %d", ev.CostMicros)
		}
		if ev.PriceVersion == "" {
			t.Fatal("计价版本必须落库")
		}
		if !ev.OK {
			t.Fatal("成功调用应标记 ok")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未记账（charge 没被调用或 goroutine 没跑）")
	}
}

// 没有 token 用量（连不上模型）不该产生一条 0 元的账单行。
func TestChargeSkipsZeroUsage(t *testing.T) {
	sink := &fakeSink{ch: make(chan CostEvent, 1)}
	m := &Manager{cost: sink, priceIn: 2, priceOut: 8}
	m.record(context.Background(), "default", "qwen-max", "general", 0.3, "", "", "",
		Usage{}, time.Second, ErrNoProvider)

	select {
	case ev := <-sink.ch:
		t.Fatalf("零用量不应记账，得到 %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

// 未接线成本中心时不能 panic（最小部署 / 单测）。
func TestChargeWithoutSink(t *testing.T) {
	m := &Manager{}
	m.record(context.Background(), "default", "m", "general", 0.3, "", "", "",
		Usage{PromptTokens: 10, TotalTokens: 10}, time.Second, nil)
	time.Sleep(50 * time.Millisecond)
}
