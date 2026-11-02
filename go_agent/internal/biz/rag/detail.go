package rag

import (
	"encoding/json"

	"go_agent/internal/biz/rag/graphrag"
)

// RecallHitDetail 一条召回命中的明细，落 op_histories.detail 供前端展开查看
type RecallHitDetail struct {
	DocID    string  `json:"doc_id"`
	ChunkIdx int64   `json:"chunk_idx"`
	Source   string  `json:"source"`
	Score    float32 `json:"score"` // RRF 融合分：越高越相关（同一次召回内可比）
	Text     string  `json:"text"`  // 命中片段正文（截断）
}

// RecallDetail RAG 召回明细：过滤条件 + 命中总数 + 逐条命中
type RecallDetail struct {
	Filter   string            `json:"filter"`
	HitCount int               `json:"hit_count"`
	Hits     []RecallHitDetail `json:"hits"`
}

// buildRecallDetail 构造明细 JSON：
// 落库只保留前 20 条（详情弹层够看），正文截断到 240 字，避免单行过大。
func buildRecallDetail(filter string, hits []Hit) string {
	d := RecallDetail{Filter: filter, HitCount: len(hits), Hits: make([]RecallHitDetail, 0, len(hits))}
	for i, h := range hits {
		if i >= 20 {
			break
		}
		d.Hits = append(d.Hits, RecallHitDetail{
			DocID:    h.DocID,
			ChunkIdx: h.ChunkIdx,
			Source:   h.Source,
			Score:    h.Score,
			Text:     clipRunes(h.Text, 240),
		})
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// clipRunes 按字符截断（避免切坏多字节）
func clipRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// GraphEntityDetail 命中实体明细
type GraphEntityDetail struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Freq        int    `json:"freq"`
}

// GraphRelationDetail 命中关系明细
type GraphRelationDetail struct {
	Source      string  `json:"source"`
	Target      string  `json:"target"`
	Relation    string  `json:"relation"`
	Weight      float64 `json:"weight"`
	Description string  `json:"description"`
}

// GraphDetail GraphRAG 召回明细：命中实体 / 命中关系 / 检索子图（证据）/ 回答。
// 落库截断：实体前 20、关系前 30、证据前 10 条（每条 240 字），详情弹层够看且单行不膨胀。
type GraphDetail struct {
	Cached        bool                  `json:"cached"`
	EntityCount   int                   `json:"entity_count"`
	RelationCount int                   `json:"relation_count"`
	EvidenceCount int                   `json:"evidence_count"`
	Answer        string                `json:"answer"`
	Entities      []GraphEntityDetail   `json:"entities"`
	Relations     []GraphRelationDetail `json:"relations"`
	Evidence      []string              `json:"evidence"`
}

// buildGraphDetail 把图谱问答结果序列化为明细 JSON
func buildGraphDetail(ans *graphrag.GraphAnswer) string {
	if ans == nil {
		return ""
	}
	d := GraphDetail{
		Cached:        ans.Cached,
		EntityCount:   len(ans.Entities),
		RelationCount: len(ans.Relations),
		EvidenceCount: len(ans.Evidence),
		Answer:        clipRunes(ans.Answer, 2000),
		Entities:      make([]GraphEntityDetail, 0, len(ans.Entities)),
		Relations:     make([]GraphRelationDetail, 0, len(ans.Relations)),
		Evidence:      make([]string, 0, len(ans.Evidence)),
	}
	for i, e := range ans.Entities {
		if i >= 20 {
			break
		}
		d.Entities = append(d.Entities, GraphEntityDetail{
			Name:        e.Name,
			Type:        e.Type,
			Description: clipRunes(e.Description, 240),
			Freq:        e.Freq,
		})
	}
	for i, r := range ans.Relations {
		if i >= 30 {
			break
		}
		d.Relations = append(d.Relations, GraphRelationDetail{
			Source:      r.Source,
			Target:      r.Target,
			Relation:    r.Relation,
			Weight:      r.Weight,
			Description: clipRunes(r.Description, 240),
		})
	}
	for i, t := range ans.Evidence {
		if i >= 10 {
			break
		}
		d.Evidence = append(d.Evidence, clipRunes(t, 240))
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}
