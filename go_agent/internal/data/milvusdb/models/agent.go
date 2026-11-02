package models

import "github.com/milvus-io/milvus/client/v2/entity"

// —— Agent 向量注册表字段 ——
//
// 只存召回必需的信息：主键（= agent_definitions.id）+ 向量 + 过滤字段。
// 定义详情（能力白名单/提示词全文）留在 MySQL/Redis，召回后按 id 回查。
const (
	AgentFieldID      = "agent_id"
	AgentFieldEnabled = "is_enabled"
	AgentFieldSource  = "source"
	AgentFieldDense   = "dense"
)

// AgentSchema Agent 向量注册表：dense-only，按 is_enabled / source 过滤召回
func AgentSchema(collection string, dim int) *entity.Schema {
	schema := entity.NewSchema().
		WithName(collection).
		WithDescription("agentfactory agents: dense-only embedding for semantic agent retrieval")
	schema.WithField(entity.NewField().
		WithName(AgentFieldID).WithDataType(entity.FieldTypeInt64).
		WithIsPrimaryKey(true).WithIsAutoID(false)) // 主键取 MySQL agent_definitions.id
	schema.WithField(entity.NewField().
		WithName(AgentFieldEnabled).WithDataType(entity.FieldTypeBool))
	schema.WithField(entity.NewField().
		WithName(AgentFieldSource).WithDataType(entity.FieldTypeVarChar).WithMaxLength(16))
	schema.WithField(entity.NewField().
		WithName(AgentFieldDense).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim)))
	return schema
}
