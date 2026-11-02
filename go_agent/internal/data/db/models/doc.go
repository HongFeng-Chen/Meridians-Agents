// Package models 集中存放 MySQL PO（Persistent Object）。
//
// PO 只做表映射（gorm tag + TableName），业务转换与查询逻辑在 internal/data/db/repo 中。
// 文件按表域拆分，一个业务对象一个文件：
//
//	document.go  rag_documents              文档元数据
//	memory.go    memories                   长期记忆
//	prompt.go    prompt_templates / prompt_versions  提示词模板与版本快照
//	llm.go       llm_providers              大模型数据源
//	skill.go     skills / skill_files / skill_hubs   技能、技能文件、技能市场
//
// 新增表时在对应域文件里追加 PO，跨域的新对象才新建文件，避免又退回单个大文件。
package models
