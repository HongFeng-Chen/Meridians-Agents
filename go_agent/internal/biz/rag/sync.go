package rag

// 本文件：文档索引同步（知识空间文档列表的「同步索引」按钮）。
//
// 一篇文档可以只建一种索引（rag 向量 / graph 图谱），也可以双索引（both）。
// 这里把「缺哪种补哪种、补齐后升级为 both」做成一个幂等入口：
//
//	rag   → 从 Milvus 分块按 chunk_idx 重建原文，补建图谱
//	graph → 从 rag_doc_texts 读入库时保存的原文，重放分块补建向量
//	both  → 已是双索引，直接返回（Deduped=true）
//
// 权限：文档可写（CanWriteDoc，与覆盖入库同一道闸）。
// 失败不改变文档状态——缺的还是缺，可以重试。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v3/log"
	"go_agent/internal/pkg/identity"
)

// syncChunkLimit 重建原文时最多取回的分块数（1 万块 ≈ 数百万字符，足够覆盖现有文档）。
const syncChunkLimit = 10000

// SyncDocIndex 补建文档缺失的索引，补齐后文档升级为双索引（both）。
func (r *Rag) SyncDocIndex(ctx context.Context, docID string) (*IngestResult, error) {
	docID = strings.TrimSpace(docID)
	if docID == "" {
		return nil, fmt.Errorf("doc_id 不能为空")
	}
	doc, err := r.repo.Get(ctx, docID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("文档不存在: %s", docID)
	}
	// 与覆盖入库同一道写权限闸：文档所有者 / 作用域管理员 / 空间管理员
	if !CanWriteDoc(ctx, doc) {
		return nil, fmt.Errorf("无权修改文档 %s", docID)
	}
	if doc.Status != "ready" {
		return nil, fmt.Errorf("文档当前状态为 %s（未就绪），请先处理入库失败问题再同步索引", doc.Status)
	}

	start := time.Now()
	switch doc.Mode {
	case ModeBoth:
		return &IngestResult{
			DocID: docID, Source: doc.Source, Mode: ModeBoth,
			Chunks: doc.Chunks, Entities: doc.Entities, Relations: doc.Relations,
			Deduped: true, Duration: time.Since(start),
		}, nil

	case ModeGraph:
		return r.syncBuildVector(ctx, doc, start)

	default: // ModeVector（含历史空值，按仅向量处理）
		return r.syncBuildGraph(ctx, doc, start)
	}
}

// syncBuildGraph 仅向量 → 补建图谱：从 Milvus 分块按 chunk_idx 重建原文。
func (r *Rag) syncBuildGraph(ctx context.Context, doc *Document, start time.Time) (*IngestResult, error) {
	items, err := r.store.GetTexts(ctx, fmt.Sprintf("doc_id == %q", doc.DocID), syncChunkLimit)
	if err != nil {
		return nil, fmt.Errorf("读取文档分块失败: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("向量分块为空，无法重建原文；请删除后用双索引模式重新入库")
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ChunkIdx < items[j].ChunkIdx })
	parts := make([]string, 0, len(items))
	for _, h := range items {
		parts = append(parts, h.Text)
	}
	ents, rels, err := r.graph.Ingest(ctx, doc.DocID, doc.Source, strings.Join(parts, "\n"))
	if err != nil {
		return nil, fmt.Errorf("图谱抽取失败: %w", err)
	}
	if err := r.repo.Upsert(ctx, &Document{
		DocID: doc.DocID, UserID: doc.UserID, TenantID: doc.TenantID,
		Scope: doc.Scope, SpaceID: doc.SpaceID,
		Source: doc.Source, Mode: ModeBoth, Status: "ready",
		Chars: doc.Chars, Chunks: doc.Chunks,
		Entities: ents, Relations: rels,
		Checksum: doc.Checksum,
	}); err != nil {
		return nil, fmt.Errorf("写入文档元数据: %w", err)
	}
	_ = r.cache.Invalidate(ctx)
	log.Info("[rag] 同步索引：向量文档已补建图谱", "doc", doc.DocID, "entities", ents, "relations", rels,
		"operator", identity.UserID(ctx), "cost", time.Since(start).String())
	return &IngestResult{
		DocID: doc.DocID, Source: doc.Source, Mode: ModeBoth,
		Chars: doc.Chars, Chunks: doc.Chunks, Entities: ents, Relations: rels,
		Duration: time.Since(start),
	}, nil
}

// syncBuildVector 仅图谱 → 补建向量：用入库时保存的原文重放分块。
// 历史文档（原文落库上线前入库的）没有原文，明确告知需要重新入库。
func (r *Rag) syncBuildVector(ctx context.Context, doc *Document, start time.Time) (*IngestResult, error) {
	ts, ok := r.repo.(DocumentTextStore)
	if !ok {
		return nil, fmt.Errorf("当前部署未启用原文持久化，无法补建向量索引；请重新入库")
	}
	text, err := ts.GetText(ctx, doc.DocID)
	if err != nil {
		return nil, fmt.Errorf("读取文档原文失败: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("该文档入库时未保存原文（旧数据），无法补建向量索引；请删除后用双索引模式重新入库")
	}
	chunks, err := r.ingestVectors(ctx, doc.DocID, doc.Source, text)
	if err != nil {
		return nil, fmt.Errorf("向量索引构建失败: %w", err)
	}
	if err := r.repo.Upsert(ctx, &Document{
		DocID: doc.DocID, UserID: doc.UserID, TenantID: doc.TenantID,
		Scope: doc.Scope, SpaceID: doc.SpaceID,
		Source: doc.Source, Mode: ModeBoth, Status: "ready",
		Chars: doc.Chars, Chunks: chunks,
		Entities: doc.Entities, Relations: doc.Relations,
		Checksum: doc.Checksum,
	}); err != nil {
		return nil, fmt.Errorf("写入文档元数据: %w", err)
	}
	_ = r.cache.Invalidate(ctx)
	log.Info("[rag] 同步索引：图谱文档已补建向量", "doc", doc.DocID, "chunks", chunks,
		"operator", identity.UserID(ctx), "cost", time.Since(start).String())
	return &IngestResult{
		DocID: doc.DocID, Source: doc.Source, Mode: ModeBoth,
		Chars: doc.Chars, Chunks: chunks, Entities: doc.Entities, Relations: doc.Relations,
		Duration: time.Since(start),
	}, nil
}
