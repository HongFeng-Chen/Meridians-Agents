package models

import "github.com/milvus-io/milvus/client/v2/entity"

// —— GraphRAG 关系集合字段 ——

const (
	RelationFieldID          = "id"
	RelationFieldSource      = "source"
	RelationFieldTarget      = "target"
	RelationFieldRelation    = "relation"
	RelationFieldDescription = "description"
	RelationFieldWeight      = "weight"
	RelationFieldRefs        = "chunk_refs"
	RelationFieldDense       = "dense"
)

// RelationSchema GraphRAG 关系边集合（仅 Dense）
func RelationSchema(collection string, dim int) *entity.Schema {
	schema := entity.NewSchema().WithName(collection).WithDescription("GraphRAG relations")
	schema.WithField(entity.NewField().
		WithName(RelationFieldID).WithDataType(entity.FieldTypeInt64).
		WithIsPrimaryKey(true).WithIsAutoID(true))
	schema.WithField(entity.NewField().
		WithName(RelationFieldSource).WithDataType(entity.FieldTypeVarChar).WithMaxLength(512))
	schema.WithField(entity.NewField().
		WithName(RelationFieldTarget).WithDataType(entity.FieldTypeVarChar).WithMaxLength(512))
	schema.WithField(entity.NewField().
		WithName(RelationFieldRelation).WithDataType(entity.FieldTypeVarChar).WithMaxLength(256))
	schema.WithField(entity.NewField().
		WithName(RelationFieldDescription).WithDataType(entity.FieldTypeVarChar).WithMaxLength(8192))
	schema.WithField(entity.NewField().
		WithName(RelationFieldWeight).WithDataType(entity.FieldTypeDouble))
	schema.WithField(entity.NewField().
		WithName(RelationFieldRefs).WithDataType(entity.FieldTypeVarChar).WithMaxLength(16384))
	schema.WithField(entity.NewField().
		WithName(RelationFieldDense).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim)))
	return schema
}
