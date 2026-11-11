package agentfactory

// Agent 配置不可变快照。
//
// 问题：任务一旦提交，执行时读的是「当前」Agent 定义。定义改一次提示词、
// 换一个模型、收紧一个能力，所有在跑的任务立刻跟着变；更糟的是审计链断掉——
// 事后想知道「这条任务当时到底用的是什么提示词、什么能力边界」，已经查不到。
//
// 方案：每次定义变更（创建 / 更新）生成一条不可变快照，任务提交时钉住
// snapshot_id；执行时按快照加载配置，权限则按**当前**授权重新校验
// （见 authz.go：快照冻结的是配置，不是权限）。
//
// 一致性取舍：
//   - 快照按 checksum 去重：只改 Enabled / 调度时间这类运行态字段不会产生新版本
//     （否则「启用/停用」会刷出一堆内容相同的快照）；
//   - 快照只增不改：历史快照不允许更新/删除，回滚是「用旧快照再发一次」；
//   - 快照缺失或解析失败时回退当前定义：快照是加固项，不能因为一条脏数据
//     让任务无法执行（会 warn + 落事件，可观测）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Snapshot Agent 定义在某一个版本上的冻结配置
type Snapshot struct {
	ID         uint64 `json:"id"`
	AgentDefID uint64 `json:"agent_def_id"`
	AgentKey   string `json:"agent_key"`
	// Version 对应的定义版本号（AgentDef.Version）
	Version int `json:"version"`
	// Checksum 配置摘要：内容相同则相同，与 Enabled / 调度时间无关
	Checksum string `json:"checksum"`
	// Config 冻结的完整 AgentDef（JSON）
	Config string `json:"config,omitempty"`
	// Deps 依赖清单（JSON SnapshotDeps）：委派目标 / 工具 / 技能
	Deps      string    `json:"deps,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SnapshotDeps 快照的外部依赖：这些资源变更不会改写历史快照，
// 但排查「当时这个 Agent 还能调什么」时要能一眼看到。
type SnapshotDeps struct {
	Agents []string `json:"agents,omitempty"`
	Tools  []string `json:"tools,omitempty"`
	Skills []string `json:"skills,omitempty"`
}

// BuildSnapshot 由当前定义生成快照（不落库）
func BuildSnapshot(def *AgentDef) (*Snapshot, error) {
	if def == nil || def.ID == 0 {
		return nil, fmt.Errorf("%w: 生成快照需要已持久化的定义", ErrInvalidDef)
	}
	cfg, err := json.Marshal(def)
	if err != nil {
		return nil, fmt.Errorf("序列化 Agent 配置失败: %w", err)
	}
	deps, err := json.Marshal(ExtractDeps(def))
	if err != nil {
		return nil, fmt.Errorf("序列化依赖清单失败: %w", err)
	}
	return &Snapshot{
		AgentDefID: def.ID,
		AgentKey:   def.AgentKey,
		Version:    def.Version,
		Checksum:   ConfigChecksum(def),
		Config:     string(cfg),
		Deps:       string(deps),
		CreatedAt:  time.Now(),
	}, nil
}

// AgentDef 反解出冻结的配置
func (s *Snapshot) AgentDef() (*AgentDef, error) {
	if s == nil || strings.TrimSpace(s.Config) == "" {
		return nil, fmt.Errorf("%w: 快照缺少配置", ErrInvalidDef)
	}
	var def AgentDef
	if err := json.Unmarshal([]byte(s.Config), &def); err != nil {
		return nil, fmt.Errorf("解析快照配置失败: %w", err)
	}
	return &def, nil
}

// DepsList 反解依赖清单（解析失败返回空清单，不影响执行）
func (s *Snapshot) DepsList() SnapshotDeps {
	var d SnapshotDeps
	if s != nil && s.Deps != "" {
		_ = json.Unmarshal([]byte(s.Deps), &d)
	}
	return d
}

// ConfigChecksum 配置摘要：先抹掉运行态字段再算 sha256。
//
// 抹掉的字段：Version（内容没变就不该因版本号递增而变）、Enabled（启停是
// 运行态开关）、Vectorized / 时间戳（每次保存都会刷新）。
func ConfigChecksum(def *AgentDef) string {
	if def == nil {
		return ""
	}
	clone := *def
	clone.Version = 0
	clone.Enabled = false
	clone.Vectorized = false
	clone.NextRunAt = time.Time{}
	clone.LastRunAt = time.Time{}
	clone.CreatedAt = time.Time{}
	clone.UpdatedAt = time.Time{}
	// encoding/json 对 struct 按字段顺序、对 map 按 key 排序输出，同一配置天然稳定
	raw, err := json.Marshal(clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ExtractDeps 从定义里抽取依赖：委派目标（members / 监察 / 风控）、工具、技能。
//
// 只做「静态字符串」层面的抽取：能力白名单是唯一声明处，动态创建的 Agent
// （autocompose）也会把能力写回定义，所以这里能覆盖到。
func ExtractDeps(def *AgentDef) SnapshotDeps {
	if def == nil {
		return SnapshotDeps{}
	}
	agentSet := map[string]struct{}{}
	addAgent := func(k string) {
		k = strings.TrimSpace(k)
		if k != "" {
			agentSet[k] = struct{}{}
		}
	}
	for _, m := range def.Members {
		addAgent(m)
	}
	addAgent(def.SupervisorKey)
	addAgent(def.RiskAgentKey)

	var tools, skills []string
	for _, c := range def.Capabilities {
		switch {
		case strings.HasPrefix(c, CapToolPrefix):
			tools = append(tools, strings.TrimPrefix(c, CapToolPrefix))
		case strings.HasPrefix(c, CapSkillPrefix):
			skills = append(skills, strings.TrimPrefix(c, CapSkillPrefix))
		}
	}
	agents := make([]string, 0, len(agentSet))
	for k := range agentSet {
		agents = append(agents, k)
	}
	sort.Strings(agents)
	sort.Strings(tools)
	sort.Strings(skills)
	return SnapshotDeps{Agents: agents, Tools: tools, Skills: skills}
}

// —— 用例：发布与读取 ——

// PublishSnapshot 为指定 Agent 生成快照（内容未变则复用最新一条）。
// 供管理接口显式发布；创建 / 更新定义时也会自动调用。
func (u *UseCase) PublishSnapshot(ctx context.Context, id uint64) (*Snapshot, error) {
	def, err := u.repo.GetDef(ctx, id)
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, ErrDefNotFound
	}
	return u.publishSnapshot(ctx, def)
}

// publishSnapshot 内容去重的落库：checksum 与最新一条相同就复用。
func (u *UseCase) publishSnapshot(ctx context.Context, def *AgentDef) (*Snapshot, error) {
	snap, err := BuildSnapshot(def)
	if err != nil {
		return nil, err
	}
	latest, err := u.repo.LatestSnapshot(ctx, def.ID)
	if err != nil {
		u.warn("读取最新快照失败", "agent", def.AgentKey, "err", err)
	} else if latest != nil && latest.Checksum == snap.Checksum {
		return latest, nil
	}
	if err := u.repo.CreateSnapshot(ctx, snap); err != nil {
		return nil, fmt.Errorf("保存 Agent 快照失败: %w", err)
	}
	return snap, nil
}

// Snapshots 快照列表（按版本倒序）
func (u *UseCase) Snapshots(ctx context.Context, agentDefID uint64, limit int) ([]Snapshot, error) {
	if agentDefID == 0 {
		return nil, fmt.Errorf("%w: agent_id 必填", ErrInvalidDef)
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return u.repo.ListSnapshots(ctx, agentDefID, limit)
}

// latestSnapshotID 任务提交时钉住的快照（没有快照 = 0，执行时回退当前定义）
func (u *UseCase) latestSnapshotID(ctx context.Context, agentDefID uint64) uint64 {
	id, _ := u.LatestSnapshotInfo(ctx, agentDefID)
	return id
}

// LatestSnapshotForKey 按 Agent key 查最新快照（评测版本绑定用）。
//
// Runner 拿到的只有 target 里的 key，没有 agent_def_id，所以这里先解析定义。
// 任何一步失败都返回 (0,0)：版本信息缺失不影响评测开跑。
func (u *UseCase) LatestSnapshotForKey(ctx context.Context, agentKey string) (uint64, int) {
	if strings.TrimSpace(agentKey) == "" {
		return 0, 0
	}
	def, err := u.loadDefByKey(ctx, agentKey)
	if err != nil || def == nil {
		return 0, 0
	}
	return u.LatestSnapshotInfo(ctx, def.ID)
}

// LatestSnapshotInfo 最新快照的 ID 与版本号。
//
// 与 latestSnapshotID 分开返回版本号：评测报告要写「测的是 v3 的快照」，
// 只有 ID 的话，人去查库才知道那是什么。查不到返回 (0,0) 不报错——
// 「还没发布过快照」是正常状态，不是异常。
func (u *UseCase) LatestSnapshotInfo(ctx context.Context, agentDefID uint64) (uint64, int) {
	if agentDefID == 0 || u.repo == nil {
		return 0, 0
	}
	latest, err := u.repo.LatestSnapshot(ctx, agentDefID)
	if err != nil || latest == nil {
		return 0, 0
	}
	return latest.ID, latest.Version
}

// defForTask 按任务钉住的快照加载配置，回退当前定义。
//
// 保留的字段：ID（回写与日志按当前主键）、Enabled（停用仍阻断新执行）。
// 其余一律用快照里的冻结值——这正是「历史任务不被定义变更影响」的实现点。
func (u *UseCase) defForTask(ctx context.Context, t *Task, live *AgentDef) (*AgentDef, *Snapshot) {
	if t == nil || t.SnapshotID == 0 || live == nil {
		return live, nil
	}
	snap, err := u.repo.GetSnapshot(ctx, t.SnapshotID)
	if err != nil || snap == nil {
		u.warn("任务快照读取失败，回退当前定义", "task", t.ID, "snapshot", t.SnapshotID, "err", err)
		return live, nil
	}
	frozen, err := snap.AgentDef()
	if err != nil {
		u.warn("任务快照解析失败，回退当前定义", "task", t.ID, "snapshot", t.SnapshotID, "err", err)
		return live, nil
	}
	frozen.ID = live.ID
	frozen.Enabled = live.Enabled
	// 版本号保留快照的：trace 里要能看出「这条任务跑的是哪一版配置」
	frozen.Version = snap.Version
	return frozen, snap
}
