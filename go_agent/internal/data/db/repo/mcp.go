// mcp.go MCP 配置与工具元数据仓储：
// mcp_servers（服务配置）、tools（工具元数据 + 向量化标记）。
//
// 工具同步按 tool_name 做差异：远端新增插入、内容变化才更新、
// 远端消失的软删除——避免每次同步都全量重写（会丢掉本地启停状态）。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go_agent/internal/biz/mcp"
	"go_agent/internal/conf"
	"go_agent/internal/data/db"
	"go_agent/internal/data/db/models"

	"gorm.io/gorm"
)

// MCPRepo MCP 仓库
type MCPRepo struct {
	db *gorm.DB
}

var _ mcp.Repo = (*MCPRepo)(nil)

// NewMCPRepo 创建仓库；auto_migrate 打开时自动建表
func NewMCPRepo(c *conf.Data, d *db.Data) (mcp.Repo, error) {
	r := &MCPRepo{db: d.DB()}
	if c.GetDatabase().GetAutoMigrate() {
		if err := r.db.AutoMigrate(&models.MCPServerPO{}, &models.ToolPO{}); err != nil {
			return nil, fmt.Errorf("迁移 mcp 表: %w", err)
		}
	}
	return r, nil
}

// —— 行结构：PO + 关联出来的服务名/命名空间 ——

type serverRow struct {
	models.MCPServerPO
	ToolCount int `gorm:"column:tool_count"`
}

type toolRow struct {
	models.ToolPO
	ServerName string `gorm:"column:server_name"`
	Namespace  string `gorm:"column:namespace"`
}

// —— 服务 ——

// ListServers 服务列表（带工具数量，不含工具详情）
func (r *MCPRepo) ListServers(ctx context.Context, f mcp.ServerFilter) ([]mcp.Server, error) {
	counts := r.db.Model(&models.ToolPO{}).
		Select("mcp_server_id, COUNT(*) AS cnt").
		Group("mcp_server_id")

	q := r.db.WithContext(ctx).
		Table("mcp_servers AS s").
		Select("s.*, COALESCE(t.cnt, 0) AS tool_count").
		Joins("LEFT JOIN (?) AS t ON t.mcp_server_id = s.id", counts).
		Where("s.deleted_at IS NULL")

	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		q = q.Where("(s.name LIKE ? ESCAPE '\\\\' OR s.description LIKE ? ESCAPE '\\\\')", like, like)
	}
	if ns := strings.TrimSpace(f.Namespace); ns != "" {
		q = q.Where("s.namespace = ?", ns)
	}
	if tr := strings.TrimSpace(f.Transport); tr != "" {
		q = q.Where("s.transport = ?", tr)
	}
	if f.Enabled != nil {
		q = q.Where("s.is_enabled = ?", *f.Enabled)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	var rows []serverRow
	if err := q.Order("s.id ASC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]mcp.Server, 0, len(rows))
	for i := range rows {
		s := serverToBiz(&rows[i].MCPServerPO)
		s.ToolCount = rows[i].ToolCount
		out = append(out, *s)
	}
	return out, nil
}

// GetServer 服务详情（含工具数量；工具明细由用例层按需查）
func (r *MCPRepo) GetServer(ctx context.Context, id uint64) (*mcp.Server, error) {
	var po models.MCPServerPO
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, mcp.ErrServerNotFound
	}
	if err != nil {
		return nil, err
	}
	s := serverToBiz(&po)
	var n int64
	if err := r.db.WithContext(ctx).Model(&models.ToolPO{}).
		Where("mcp_server_id = ?", id).Count(&n).Error; err != nil {
		return nil, err
	}
	s.ToolCount = int(n)
	return s, nil
}

// GetServerByName 按名称取服务（工具唯一名前缀用）
func (r *MCPRepo) GetServerByName(ctx context.Context, name string) (*mcp.Server, error) {
	var po models.MCPServerPO
	err := r.db.WithContext(ctx).Where("name = ?", strings.TrimSpace(name)).First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, mcp.ErrServerNotFound
	}
	if err != nil {
		return nil, err
	}
	return serverToBiz(&po), nil
}

// CreateServer 创建服务（name 唯一）
func (r *MCPRepo) CreateServer(ctx context.Context, s *mcp.Server) error {
	po := serverFromBiz(s)
	if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
		if isDuplicate(err) {
			return fmt.Errorf("%w: %s", mcp.ErrServerExists, s.Name)
		}
		return err
	}
	s.ID = po.ID
	s.CreatedAt = po.CreatedAt
	s.UpdatedAt = po.UpdatedAt
	return nil
}

// UpdateServer 更新服务配置（字段全量覆盖，允许清空）
func (r *MCPRepo) UpdateServer(ctx context.Context, s *mcp.Server) error {
	po := serverFromBiz(s)
	res := r.db.WithContext(ctx).Model(&models.MCPServerPO{}).
		Where("id = ?", s.ID).
		Updates(map[string]any{
			"name":                  po.Name,
			"namespace":             po.Namespace,
			"transport":             po.Transport,
			"is_enabled":            po.Enabled,
			"description":           po.Description,
			"endpoint":              po.Endpoint,
			"headers":               po.Headers,
			"command":               po.Command,
			"args":                  po.Args,
			"env":                   po.Env,
			"health_check_interval": po.HealthCheckInterval,
		})
	if res.Error != nil {
		if isDuplicate(res.Error) {
			return fmt.Errorf("%w: %s", mcp.ErrServerExists, s.Name)
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return mcp.ErrServerNotFound
	}
	return nil
}

// DeleteServer 软删除服务
func (r *MCPRepo) DeleteServer(ctx context.Context, id uint64) error {
	res := r.db.WithContext(ctx).Delete(&models.MCPServerPO{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return mcp.ErrServerNotFound
	}
	return nil
}

// SetServerEnabled 启停服务
func (r *MCPRepo) SetServerEnabled(ctx context.Context, id uint64, enabled bool) error {
	res := r.db.WithContext(ctx).Model(&models.MCPServerPO{}).
		Where("id = ?", id).Update("is_enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return mcp.ErrServerNotFound
	}
	return nil
}

// —— 工具 ——

// UpsertTools 以 tool_name 为键做差异同步。
//
// 本地启停状态（is_enabled）不被远端覆盖：工具停用是运维决策，
// 重新同步不该把它又打开；但内容变化时要把 vectorized 置 false，等重新向量化。
func (r *MCPRepo) UpsertTools(ctx context.Context, serverID uint64, tools []mcp.Tool) (mcp.ToolSync, []mcp.Tool, error) {
	var stat mcp.ToolSync

	var existing []models.ToolPO
	if err := r.db.WithContext(ctx).Where("mcp_server_id = ?", serverID).Find(&existing).Error; err != nil {
		return stat, nil, err
	}
	byName := make(map[string]*models.ToolPO, len(existing))
	for i := range existing {
		byName[existing[i].ToolName] = &existing[i]
	}

	out := make([]mcp.Tool, 0, len(tools))
	for i := range tools {
		t := tools[i]
		po := toolFromBiz(serverID, &t)

		if old, ok := byName[t.ToolName]; ok {
			delete(byName, t.ToolName)
			t.ID = old.ID
			t.Enabled = old.Enabled // 保留本地启停状态
			t.CreatedAt = old.CreatedAt
			if toolChanged(old, po) {
				if err := r.db.WithContext(ctx).Model(&models.ToolPO{}).
					Where("id = ?", old.ID).
					Updates(map[string]any{
						"unique_name":   po.UniqueName,
						"display_name":  po.DisplayName,
						"description":   po.Description,
						"input_schema":  po.InputSchema,
						"output_schema": po.OutputSchema,
						"examples":      po.Examples,
						"tags":          po.Tags,
						"version":       po.Version,
						"route":         po.Route,
						"vectorized":    false, // 内容变了，旧向量作废
					}).Error; err != nil {
					return stat, nil, err
				}
				stat.Updated++
				t.Vectorized = false
			} else {
				stat.Unchanged++
				t.Vectorized = old.Vectorized
			}
			out = append(out, t)
			continue
		}

		id, err := r.insertTool(ctx, serverID, po)
		if err != nil {
			return stat, nil, err
		}
		t.ID = id
		stat.Added++
		out = append(out, t)
	}

	// 远端已消失的工具：软删除（保留行，便于审计与恢复）
	if len(byName) > 0 {
		ids := make([]uint64, 0, len(byName))
		for _, po := range byName {
			ids = append(ids, po.ID)
		}
		if err := r.db.WithContext(ctx).Where("id IN ?", ids).Delete(&models.ToolPO{}).Error; err != nil {
			return stat, nil, err
		}
		stat.Removed = len(ids)
		stat.RemovedIDs = ids
	}
	return stat, out, nil
}

// insertTool 新建工具；同名 unique_name 曾被软删除时直接复活并改挂到当前服务
// （否则唯一索引会让「删了服务再建」永久失败）。
func (r *MCPRepo) insertTool(ctx context.Context, serverID uint64, po *models.ToolPO) (uint64, error) {
	var old models.ToolPO
	err := r.db.Unscoped().Where("unique_name = ?", po.UniqueName).First(&old).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := r.db.WithContext(ctx).Create(po).Error; err != nil {
			return 0, fmt.Errorf("插入工具 %s: %w", po.UniqueName, err)
		}
		return po.ID, nil
	}
	if err != nil {
		return 0, err
	}
	if err := r.db.Unscoped().Model(&models.ToolPO{}).Where("id = ?", old.ID).
		Updates(map[string]any{
			"mcp_server_id": serverID,
			"tool_name":     po.ToolName,
			"display_name":  po.DisplayName,
			"description":   po.Description,
			"input_schema":  po.InputSchema,
			"output_schema": po.OutputSchema,
			"examples":      po.Examples,
			"tags":          po.Tags,
			"version":       po.Version,
			"route":         po.Route,
			"is_enabled":    po.Enabled,
			"vectorized":    false,
			"deleted_at":    nil,
		}).Error; err != nil {
		return 0, fmt.Errorf("复活工具 %s: %w", po.UniqueName, err)
	}
	return old.ID, nil
}

// ListTools 工具列表（联表带出服务名与命名空间）
func (r *MCPRepo) ListTools(ctx context.Context, f mcp.ToolFilter) ([]mcp.Tool, error) {
	q := r.db.WithContext(ctx).
		Table("tools AS t").
		Select("t.*, s.name AS server_name, s.namespace AS namespace").
		Joins("JOIN mcp_servers AS s ON s.id = t.mcp_server_id AND s.deleted_at IS NULL").
		Where("t.deleted_at IS NULL")

	if f.ServerID > 0 {
		q = q.Where("t.mcp_server_id = ?", f.ServerID)
	}
	if ns := strings.TrimSpace(f.Namespace); ns != "" {
		q = q.Where("s.namespace = ?", ns)
	}
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		q = q.Where("(t.unique_name LIKE ? ESCAPE '\\\\' OR t.tool_name LIKE ? ESCAPE '\\\\' OR t.description LIKE ? ESCAPE '\\\\')",
			like, like, like)
	}
	if f.Enabled != nil {
		q = q.Where("t.is_enabled = ?", *f.Enabled)
	}
	if f.Vectorized != nil {
		q = q.Where("t.vectorized = ?", *f.Vectorized)
	}
	if len(f.IDs) > 0 {
		q = q.Where("t.id IN ?", f.IDs)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	var rows []toolRow
	if err := q.Order("t.mcp_server_id ASC, t.id ASC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]mcp.Tool, 0, len(rows))
	for i := range rows {
		out = append(out, *toolToBiz(&rows[i].ToolPO, rows[i].ServerName, rows[i].Namespace))
	}
	return out, nil
}

// GetTool 单个工具
func (r *MCPRepo) GetTool(ctx context.Context, id uint64) (*mcp.Tool, error) {
	list, err := r.ListTools(ctx, mcp.ToolFilter{IDs: []uint64{id}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, mcp.ErrToolNotFound
	}
	return &list[0], nil
}

// GetTools 批量取工具（召回后补详情用）
func (r *MCPRepo) GetTools(ctx context.Context, ids []uint64) ([]mcp.Tool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return r.ListTools(ctx, mcp.ToolFilter{IDs: ids, Limit: len(ids)})
}

// GetToolByUniqueName 按全局唯一名取工具
func (r *MCPRepo) GetToolByUniqueName(ctx context.Context, uniqueName string) (*mcp.Tool, error) {
	var row toolRow
	err := r.db.WithContext(ctx).
		Table("tools AS t").
		Select("t.*, s.name AS server_name, s.namespace AS namespace").
		Joins("JOIN mcp_servers AS s ON s.id = t.mcp_server_id AND s.deleted_at IS NULL").
		Where("t.deleted_at IS NULL AND t.unique_name = ?", strings.TrimSpace(uniqueName)).
		Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, mcp.ErrToolNotFound
	}
	return toolToBiz(&row.ToolPO, row.ServerName, row.Namespace), nil
}

// SetToolEnabled 启停工具
func (r *MCPRepo) SetToolEnabled(ctx context.Context, id uint64, enabled bool) error {
	res := r.db.WithContext(ctx).Model(&models.ToolPO{}).
		Where("id = ?", id).Update("is_enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return mcp.ErrToolNotFound
	}
	return nil
}

// DeleteTool 软删除工具
func (r *MCPRepo) DeleteTool(ctx context.Context, id uint64) error {
	res := r.db.WithContext(ctx).Delete(&models.ToolPO{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return mcp.ErrToolNotFound
	}
	return nil
}

// MarkVectorized 标记向量化状态
func (r *MCPRepo) MarkVectorized(ctx context.Context, ids []uint64, vectorized bool) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&models.ToolPO{}).
		Where("id IN ?", ids).Update("vectorized", vectorized).Error
}

// Stats 规模统计
func (r *MCPRepo) Stats(ctx context.Context) (mcp.Stats, error) {
	var st mcp.Stats
	db := r.db.WithContext(ctx)
	if err := db.Model(&models.MCPServerPO{}).Count(&st.Servers).Error; err != nil {
		return st, err
	}
	if err := db.Model(&models.MCPServerPO{}).Where("is_enabled = ?", true).Count(&st.EnabledServers).Error; err != nil {
		return st, err
	}
	if err := db.Model(&models.ToolPO{}).Count(&st.Tools).Error; err != nil {
		return st, err
	}
	if err := db.Model(&models.ToolPO{}).Where("is_enabled = ?", true).Count(&st.EnabledTools).Error; err != nil {
		return st, err
	}
	if err := db.Model(&models.ToolPO{}).Where("vectorized = ?", true).Count(&st.Vectorized).Error; err != nil {
		return st, err
	}
	var namespaces []string
	if err := db.Model(&models.MCPServerPO{}).Distinct().Pluck("namespace", &namespaces).Error; err != nil {
		return st, err
	}
	st.Namespaces = int64(len(namespaces))
	return st, nil
}

// —— PO ↔ DO ——

func serverToBiz(po *models.MCPServerPO) *mcp.Server {
	return &mcp.Server{
		ID:                  po.ID,
		Name:                po.Name,
		Namespace:           po.Namespace,
		Transport:           po.Transport,
		Enabled:             po.Enabled,
		Description:         po.Description,
		Endpoint:            po.Endpoint,
		Headers:             decodeMap(po.Headers),
		Command:             po.Command,
		Args:                decodeList(po.Args),
		Env:                 decodeMap(po.Env),
		ProxyConfig:         po.ProxyConfig,
		HealthCheckInterval: po.HealthCheckInterval,
		CreatedAt:           po.CreatedAt,
		UpdatedAt:           po.UpdatedAt,
	}
}

func serverFromBiz(s *mcp.Server) *models.MCPServerPO {
	return &models.MCPServerPO{
		ID:                  s.ID,
		Name:                s.Name,
		Namespace:           s.EffectiveNamespace(),
		Transport:           s.Transport,
		Enabled:             s.Enabled,
		Description:         s.Description,
		Endpoint:            s.Endpoint,
		Headers:             encodeMap(s.Headers),
		Command:             s.Command,
		Args:                encodeList(s.Args),
		Env:                 encodeMap(s.Env),
		ProxyConfig:         s.ProxyConfig,
		HealthCheckInterval: s.HealthCheckInterval,
	}
}

func toolToBiz(po *models.ToolPO, serverName, namespace string) *mcp.Tool {
	return &mcp.Tool{
		ID:           po.ID,
		ServerID:     po.MCPServerID,
		ServerName:   serverName,
		Namespace:    namespace,
		ToolName:     po.ToolName,
		UniqueName:   po.UniqueName,
		DisplayName:  po.DisplayName,
		Description:  po.Description,
		InputSchema:  po.InputSchema,
		OutputSchema: po.OutputSchema,
		Examples:     decodeList(po.Examples),
		Tags:         decodeList(po.Tags),
		Enabled:      po.Enabled,
		Vectorized:   po.Vectorized,
		Version:      po.Version,
		Route:        po.Route,
		CreatedAt:    po.CreatedAt,
		UpdatedAt:    po.UpdatedAt,
	}
}

// encodeList / encodeMap 输出合法 JSON：headers / args / env 是 MySQL 的 JSON 列，
// 空串会被服务端直接拒绝（Error 3140），所以空集合也要写成 [] / {}。
func encodeList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func encodeMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func decodeMap(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func toolFromBiz(serverID uint64, t *mcp.Tool) *models.ToolPO {
	return &models.ToolPO{
		MCPServerID:  serverID,
		ToolName:     clip(strings.TrimSpace(t.ToolName), 128),
		UniqueName:   clip(strings.TrimSpace(t.UniqueName), 256),
		DisplayName:  clip(strings.TrimSpace(t.DisplayName), 191),
		Description:  t.Description,
		InputSchema:  t.InputSchema,
		OutputSchema: t.OutputSchema,
		Examples:     encodeList(t.Examples),
		Tags:         encodeList(t.Tags),
		Enabled:      t.Enabled,
		Vectorized:   t.Vectorized,
		Version:      clip(strings.TrimSpace(t.Version), 64),
		Route:        t.Route,
	}
}

// toolChanged 内容是否变化（启停状态是本地决策，不参与比较）
func toolChanged(old *models.ToolPO, in *models.ToolPO) bool {
	return old.UniqueName != in.UniqueName ||
		old.DisplayName != in.DisplayName ||
		old.Description != in.Description ||
		old.InputSchema != in.InputSchema ||
		old.OutputSchema != in.OutputSchema ||
		old.Examples != in.Examples ||
		old.Tags != in.Tags ||
		old.Version != in.Version ||
		old.Route != in.Route
}
