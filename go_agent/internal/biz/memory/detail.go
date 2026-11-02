package memory

import "encoding/json"

// RecallMemoryDetail 一条被召回的记忆明细，落 op_histories.detail 供前端展开
type RecallMemoryDetail struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Content    string  `json:"content"` // 记忆正文（截断）
	Score      float32 `json:"score"`   // 向量相似度：0~1，越高越相关
	Importance int     `json:"importance"`
	Source     string  `json:"source"`
}

// RecallDetail 记忆召回明细：查询 + 命中总数 + 逐条记忆 + 图谱规模
type RecallDetail struct {
	Query          string               `json:"query"`
	UserID         string               `json:"user_id"`
	WithGraph      bool                 `json:"with_graph"`
	HitCount       int                  `json:"hit_count"`
	Memories       []RecallMemoryDetail `json:"memories"`
	GraphEntities  int                  `json:"graph_entities"`
	GraphRelations int                  `json:"graph_relations"`
}

// buildRecallDetail 构造记忆召回明细 JSON：最多保留 20 条，正文截断到 240 字。
func buildRecallDetail(res *RecallResult, withGraph bool) string {
	if res == nil {
		return ""
	}
	d := RecallDetail{
		Query:     res.Query,
		UserID:    res.UserID,
		WithGraph: withGraph,
		HitCount:  len(res.Memories),
		Memories:  make([]RecallMemoryDetail, 0, len(res.Memories)),
	}
	if res.Graph != nil {
		d.GraphEntities = len(res.Graph.Entities)
		d.GraphRelations = len(res.Graph.Relations)
	}
	for i, m := range res.Memories {
		if i >= 20 {
			break
		}
		item := RecallMemoryDetail{Score: m.Score}
		if m.LongTermMemory != nil {
			item.ID = m.ID
			item.Kind = m.Kind
			item.Content = clipText(m.Content, 240)
			item.Importance = m.Importance
			item.Source = m.Source
		}
		d.Memories = append(d.Memories, item)
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// clipText 按字符截断（避免切坏多字节）
func clipText(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
