package prompt

// Builtin 内置模板定义：首次启动时写入数据库，之后与普通模板一样可发布、回滚、重置。
type Builtin struct {
	Key         string
	Name        string
	Scene       string
	Description string
	Tags        []string
	Content     string
	Temperature float64
	MaxTokens   int
}

// Builtins 全部内置模板。
//
// 这里的原文即业务兜底：即使数据库不可用、模板被误删，RAG / GraphRAG 仍按此内容运行。
// 修改这里只影响「新环境首次初始化」与 reset 的默认值，不会覆盖线上已有版本。
var Builtins = []Builtin{
	{
		Key:         KeyRAGSystem,
		Name:        "RAG 问答系统提示词",
		Scene:       SceneRAG,
		Description: "约束回答必须基于检索到的参考资料，并标注引用编号",
		Tags:        []string{"rag", "system", "qa"},
		Content: `你是一个严谨的知识库问答助手。请仅根据提供的参考资料回答用户问题。
规则：
1. 只使用参考资料中的信息，不要编造；资料中没有的内容请明确说明"参考资料中未提及"。
2. 回答使用中文，条理清晰，重点内容可分点。
3. 在引用某条资料时，在句子末尾标注其编号，如 [1]、[2]。`,
		Temperature: 0.3,
	},
	{
		Key:         KeyRAGUser,
		Name:        "RAG 问答用户提示词",
		Scene:       SceneRAG,
		Description: "拼接检索片段与用户问题；context 为编号化的参考资料",
		Tags:        []string{"rag", "user"},
		Content:     "参考资料：\n{{context}}\n\n问题：{{question}}",
		Temperature: 0.3,
	},
	{
		Key:         KeyGraphSystem,
		Name:        "GraphRAG 问答系统提示词",
		Scene:       SceneGraph,
		Description: "基于知识图谱做多跳推理，标注实体/关系/原文编号",
		Tags:        []string{"graphrag", "system", "qa"},
		Content: `你是一个基于知识图谱的问答助手。请根据提供的知识图谱（实体、关系）和原文片段回答问题。
规则：
1. 优先利用图谱中的实体与关系进行多跳推理，串联出完整答案。
2. 只使用给定信息，不要编造；信息不足时明确说明。
3. 回答使用中文，条理清晰；引用实体或关系时标注编号，如 [E1]、[R2]，引用原文时标注 [D1]。`,
		Temperature: 0.3,
	},
	{
		Key:         KeyGraphUser,
		Name:        "GraphRAG 问答用户提示词",
		Scene:       SceneGraph,
		Description: "拼接子图上下文与问题；context 含实体、关系与原文片段",
		Tags:        []string{"graphrag", "user"},
		Content:     "知识图谱与原文：\n{{context}}\n\n问题：{{question}}",
		Temperature: 0.3,
	},
	{
		Key:         KeyExtractSystem,
		Name:        "图谱抽取系统提示词",
		Scene:       SceneExtract,
		Description: "从文本块抽取实体与关系，强制 JSON 输出（温度固定 0）",
		Tags:        []string{"graphrag", "extract", "json"},
		Content: `你是知识图谱构建专家。从给定文本中抽取实体和实体间的关系。

要求：
1. 实体：人名、组织、产品、技术、地点、事件、概念等，给出简明中文/原名描述。
2. 关系：实体间的有向关系，source 和 target 必须是已抽取实体的名字（与实体 name 完全一致）。
3. 每个实体带 type（如 人物/组织/产品/技术/地点/概念）和 description（一句话，不超过 50 字）。
4. 每条关系带 relation（谓词短语，如 "隶属于"、"研发"、"位于"）和 description（一句话）。
5. 只抽取文本中明确提到的内容，不要臆造；最多 10 个实体、8 条关系。
6. 仅输出 JSON，不要输出任何其他文字。格式：
{"entities":[{"name":"...","type":"...","description":"..."}],
 "relations":[{"source":"...","target":"...","relation":"...","description":"..."}]}`,
		Temperature: 0,
	},
	{
		Key:         KeyExtractUser,
		Name:        "图谱抽取用户提示词",
		Scene:       SceneExtract,
		Description: "待抽取的文本块；text 为原始分块内容",
		Tags:        []string{"graphrag", "extract"},
		Content:     "{{text}}",
		Temperature: 0,
	},
	{
		Key:         KeyMemorySystem,
		Name:        "记忆问答系统提示词",
		Scene:       SceneMemory,
		Description: "结合短期会话历史与长期记忆回答，引用记忆编号 [M1]、图谱编号 [E1]/[R1]",
		Tags:        []string{"memory", "system", "qa"},
		Content: `你是一个拥有长期记忆与短期会话上下文的 AI 助手。请结合【历史对话】与【记忆上下文】回答用户问题。
规则：
1. 优先使用记忆中明确记录的事实与偏好；记忆与历史冲突时，以最近的历史对话为准。
2. 只使用给定信息，不要编造；记忆中没有的内容请明确说明"记忆中没有相关记录"。
3. 回答使用中文，条理清晰；引用长期记忆时标注编号如 [M1]，引用图谱实体/关系时标注 [E1]/[R1]。
4. 如果用户表达了新的偏好或重要事实，在回答末尾用一行「记忆点：...」简短总结（供系统沉淀）。`,
		Temperature: 0.3,
	},
	{
		Key:         KeyMemoryUser,
		Name:        "记忆问答用户提示词",
		Scene:       SceneMemory,
		Description: "拼接会话历史、召回的记忆与图谱、当前问题",
		Tags:        []string{"memory", "user"},
		Content: `【历史对话】
{{history}}

【记忆上下文】
{{context}}

【当前问题】
{{question}}`,
		Temperature: 0.3,
	},
}

// BuiltinOf 按 key 查内置模板
func BuiltinOf(key string) (Builtin, bool) {
	for _, b := range Builtins {
		if b.Key == key {
			return b, true
		}
	}
	return Builtin{}, false
}

// BuiltinContent 返回内置模板原文；未知 key 返回空串
func BuiltinContent(key string) string {
	if b, ok := BuiltinOf(key); ok {
		return b.Content
	}
	return ""
}

// RenderBuiltin 用 vars 渲染内置模板；ok=false 表示 key 未内置
func RenderBuiltin(key string, vars map[string]string) (string, bool) {
	b, ok := BuiltinOf(key)
	if !ok {
		return "", false
	}
	out, _ := Render(b.Content, vars)
	return out, true
}
