// extractor.go 使用 LLM 从文本块中抽取实体与关系（JSON 输出）。
// 提示词由 /api/prompts 管理（key: graph.extract.system / graph.extract.user）。
package graphrag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go_agent/internal/biz/prompt"
)

// Extractor 实体关系抽取器
type Extractor struct {
	llm          Chatter
	prompts      Prompter
	maxEntities  int
	maxRelations int
}

// NewExtractor 创建抽取器（temperature=0 保证输出稳定）
func NewExtractor(l Chatter, p Prompter, maxEntities, maxRelations int) *Extractor {
	return &Extractor{llm: l, prompts: p, maxEntities: maxEntities, maxRelations: maxRelations}
}

// Extract 从单个文本块抽取实体与关系
func (e *Extractor) Extract(ctx context.Context, text string) (*Extraction, error) {
	system := renderPrompt(ctx, e.prompts, prompt.KeyExtractSystem, nil)
	user := renderPrompt(ctx, e.prompts, prompt.KeyExtractUser, map[string]string{"text": text})
	resp, err := e.llm.ChatTemp(ctx, system, user, 0)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	ex, err := parseExtraction(resp)
	if err != nil {
		return nil, fmt.Errorf("parse extraction: %w (raw: %.200s)", err, resp)
	}
	return e.clamp(ex), nil
}

// clamp 截断超限的抽取结果
func (e *Extractor) clamp(ex *Extraction) *Extraction {
	if len(ex.Entities) > e.maxEntities {
		ex.Entities = ex.Entities[:e.maxEntities]
	}
	if len(ex.Relations) > e.maxRelations {
		ex.Relations = ex.Relations[:e.maxRelations]
	}
	// 过滤掉 source/target 不在实体集内的关系，保证图的一致性
	names := make(map[string]struct{}, len(ex.Entities))
	for _, en := range ex.Entities {
		if strings.TrimSpace(en.Name) == "" {
			continue
		}
		names[normName(en.Name)] = struct{}{}
	}
	kept := ex.Relations[:0]
	for _, r := range ex.Relations {
		if strings.TrimSpace(r.Source) == "" || strings.TrimSpace(r.Target) == "" {
			continue
		}
		_, sOK := names[normName(r.Source)]
		_, tOK := names[normName(r.Target)]
		if sOK && tOK {
			kept = append(kept, r)
		}
	}
	ex.Relations = kept
	return ex
}

// parseExtraction 解析 LLM 返回的 JSON（容忍 markdown 代码块包裹）
func parseExtraction(s string) (*Extraction, error) {
	s = strings.TrimSpace(s)
	// 剥离 ```json ... ``` 代码块
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var ex Extraction
	if err := json.Unmarshal([]byte(s), &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}
