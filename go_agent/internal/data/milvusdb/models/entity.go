package models

import "github.com/milvus-io/milvus/client/v2/entity"

// —— GraphRAG 实体集合字段 ——

const (
	EntityFieldID          = "id"
	EntityFieldName        = "name"
	EntityFieldType        = "type"
	EntityFieldDescription = "description"
	EntityFieldFreq        = "freq"
	EntityFieldRefs        = "chunk_refs"
	EntityFieldSource      = "source" // "name type: description"，BM25 输入字段
	EntityFieldSparse      = "sparse"
	EntityFieldDense       = "dense"
)

// EntitySchema GraphRAG 实体集合（BM25 + Dense 混合检索）
func EntitySchema(collection string, dim int) *entity.Schema {
	schema := entity.NewSchema().WithName(collection).WithDescription("GraphRAG entities")
	schema.WithField(entity.NewField().
		WithName(EntityFieldID).WithDataType(entity.FieldTypeInt64).
		WithIsPrimaryKey(true).WithIsAutoID(true))
	schema.WithField(entity.NewField().
		WithName(EntityFieldName).WithDataType(entity.FieldTypeVarChar).WithMaxLength(512))
	schema.WithField(entity.NewField().
		WithName(EntityFieldType).WithDataType(entity.FieldTypeVarChar).WithMaxLength(64))
	schema.WithField(entity.NewField().
		WithName(EntityFieldDescription).WithDataType(entity.FieldTypeVarChar).WithMaxLength(8192))
	schema.WithField(entity.NewField().
		WithName(EntityFieldFreq).WithDataType(entity.FieldTypeInt64))
	schema.WithField(entity.NewField().
		WithName(EntityFieldRefs).WithDataType(entity.FieldTypeVarChar).WithMaxLength(16384))
	// BM25 输入字段：名字+类型+描述，开启中文分词
	schema.WithField(entity.NewField().
		WithName(EntityFieldSource).WithDataType(entity.FieldTypeVarChar).WithMaxLength(16384).
		WithEnableAnalyzer(true).
		WithAnalyzerParams(map[string]any{"type": "chinese"}))
	schema.WithField(entity.NewField().
		WithName(EntityFieldSparse).WithDataType(entity.FieldTypeSparseVector))
	schema.WithField(entity.NewField().
		WithName(EntityFieldDense).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim)))
	schema.WithFunction(entity.NewFunction().
		WithName("entity_bm25").
		WithType(entity.FunctionTypeBM25).
		WithInputFields(EntityFieldSource).
		WithOutputFields(EntityFieldSparse))
	return schema
}
