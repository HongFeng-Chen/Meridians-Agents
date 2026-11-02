// Package models 定义 Milvus 各集合的 Schema（字段、BM25 Function、索引）。
//
// repo 层只调用这里的构造函数与字段常量，不再自行拼装 Schema。
// 一个集合一个文件，集合内的字段常量与构造函数保持同一文件，改集合时只需看一处：
//
//	chunk.go     RAG 主集合（分块，dense + BM25 稀疏混合检索）
//	entity.go    GraphRAG 实体集合
//	relation.go  GraphRAG 关系边集合
//	memory.go    长期记忆集合
//	index.go     索引定义（各集合共用的 HNSW / BM25 参数）
package models
