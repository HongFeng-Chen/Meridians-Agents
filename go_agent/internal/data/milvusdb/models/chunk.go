package models

import "github.com/milvus-io/milvus/client/v2/entity"

// —— 主文档集合（RAG 分块）字段 ——

const (
	ChunkFieldID      = "id"
	ChunkFieldDocID   = "doc_id"
	ChunkFieldSource  = "source"
	ChunkFieldText    = "text"
	ChunkFieldChunkID = "chunk_idx"
	ChunkFieldSparse  = "sparse"
	ChunkFieldDense   = "dense"
)

// ChunkSchema 主集合：text 字段开启中文分词 + BM25 Function 服务端生成稀疏向量，
// dense 字段存外部 Embedding 结果。
func ChunkSchema(collection string, dim int) *entity.Schema {
	schema := entity.NewSchema().
		WithName(collection).
		WithDescription("RAG documents: dense + BM25 sparse hybrid")
	schema.WithField(entity.NewField().
		WithName(ChunkFieldID).WithDataType(entity.FieldTypeInt64).
		WithIsPrimaryKey(true).WithIsAutoID(true))
	schema.WithField(entity.NewField().
		WithName(ChunkFieldDocID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(256))
	schema.WithField(entity.NewField().
		WithName(ChunkFieldChunkID).WithDataType(entity.FieldTypeInt64))
	schema.WithField(entity.NewField().
		WithName(ChunkFieldSource).WithDataType(entity.FieldTypeVarChar).WithMaxLength(1024))
	schema.WithField(entity.NewField().
		WithName(ChunkFieldText).WithDataType(entity.FieldTypeVarChar).WithMaxLength(32768).
		WithEnableAnalyzer(true).
		// 中文场景用 jieba 分词器，保证 BM25 中文分词正确
		WithAnalyzerParams(map[string]any{"type": "chinese"}))
	schema.WithField(entity.NewField().
		WithName(ChunkFieldSparse).WithDataType(entity.FieldTypeSparseVector))
	schema.WithField(entity.NewField().
		WithName(ChunkFieldDense).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim)))
	// BM25 Function：服务端由 text 自动生成 sparse
	schema.WithFunction(entity.NewFunction().
		WithName("text_bm25").
		WithType(entity.FunctionTypeBM25).
		WithInputFields(ChunkFieldText).
		WithOutputFields(ChunkFieldSparse))
	return schema
}
