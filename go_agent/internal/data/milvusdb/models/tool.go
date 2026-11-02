package models

import "github.com/milvus-io/milvus/client/v2/entity"

// —— 工具向量集合字段 ——
//
// 只存召回必需的信息：主键（= tools.id）+ 向量 + 过滤字段。
// 工具详情（schema / 描述全文）留在 MySQL，召回后按 id 回查，
// 避免向量库成为第二份真相。
const (
	ToolFieldID        = "tool_id"
	ToolFieldServerID  = "server_id"
	ToolFieldNamespace = "ns" // namespace 是 Milvus 保留字，字段名缩写成 ns
	ToolFieldEnabled   = "is_enabled"
	ToolFieldDense     = "dense"
)

// ToolSchema 工具向量集合：只有 dense 向量，按 ns / is_enabled 过滤召回。
func ToolSchema(collection string, dim int) *entity.Schema {
	schema := entity.NewSchema().
		WithName(collection).
		WithDescription("MCP tools: dense-only embedding for semantic tool retrieval")
	schema.WithField(entity.NewField().
		WithName(ToolFieldID).WithDataType(entity.FieldTypeInt64).
		WithIsPrimaryKey(true).WithIsAutoID(false)) // 主键取 MySQL tools.id，需要手工写入
	schema.WithField(entity.NewField().
		WithName(ToolFieldServerID).WithDataType(entity.FieldTypeInt64))
	schema.WithField(entity.NewField().
		WithName(ToolFieldNamespace).WithDataType(entity.FieldTypeVarChar).WithMaxLength(128))
	schema.WithField(entity.NewField().
		WithName(ToolFieldEnabled).WithDataType(entity.FieldTypeBool))
	schema.WithField(entity.NewField().
		WithName(ToolFieldDense).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim)))
	return schema
}
