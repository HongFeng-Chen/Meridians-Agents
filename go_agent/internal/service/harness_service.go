package service

// 评测 Harness 接入面。
//
// 实现 api/harness/v1/harness.proto：用例集 / 用例 CRUD、启动 / 停止 / 查询评测运行
// 与逐例报告。规则全部在 internal/biz/harness：异步执行、规则断言 + LLM Judge 判分。

import (
	"context"
	"time"

	v1 "go_agent/api/harness/v1"
	"go_agent/internal/biz/harness"
)

// HarnessService 评测接入面。
type HarnessService struct {
	v1.UnimplementedHarnessServiceServer

	uc *harness.UseCase
}

var (
	_ v1.HarnessServiceHTTPServer = (*HarnessService)(nil)
	_ v1.HarnessServiceServer     = (*HarnessService)(nil)
)

// NewHarnessService 创建服务
func NewHarnessService(uc *harness.UseCase) *HarnessService {
	return &HarnessService{uc: uc}
}

// ListSets 用例集列表。
func (s *HarnessService) ListSets(ctx context.Context, req *v1.ListSetsRequest) (*v1.ListSetsReply, error) {
	list, err := s.uc.ListSets(ctx, harness.SetFilter{Keyword: req.GetQ(), Limit: int(req.GetLimit())})
	if err != nil {
		return nil, mapHarnessErr(err)
	}
	out := &v1.ListSetsReply{Sets: make([]*v1.EvalSet, 0, len(list))}
	for i := range list {
		out.Sets = append(out.Sets, toEvalSet(&list[i]))
	}
	return out, nil
}

// SaveSet 新建（id=0）或更新用例集。
func (s *HarnessService) SaveSet(ctx context.Context, req *v1.SaveSetRequest) (*v1.EvalSet, error) {
	set, err := s.uc.SaveSet(ctx, harness.SaveSetInput{
		ID:          uint64(req.GetId()),
		Name:        req.GetName(),
		Description: req.GetDescription(),
		Target:      req.GetTarget(),
		Tags:        req.GetTags(),
		Enabled:     req.GetEnabled(),
	})
	if err != nil {
		return nil, mapHarnessErr(err)
	}
	return toEvalSet(set), nil
}

// GetSet 用例集详情（含全部用例）。
func (s *HarnessService) GetSet(ctx context.Context, req *v1.GetSetRequest) (*v1.GetSetReply, error) {
	set, cases, err := s.uc.GetSet(ctx, uint64(req.GetId()))
	if err != nil {
		return nil, mapHarnessErr(err)
	}
	out := &v1.GetSetReply{Set: toEvalSet(set), Cases: make([]*v1.EvalCase, 0, len(cases))}
	for i := range cases {
		out.Cases = append(out.Cases, toEvalCase(&cases[i]))
	}
	return out, nil
}

// DeleteSet 删除用例集。
func (s *HarnessService) DeleteSet(ctx context.Context, req *v1.DeleteSetRequest) (*v1.DeleteReply, error) {
	if err := s.uc.DeleteSet(ctx, uint64(req.GetId())); err != nil {
		return nil, mapHarnessErr(err)
	}
	return &v1.DeleteReply{}, nil
}

// SaveCase 新建（id=0）或更新用例。
func (s *HarnessService) SaveCase(ctx context.Context, req *v1.SaveCaseRequest) (*v1.CaseReply, error) {
	checks := make([]harness.Check, 0, len(req.GetChecks()))
	for _, c := range req.GetChecks() {
		checks = append(checks, harness.Check{Kind: c.GetKind(), Pattern: c.GetPattern(), Weight: c.GetWeight()})
	}
	c, err := s.uc.SaveCase(ctx, harness.SaveCaseInput{
		ID:      uint64(req.GetId()),
		SetID:   uint64(req.GetSetId()),
		Name:    req.GetName(),
		Input:   req.GetInput(),
		Context: req.GetContext(),
		Expect:  req.GetExpect(),
		Checks:  checks,
		Tags:    req.GetTags(),
		Enabled: req.GetEnabled(),
	})
	if err != nil {
		return nil, mapHarnessErr(err)
	}
	return &v1.CaseReply{Case: toEvalCase(c)}, nil
}

// DeleteCase 删除用例。
func (s *HarnessService) DeleteCase(ctx context.Context, req *v1.DeleteCaseRequest) (*v1.DeleteReply, error) {
	if err := s.uc.DeleteCase(ctx, uint64(req.GetId())); err != nil {
		return nil, mapHarnessErr(err)
	}
	return &v1.DeleteReply{}, nil
}

// StartRun 启动评测运行（异步）。
func (s *HarnessService) StartRun(ctx context.Context, req *v1.StartRunRequest) (*v1.StartRunReply, error) {
	run, err := s.uc.StartRun(ctx, uint64(req.GetSetId()), harness.RunConfig{
		Target:            req.GetTarget(),
		JudgeModel:        req.GetJudgeModel(),
		Concurrency:       int(req.GetConcurrency()),
		PerCaseTimeoutSec: int(req.GetPerCaseTimeoutSec()),
		PromptKey:         req.GetPromptKey(),
		PromptVersion:     int(req.GetPromptVersion()),
	})
	if err != nil {
		return nil, mapHarnessErr(err)
	}
	return &v1.StartRunReply{Run: toEvalRun(run)}, nil
}

// StopRun 停止运行。
func (s *HarnessService) StopRun(ctx context.Context, req *v1.StopRunRequest) (*v1.StopRunReply, error) {
	if err := s.uc.StopRun(ctx, uint64(req.GetId())); err != nil {
		return nil, mapHarnessErr(err)
	}
	return &v1.StopRunReply{}, nil
}

// ListRuns 运行列表。
func (s *HarnessService) ListRuns(ctx context.Context, req *v1.ListRunsRequest) (*v1.ListRunsReply, error) {
	list, err := s.uc.ListRuns(ctx, harness.RunFilter{
		SetID:  uint64(req.GetSetId()),
		Status: req.GetStatus(),
		Limit:  int(req.GetLimit()),
	})
	if err != nil {
		return nil, mapHarnessErr(err)
	}
	out := &v1.ListRunsReply{Runs: make([]*v1.EvalRun, 0, len(list))}
	for i := range list {
		out.Runs = append(out.Runs, toEvalRun(&list[i]))
	}
	return out, nil
}

// GetRun 运行报告（运行行 + 逐例结果）。
func (s *HarnessService) GetRun(ctx context.Context, req *v1.GetRunRequest) (*v1.RunReportReply, error) {
	run, items, err := s.uc.GetRunReport(ctx, uint64(req.GetId()))
	if err != nil {
		return nil, mapHarnessErr(err)
	}
	out := &v1.RunReportReply{Run: toEvalRun(run), Items: make([]*v1.RunItem, 0, len(items))}
	for i := range items {
		out.Items = append(out.Items, toRunItem(&items[i]))
	}
	return out, nil
}

// DeleteRun 删除运行。
func (s *HarnessService) DeleteRun(ctx context.Context, req *v1.DeleteRunRequest) (*v1.DeleteReply, error) {
	if err := s.uc.DeleteRun(ctx, uint64(req.GetId())); err != nil {
		return nil, mapHarnessErr(err)
	}
	return &v1.DeleteReply{}, nil
}

// —— proto 转换 ——

func toEvalSet(s *harness.Set) *v1.EvalSet {
	return &v1.EvalSet{
		Id:          int64(s.ID),
		Name:        s.Name,
		Description: s.Description,
		Target:      s.Target,
		Tags:        s.Tags,
		Enabled:     s.Enabled,
		CaseCount:   int32(s.CaseCount),
		CreatedAt:   formatTimeVal(s.CreatedAt),
		UpdatedAt:   formatTimeVal(s.UpdatedAt),
	}
}

func toEvalCase(c *harness.Case) *v1.EvalCase {
	checks := make([]*v1.Check, 0, len(c.Checks))
	for _, chk := range c.Checks {
		checks = append(checks, &v1.Check{Kind: chk.Kind, Pattern: chk.Pattern, Weight: chk.Weight})
	}
	return &v1.EvalCase{
		Id:        int64(c.ID),
		SetId:     int64(c.SetID),
		Name:      c.Name,
		Input:     c.Input,
		Context:   c.Context,
		Expect:    c.Expect,
		Checks:    checks,
		Tags:      c.Tags,
		Enabled:   c.Enabled,
		CreatedAt: formatTimeVal(c.CreatedAt),
		UpdatedAt: formatTimeVal(c.UpdatedAt),
	}
}

func toEvalRun(r *harness.Run) *v1.EvalRun {
	return &v1.EvalRun{
		Id:      int64(r.ID),
		SetId:   int64(r.SetID),
		SetName: r.SetName,
		Target:  r.Target,
		Status:  r.Status,
		Total:   int32(r.Total),
		Done:    int32(r.Done),
		Passed:  int32(r.Passed),
		Failed:  int32(r.Failed),
		Errored: int32(r.Errored),
		Score:   r.Score,
		CostMs:  r.CostMS,
		Config: &v1.RunConfig{
			Target:            r.Config.Target,
			JudgeModel:        r.Config.JudgeModel,
			Concurrency:       int32(r.Config.Concurrency),
			PerCaseTimeoutSec: int32(r.Config.PerCaseTimeoutSec),
		},
		PromptKey:     r.Config.PromptKey,
		PromptVersion: int32(r.Config.PromptVersion),
		Error:         r.Error,
		CreatedAt:     formatTimeVal(r.CreatedAt),
		FinishedAt:    formatTimeVal(r.FinishedAt),
	}
}

func toRunItem(it *harness.RunItem) *v1.RunItem {
	checks := make([]*v1.CheckResult, 0, len(it.Checks))
	for _, c := range it.Checks {
		checks = append(checks, &v1.CheckResult{
			Kind: c.Kind, Pattern: c.Pattern, Passed: c.Passed, Weight: c.Weight, Detail: c.Detail,
		})
	}
	return &v1.RunItem{
		Id:               int64(it.ID),
		RunId:            int64(it.RunID),
		CaseId:           int64(it.CaseID),
		CaseName:         it.CaseName,
		Output:           it.Output,
		Checks:           checks,
		Passed:           it.Passed,
		Score:            it.Score,
		LatencyMs:        it.LatencyMS,
		PromptTokens:     it.PromptTokens,
		CompletionTokens: it.CompletionTokens,
		Error:            it.Error,
		CreatedAt:        formatTimeVal(it.CreatedAt),
	}
}

// formatTimeVal 时间转字符串（零值返回空串，避免 "0001-01-01 00:00:00" 出现在前端）
func formatTimeVal(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(timeLayout)
}
