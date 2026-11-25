package skill

// Skill Runtime：技能执行的唯一入口。
//
// 三种执行模式在这里收敛成同一个 Execute 调用，Agent / API / 异步派发都不需要
// 知道技能部署在哪台机器上：
//
//	local  —— 进程内读 SKILL.md（V1 行为，指令包）
//	remote —— 调度到 Worker 实例，同步等结果
//	async  —— 落执行记录（queued），由派发协程投递，调用方按 execution_id 查询
//
// 这里同时承担三件事，都是「分布式执行」绕不过去的：
//  1. 幂等：同幂等键的重复调用回放上次成功结果（MQ 至少一次投递 / Agent 重试都会重复）；
//  2. 重试与熔断：换实例重试，失败累计到阈值后暂时摘掉坏实例；
//  3. 账本：每次远程/异步执行都落 skill_executions，便于排障与成本核算。
//
// 注意：local 模式不写执行记录——它每次只是读一行文本，已经有 op_histories 记录，
// 再写一份执行账本只会把表撑大而没有新信息。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go_agent/internal/pkg/egress"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/pkg/trace"

	"github.com/go-kratos/kratos/v3/log"
)

const (
	// 执行记录里 input/output 的存储上限：结果可能很大，账本只留可排查的摘要
	maxRecordPayload = 32 << 10
	// 回给 LLM 的文本上限（与 MCP 工具结果一致）
	maxResultText = 6000
)

// Execute 统一执行入口。
func (u *UseCase) Execute(ctx context.Context, req RuntimeRequest) (res *RuntimeResult, err error) {
	name := strings.TrimSpace(req.SkillName)
	if name == "" {
		return nil, ErrInvalidName
	}
	// 一次技能调用在链路里必须是一个节点：否则排障时只能看到「Agent 卡了 3 秒」，
	// 看不出这段时间花在哪个技能、哪个 Worker 上。
	ctx, span := trace.Start(ctx, "skill.execute",
		trace.String(trace.AttrKind, trace.KindTool),
		trace.String(trace.AttrToolName, "skill:"+name))
	defer func() { span.End(err) }()

	sk, err := u.repo.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if !sk.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrSkillDisabled, name)
	}
	req = withIdentity(ctx, req)
	spec := sk.Execution.Normalized()
	span.Set("skill.mode", spec.Mode)

	if spec.Mode == ModeLocal {
		return u.execLocal(sk, req)
	}
	if u.rt == nil || u.worker == nil {
		return nil, fmt.Errorf("%w：技能 %s 配置为 %s，但运行时未接线（skill_instances / Worker 客户端缺失）",
			ErrNotExecutable, name, spec.Mode)
	}
	return u.execRemote(ctx, sk, spec, req, span.TraceID())
}

// execLocal local 模式：把 SKILL.md 作为指令文本返回
//
// 不写执行记录：每次只是读一行文本，op_histories 已有「技能使用」记录，
// 再落一份执行账本只会撑大表而没有新信息。
func (u *UseCase) execLocal(sk *Skill, req RuntimeRequest) (*RuntimeResult, error) {
	return &RuntimeResult{
		SkillName: sk.Name,
		Mode:      ModeLocal,
		Status:    ExecSucceeded,
		Text:      renderSkillText(sk, req.Purpose),
		Output: map[string]any{
			"content":    sk.Content,
			"file_count": sk.FileCount,
			"version":    sk.Version,
		},
	}, nil
}

// renderSkillText 本地技能注入上下文的文本视图
func renderSkillText(sk *Skill, purpose string) string {
	head := fmt.Sprintf("[技能 %s v%s 已加载]", sk.Name, sk.Version)
	if purpose != "" {
		head += "\n使用目的：" + purpose
	}
	body := sk.Content
	if body == "" {
		body = "（该技能没有 SKILL.md 说明文本）"
	}
	return head + "\n\n" + body
}

// execRemote remote / async 模式
func (u *UseCase) execRemote(ctx context.Context, sk *Skill, spec ExecutionSpec, req RuntimeRequest, traceID string) (*RuntimeResult, error) {
	idemKey := strings.TrimSpace(req.IdempotencyKey)
	if idemKey == "" && spec.Idempotent {
		// 默认幂等键：任务 + 技能 + 参数摘要。同一任务同一轮重发同一调用时回放。
		idemKey = idemDigest(req.TaskID, sk.Name, req.Input)
	}
	if spec.Idempotent && idemKey != "" {
		if prev, err := u.rt.FindExecutionByKey(ctx, req.TenantID, sk.Name, idemKey); err == nil && prev != nil {
			out := decodeOutput(prev.Output)
			return &RuntimeResult{
				ExecutionID: prev.ID,
				SkillName:   sk.Name,
				Mode:        prev.Mode,
				Status:      prev.Status,
				Output:      out,
				Text:        renderWorkerText(sk.Name, prev.Status, out, prev.Error, prev.WorkerID, prev.Attempts, true),
				WorkerID:    prev.WorkerID,
				Attempts:    prev.Attempts,
				LatencyMS:   prev.LatencyMS,
				Replayed:    true,
			}, nil
		}
	}

	async := spec.Mode == ModeAsync || req.ForceAsync
	now := time.Now()
	exec := &Execution{
		ID:             newExecutionID(),
		TaskID:         req.TaskID,
		SkillName:      sk.Name,
		SkillVersion:   sk.Version,
		Mode:           spec.Mode,
		Status:         ExecRunning,
		TenantID:       req.TenantID,
		UserID:         req.UserID,
		AgentKey:       req.AgentKey,
		SessionID:      req.SessionID,
		IdempotencyKey: idemKey,
		Input:          encodePayload(req.Input),
		QueuedAt:       &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if async {
		exec.Mode = ModeAsync
		exec.Status = ExecQueued
		exec.QueuedAt = &now
	} else {
		exec.StartedAt = &now
	}
	if traceID != "" {
		exec.TraceID = traceID
	}
	if err := u.rt.CreateExecution(ctx, exec); err != nil {
		return nil, fmt.Errorf("创建执行记录失败: %w", err)
	}

	if async {
		// 入队即返回：Worker 的可用性、排队深度都不该让调用方阻塞
		return &RuntimeResult{
			ExecutionID: exec.ID,
			SkillName:   sk.Name,
			Mode:        ModeAsync,
			Status:      ExecQueued,
			Queued:      true,
			Text: fmt.Sprintf("[技能 %s 已提交异步执行] execution_id=%s\n"+
				"稍后用该 ID 查询结果（GET /api/skills/executions/%s 或能力 skill:status）。",
				sk.Name, exec.ID, exec.ID),
		}, nil
	}

	return u.runRemote(ctx, sk, spec, exec, req)
}

// runRemote 调度 + 调用 + 重试 + 记账（同步执行与异步派发共用）
func (u *UseCase) runRemote(ctx context.Context, sk *Skill, spec ExecutionSpec, exec *Execution, req RuntimeRequest) (*RuntimeResult, error) {
	deadline := time.Duration(spec.TimeoutSec) * time.Second
	if req.Timeout > 0 && req.Timeout < deadline {
		deadline = req.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	attempts := spec.Retry + 1
	if spec.Retry < 0 {
		attempts = 1
	}
	exclude := make(map[string]struct{}, attempts)
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		inst, err := u.pickInstance(ctx, sk.Name, spec, exclude)
		if err != nil {
			lastErr = err
			break
		}
		start := time.Now()
		resp, callErr := u.worker.Execute(ctx, *inst, WorkerRequest{
			TaskID:         exec.TaskID,
			ExecutionID:    exec.ID,
			SkillName:      handlerOf(sk, spec),
			SkillVersion:   sk.Version,
			TenantID:       exec.TenantID,
			UserID:         exec.UserID,
			AgentKey:       exec.AgentKey,
			SessionID:      exec.SessionID,
			IdempotencyKey: exec.IdempotencyKey,
			TimeoutMS:      int(deadline / time.Millisecond),
			TraceID:        exec.TraceID,
			Input:          req.Input,
		})
		latency := time.Since(start).Milliseconds()
		exec.Attempts = attempt

		if callErr == nil && resp != nil && resp.Success {
			exec.Status = ExecSucceeded
			exec.Output = encodePayload(resp.Output)
			exec.WorkerID = instanceName(inst, resp)
			exec.LatencyMS = latency
			u.finishExecution(ctx, exec, inst.ID, true, latency)
			return u.resultFromExec(ctx, sk, exec, false), nil
		}
		// 失败：记录到实例（熔断计数）与本条执行记录
		reason := ""
		if callErr != nil {
			reason = callErr.Error()
		} else if resp != nil && resp.Error != "" {
			reason = resp.Error
		} else {
			reason = "Worker 返回失败"
		}
		lastErr = errors.New(reason)
		exec.Error = truncate(reason, 512)
		exec.WorkerID = inst.ID
		exec.LatencyMS = latency
		u.finishExecution(ctx, exec, inst.ID, false, latency)
		exclude[inst.ID] = struct{}{}
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
	}

	// 全部尝试失败：区分超时 / 无实例 / 业务失败（调用方据此决定是否重试）
	status := ExecFailed
	if errors.Is(lastErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		status = ExecTimeout
	}
	exec.Status = status
	if exec.Error == "" && lastErr != nil {
		exec.Error = truncate(lastErr.Error(), 512)
	}
	now := time.Now()
	exec.FinishedAt = &now
	exec.LeaseUntil = nil
	exec.UpdatedAt = now
	if err := u.rt.UpdateExecution(ctx, exec); err != nil {
		// 状态写不进去就等于「任务永远 running」，必须留痕（别用 _ = 吞掉）
		log.Error("[skill] 写回执行状态失败", "execution", exec.ID, "status", exec.Status, "err", err)
	}

	// 结果照常返回：让 Agent 看到「哪个技能失败了、为什么」，而不是整轮崩掉
	res := u.resultFromExec(ctx, sk, exec, false)
	if lastErr != nil {
		res.Text = renderWorkerText(sk.Name, status, nil, exec.Error, exec.WorkerID, exec.Attempts, false)
	}
	return res, nil
}

// pickInstance 取实例并调度；没有注册实例时回退到技能配置的直连地址
func (u *UseCase) pickInstance(ctx context.Context, skillName string, spec ExecutionSpec, exclude map[string]struct{}) (*Instance, error) {
	instances, err := u.rt.ListInstances(ctx, skillName)
	if err != nil {
		return nil, fmt.Errorf("读取技能实例失败: %w", err)
	}
	static := staticInstance(skillName, spec)
	if len(instances) == 0 {
		if static != nil {
			// 单实例直连部署：Worker 没注册也能跑（endpoint 写在技能配置里）
			return static, nil
		}
		return nil, ErrNoWorker
	}
	if u.sched == nil {
		return &instances[0], nil
	}
	inst, err := u.sched.Pick(instances, exclude)
	if err == nil {
		return inst, nil
	}
	// 一个健康实例都没有时，退到配置里的直连地址。
	// 这条兜底不能只看「实例表为空」：Worker 换过机器后旧实例会一直躺在表里
	// （心跳过期 → offline），否则配了 endpoint 的技能会因为一条死记录永久不可用。
	if static != nil && errors.Is(err, ErrNoWorker) {
		return static, nil
	}
	return inst, err
}

// staticInstance 把技能配置里的 endpoint 包成一个虚拟实例（未配置则返回 nil）。
func staticInstance(skillName string, spec ExecutionSpec) *Instance {
	if spec.Endpoint == "" {
		return nil
	}
	return &Instance{
		ID:             "static:" + skillName,
		SkillName:      skillName,
		Endpoint:       spec.Endpoint,
		MaxConcurrency: spec.Concurrency,
		Status:         InstanceOnline,
		LastHeartbeat:  time.Now(),
	}
}

// finishExecution 写回执行结果 + 更新实例熔断计数。
//
// instID 必须传「调度用的实例 ID」而不是 exec.WorkerID：后者可能是
// "实例ID/Worker自报ID" 的展示形式，拿它去更新指标会一行都命中不了
// （表现就是「调用成功但成功率永远是 0」）。
func (u *UseCase) finishExecution(ctx context.Context, exec *Execution, instID string, ok bool, latencyMS int64) {
	// 用 WithoutCancel：调用方超时/取消不该让「失败原因」丢在内存里
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if ok {
		now := time.Now()
		exec.FinishedAt = &now
		exec.LeaseUntil = nil
		exec.UpdatedAt = now
	}
	if err := u.rt.UpdateExecution(writeCtx, exec); err != nil {
		log.Error("[skill] 写回执行状态失败", "execution", exec.ID, "status", exec.Status, "err", err)
	}
	if instID != "" {
		// 实例健康度写失败只影响调度精度，不影响本次结果，warn 即可
		if err := u.rt.RecordResult(writeCtx, exec.SkillName, instID, ok, latencyMS, exec.Error); err != nil {
			log.Warn("[skill] 更新实例指标失败", "instance", instID, "err", err)
		}
	}
}

// resultFromExec 由执行记录构造返回
func (u *UseCase) resultFromExec(_ context.Context, sk *Skill, exec *Execution, replayed bool) *RuntimeResult {
	out := decodeOutput(exec.Output)
	return &RuntimeResult{
		ExecutionID: exec.ID,
		SkillName:   sk.Name,
		Mode:        exec.Mode,
		Status:      exec.Status,
		Output:      out,
		Text:        renderWorkerText(sk.Name, exec.Status, out, exec.Error, exec.WorkerID, exec.Attempts, replayed),
		WorkerID:    exec.WorkerID,
		Attempts:    exec.Attempts,
		LatencyMS:   exec.LatencyMS,
		Replayed:    replayed,
	}
}

// GetExecution 查询执行记录（异步模式轮询用；同步模式排障用）
func (u *UseCase) GetExecution(ctx context.Context, id string) (*Execution, error) {
	if u.rt == nil {
		return nil, fmt.Errorf("技能运行时未接线")
	}
	return u.rt.GetExecution(ctx, strings.TrimSpace(id))
}

// ListExecutions 执行记录列表（租户隔离：非管理员只看自己的）
func (u *UseCase) ListExecutions(ctx context.Context, f ExecutionFilter) ([]Execution, error) {
	if u.rt == nil {
		return nil, fmt.Errorf("技能运行时未接线")
	}
	return u.rt.ListExecutions(ctx, f)
}

// CancelExecution 取消排队中的执行（running 不强行中断远端调用，只标记不再派发）
func (u *UseCase) CancelExecution(ctx context.Context, id string) (*Execution, error) {
	exec, err := u.GetExecution(ctx, id)
	if err != nil {
		return nil, err
	}
	if exec.Done() {
		return exec, nil
	}
	if exec.Status != ExecQueued {
		// 已在执行：交给 Worker 侧超时/幂等处理，这里不假装取消成功
		return exec, fmt.Errorf("执行已开始（%s），无法取消；请等待超时或结果", exec.Status)
	}
	now := time.Now()
	exec.Status = ExecCanceled
	exec.FinishedAt = &now
	exec.UpdatedAt = now
	if err := u.rt.UpdateExecution(ctx, exec); err != nil {
		return nil, err
	}
	return exec, nil
}

// —— Worker 注册中心 ——

// RegisterInstance Worker 注册（同 ID 重复注册 = 更新地址与容量）
func (u *UseCase) RegisterInstance(ctx context.Context, in RegisterInstanceInput) (*Instance, error) {
	name := strings.TrimSpace(in.SkillName)
	if !validName(name) {
		return nil, ErrInvalidName
	}
	endpoint := strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("%w: endpoint 不能为空", ErrInvalidURL)
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	if u.rt == nil {
		return nil, fmt.Errorf("技能运行时未接线")
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = endpoint
	}
	maxConc := in.MaxConcurrency
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrency
	}
	weight := in.Weight
	if weight <= 0 {
		weight = 1
	}
	return u.rt.SaveInstance(ctx, Instance{
		ID:             id,
		SkillName:      name,
		Endpoint:       endpoint,
		Version:        strings.TrimSpace(in.Version),
		Region:         strings.TrimSpace(in.Region),
		Labels:         cleanTags(in.Labels),
		Weight:         weight,
		MaxConcurrency: maxConc,
		Status:         InstanceOnline,
		LastHeartbeat:  time.Now(),
	})
}

// Heartbeat 实例心跳（在途数与自报延迟）
func (u *UseCase) Heartbeat(ctx context.Context, hb InstanceHeartbeat) error {
	if u.rt == nil {
		return fmt.Errorf("技能运行时未接线")
	}
	return u.rt.Heartbeat(ctx, hb)
}

// ListInstances 实例列表（附带在线判定，前端可直接展示）
func (u *UseCase) ListInstances(ctx context.Context, skillName string) ([]Instance, error) {
	if u.rt == nil {
		return nil, fmt.Errorf("技能运行时未接线")
	}
	list, err := u.rt.ListInstances(ctx, strings.TrimSpace(skillName))
	if err != nil {
		return nil, err
	}
	if u.sched != nil {
		now := time.Now()
		for i := range list {
			if !u.sched.Online(list[i], now) && list[i].Status != InstanceOffline {
				list[i].Status = InstanceOffline
			}
		}
	}
	return list, nil
}

// DeleteInstance 摘除实例（下线 Worker）
func (u *UseCase) DeleteInstance(ctx context.Context, skillName, id string) error {
	if u.rt == nil {
		return fmt.Errorf("技能运行时未接线")
	}
	return u.rt.DeleteInstance(ctx, strings.TrimSpace(skillName), strings.TrimSpace(id))
}

// SetRuntime 注入运行时依赖（数据层构造；未注入时只有 local 模式可用）
func (u *UseCase) SetRuntime(rt RuntimeStore, worker WorkerClient) {
	u.rt = rt
	u.worker = worker
}

// —— 工具 ——

// withIdentity 用请求身份兜底：调用方（Agent / API）没传就从 context 取
func withIdentity(ctx context.Context, req RuntimeRequest) RuntimeRequest {
	if req.TenantID == "" || req.UserID == "" {
		info := identity.From(ctx)
		if req.TenantID == "" {
			req.TenantID = info.TenantID
		}
		if req.UserID == "" {
			req.UserID = info.UserID
		}
	}
	return req
}

// handlerOf Worker 侧实现名：技能没配 handler 时用技能名兜底
func handlerOf(sk *Skill, spec ExecutionSpec) string {
	if h := strings.TrimSpace(spec.Handler); h != "" {
		return h
	}
	return sk.Name
}

func instanceName(inst *Instance, resp *WorkerResponse) string {
	if resp != nil && resp.WorkerID != "" && resp.WorkerID != inst.ID {
		return inst.ID + "/" + resp.WorkerID
	}
	return inst.ID
}

// newExecutionID 生成执行 ID（时间前缀 + 随机后缀，便于按时间排序与定位）
func newExecutionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "exec_" + hex.EncodeToString(b[:])
}

// idemDigest 默认幂等键：任务 + 技能 + 参数摘要
func idemDigest(taskID, skillName string, input map[string]any) string {
	raw, _ := json.Marshal(input)
	sum := sha256Hex(string(raw))
	if taskID == "" {
		return skillName + ":" + sum[:16]
	}
	return fmt.Sprintf("%s:%s:%s", taskID, skillName, sum[:16])
}

func encodePayload(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return truncate(string(raw), maxRecordPayload)
}

func decodeOutput(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]any{"raw": raw}
	}
	return out
}

// renderWorkerText 远程结果 → LLM 可读文本（状态 + 结果 JSON + 落点信息）
func renderWorkerText(skillName, status string, output map[string]any, errMsg, workerID string, attempts int, replayed bool) string {
	var b strings.Builder
	switch status {
	case ExecSucceeded:
		fmt.Fprintf(&b, "[技能 %s 执行成功]", skillName)
	case ExecQueued:
		fmt.Fprintf(&b, "[技能 %s 已入队]", skillName)
	case ExecTimeout:
		fmt.Fprintf(&b, "[技能 %s 执行超时]", skillName)
	default:
		fmt.Fprintf(&b, "[技能 %s 执行失败]", skillName)
	}
	if workerID != "" {
		fmt.Fprintf(&b, " worker=%s", workerID)
	}
	if attempts > 1 {
		fmt.Fprintf(&b, " 尝试=%d", attempts)
	}
	if replayed {
		b.WriteString("（幂等回放，未重复执行）")
	}
	b.WriteString("\n")
	if len(output) > 0 {
		raw, _ := json.Marshal(output)
		b.WriteString(truncate(string(raw), maxResultText))
		b.WriteString("\n")
	}
	if errMsg != "" {
		b.WriteString("原因：" + truncate(errMsg, 600))
	}
	return strings.TrimSpace(b.String())
}

// truncate 按字符截断（避免切坏 UTF-8）
func truncate(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n]) + "…"
}

func validateEndpoint(endpoint string) error {
	if err := egress.ValidateURL(endpoint); err != nil {
		return fmt.Errorf("%w: endpoint %q（%v）", ErrInvalidURL, endpoint, err)
	}
	return nil
}

// sha256Hex 参数摘要（幂等键用）
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
