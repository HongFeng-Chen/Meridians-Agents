package models

import "github.com/milvus-io/milvus/client/v2/entity"

// —— 长期记忆集合字段 ——

const (
	MemoryFieldID       = "id"
	MemoryFieldMemoryID = "memory_id"
	MemoryFieldUserID   = "user_id"
	MemoryFieldKind     = "kind"
	MemoryFieldTags     = "tags"
	MemoryFieldContent  = "content" // BM25 输入字段
	MemoryFieldSparse   = "sparse"
	MemoryFieldDense    = "dense"
)

// MemorySchema 长期记忆集合：content 中文分词 + BM25 Function，可按 user_id 过滤。
func MemorySchema(collection string, dim int) *entity.Schema {
	schema := entity.NewSchema().WithName(collection).WithDescription("long-term memories")
	schema.WithField(entity.NewField().
		WithName(MemoryFieldID).WithDataType(entity.FieldTypeInt64).
		WithIsPrimaryKey(true).WithIsAutoID(true))
	schema.WithField(entity.NewField().
		WithName(MemoryFieldMemoryID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(128))
	schema.WithField(entity.NewField().
		WithName(MemoryFieldUserID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(128))
	schema.WithField(entity.NewField().
		WithName(MemoryFieldKind).WithDataType(entity.FieldTypeVarChar).WithMaxLength(32))
	schema.WithField(entity.NewField().
		WithName(MemoryFieldTags).WithDataType(entity.FieldTypeVarChar).WithMaxLength(2048))
	schema.WithField(entity.NewField().
		WithName(MemoryFieldContent).WithDataType(entity.FieldTypeVarChar).WithMaxLength(16384).
		WithEnableAnalyzer(true).
		WithAnalyzerParams(map[string]any{"type": "chinese"}))
	schema.WithField(entity.NewField().
		WithName(MemoryFieldSparse).WithDataType(entity.FieldTypeSparseVector))
	schema.WithField(entity.NewField().
		WithName(MemoryFieldDense).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim)))
	schema.WithFunction(entity.NewFunction().
		WithName("memory_bm25").
		WithType(entity.FunctionTypeBM25).
		WithInputFields(MemoryFieldContent).
		WithOutputFields(MemoryFieldSparse))
	return schema
}
