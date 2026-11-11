package mcp

import "context"

// Repo MCP 配置与工具元数据仓库（MySQL 实现，位于 data/db/repo）。
type Repo interface {
	// —— 服务 ——

	ListServers(ctx context.Context, f ServerFilter) ([]Server, error)
	GetServer(ctx context.Context, id uint64) (*Server, error)
	GetServerByName(ctx context.Context, name string) (*Server, error)
	CreateServer(ctx context.Context, s *Server) error
	UpdateServer(ctx context.Context, s *Server) error
	DeleteServer(ctx context.Context, id uint64) error
	SetServerEnabled(ctx context.Context, id uint64, enabled bool) error

	// —— 工具 ——

	// UpsertTools 以 tool_name 为键对某服务的工具做差异同步：
	// 新增 / 更新（内容变化才写）/ 软删除远端已消失的工具。
	// 返回差异统计与「带数据库 ID 的工具列表」，后者供向量化使用。
	UpsertTools(ctx context.Context, serverID uint64, tools []Tool) (ToolSync, []Tool, error)
	ListTools(ctx context.Context, f ToolFilter) ([]Tool, error)
	GetTool(ctx context.Context, id uint64) (*Tool, error)
	GetTools(ctx context.Context, ids []uint64) ([]Tool, error)
	GetToolByUniqueName(ctx context.Context, uniqueName string) (*Tool, error)
	SetToolEnabled(ctx context.Context, id uint64, enabled bool) error
	DeleteTool(ctx context.Context, id uint64) error
	// MarkVectorized 标记向量是否已入库（embedding 失败时置 false）
	MarkVectorized(ctx context.Context, ids []uint64, vectorized bool) error
	Stats(ctx context.Context) (Stats, error)

	// —— 幂等记录 ——
	// 任务执行链路的工具调用去重依据，语义见 idem.go。
	IdemStore
}

// VectorStore 工具向量存储（Milvus 实现，位于 data/milvusdb/repo）。
//
// 只存 tool_id + 向量 + 过滤字段；召回结果按 id 回 MySQL/Redis 取详情。
type VectorStore interface {
	EnsureCollection(ctx context.Context) error
	// Upsert 覆盖写：tool_id 为主键，重复写入即更新（含 is_enabled 等标量）
	Upsert(ctx context.Context, tools []Tool, vecs [][]float32) error
	Search(ctx context.Context, vec []float32, topK int, f VectorFilter) ([]VectorHit, error)
	Delete(ctx context.Context, toolIDs []uint64) error
	Count(ctx context.Context) (int64, error)
}

// VectorFilter 向量检索过滤条件
type VectorFilter struct {
	Namespace   string
	ServerID    uint64
	EnabledOnly bool
}

// VectorHit 向量命中
type VectorHit struct {
	ToolID uint64
	Score  float64
}

// Cache MCP 热点缓存（Redis 实现，位于 data/redis）。
type Cache interface {
	// GetTools 批量读取工具对象；缺失的 id 不出现在返回 map 中
	GetTools(ctx context.Context, ids []uint64) (map[uint64]Tool, error)
	PutTools(ctx context.Context, tools []Tool) error
	DeleteTools(ctx context.Context, ids []uint64)

	GetStatus(ctx context.Context, serverID uint64) (Status, bool)
	GetStatuses(ctx context.Context, serverIDs []uint64) map[uint64]Status
	PutStatus(ctx context.Context, st Status)
	DeleteStatus(ctx context.Context, serverID uint64)

	// NamespaceIndex 维护 namespace → toolID 集合（Redis Set），供按命名空间快速取工具
	AddToNamespace(ctx context.Context, namespace string, toolIDs []uint64)
	RemoveFromNamespace(ctx context.Context, namespace string, toolIDs []uint64)
	NamespaceTools(ctx context.Context, namespace string) []uint64
}
