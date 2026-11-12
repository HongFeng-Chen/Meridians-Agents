// media_service.go 媒体生成（文生图 / 图生图 / 视频生成）管理面。
//
// 挂在 LLMService 上（同一个数据源管理面），实现 llm.proto 的媒体四个 RPC。
// 记录查询复用调用历史的归属收窄约定：普通用户只看本租户自己的记录，
// 管理员可看全平台（auditTenant / auditScope）。
package service

import (
	"context"

	v1 "go_agent/api/llm/v1"
	"go_agent/internal/biz/llm"
)

// GenerateImages 生成图片（文生图 / 图生图，同步返回）。
func (s *LLMService) GenerateImages(ctx context.Context, req *v1.GenerateImagesRequest) (*v1.GenerateImagesReply, error) {
	in := &llm.ImageGenInput{
		Provider: req.GetProvider(),
		Model:    req.GetModel(),
		Mode:     req.GetMode(),
		Prompt:   req.GetPrompt(),
		Images:   req.GetImages(),
		Size:     req.GetSize(),
		Count:    int(req.GetCount()),
	}
	if req.Seed != nil {
		seed := req.GetSeed()
		in.Seed = &seed
	}
	rec, err := s.media.GenerateImage(ctx, in)
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.GenerateImagesReply{Record: toImageRecord(rec)}, nil
}

// SubmitVideo 提交视频生成任务。
func (s *LLMService) SubmitVideo(ctx context.Context, req *v1.SubmitVideoRequest) (*v1.SubmitVideoReply, error) {
	in := &llm.VideoGenInput{
		Provider:    req.GetProvider(),
		Model:       req.GetModel(),
		Prompt:      req.GetPrompt(),
		Image:       req.GetImage(),
		Size:        req.GetSize(),
		DurationSec: int(req.GetDurationSeconds()),
	}
	if req.Seed != nil {
		seed := req.GetSeed()
		in.Seed = &seed
	}
	rec, err := s.media.SubmitVideo(ctx, in)
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.SubmitVideoReply{Record: toVideoRecord(rec)}, nil
}

// GetVideo 查询视频任务（进行中的任务顺带轮询厂商一次）。
func (s *LLMService) GetVideo(ctx context.Context, req *v1.GetVideoRequest) (*v1.GetVideoReply, error) {
	rec, err := s.media.GetVideo(ctx, req.GetId())
	if err != nil {
		return nil, mapLLMErr(err)
	}
	return &v1.GetVideoReply{Record: toVideoRecord(rec)}, nil
}

// ListMediaRecords 生成历史（kind=image | video）。
func (s *LLMService) ListMediaRecords(ctx context.Context, req *v1.ListMediaRecordsRequest) (*v1.ListMediaRecordsReply, error) {
	f := llm.MediaFilter{
		TenantID: auditTenant(ctx), // 普通用户只看本租户；管理员可看全平台
		UserID:   auditScope(ctx),  // 再收窄到本人记录
		Provider: req.GetProvider(),
		Model:    req.GetModel(),
		Limit:    int(req.GetLimit()),
		Offset:   int(req.GetOffset()),
	}
	switch req.GetResult() {
	case "ok":
		t := true
		f.OK = &t
	case "failed":
		t := false
		f.OK = &t
	}

	out := &v1.ListMediaRecordsReply{}
	switch req.GetKind() {
	case "image":
		list, total, err := s.media.ListImageGenRecords(ctx, f)
		if err != nil {
			return nil, mapLLMErr(err)
		}
		out.Total = total
		out.Images = make([]*v1.ImageGenRecord, 0, len(list))
		for _, rec := range list {
			out.Images = append(out.Images, toImageRecord(rec))
		}
	case "video":
		list, total, err := s.media.ListVideoGenRecords(ctx, f)
		if err != nil {
			return nil, mapLLMErr(err)
		}
		out.Total = total
		out.Videos = make([]*v1.VideoGenRecord, 0, len(list))
		for _, rec := range list {
			out.Videos = append(out.Videos, toVideoRecord(rec))
		}
	default:
		return nil, badRequest("kind 需为 image 或 video")
	}
	return out, nil
}

// ---------- 领域模型 → 协议 ----------

func toImageRecord(rec *llm.ImageGenRecord) *v1.ImageGenRecord {
	if rec == nil {
		return nil
	}
	return &v1.ImageGenRecord{
		Id:         rec.ID,
		Provider:   rec.Provider,
		Model:      rec.Model,
		Mode:       rec.Mode,
		Prompt:     rec.Prompt,
		Size:       rec.Size,
		Count:      int32(rec.Count),
		Inputs:     rec.Inputs,
		Outputs:    rec.Outputs,
		Ok:         rec.OK,
		Error:      rec.Error,
		DurationMs: rec.DurationMS,
		CreatedAt:  rec.CreatedAt.Format(timeLayout),
	}
}

func toVideoRecord(rec *llm.VideoGenRecord) *v1.VideoGenRecord {
	if rec == nil {
		return nil
	}
	finishedAt := ""
	if !rec.FinishedAt.IsZero() {
		finishedAt = rec.FinishedAt.Format(timeLayout)
	}
	return &v1.VideoGenRecord{
		Id:              rec.ID,
		Provider:        rec.Provider,
		Model:           rec.Model,
		Prompt:          rec.Prompt,
		Size:            rec.Size,
		DurationSeconds: int32(rec.DurationSec),
		Inputs:          rec.Inputs,
		Status:          rec.Status,
		Output:          rec.Output,
		Error:           rec.Error,
		DurationMs:      rec.DurationMS,
		SubmittedAt:     rec.SubmittedAt.Format(timeLayout),
		FinishedAt:      finishedAt,
		CreatedAt:       rec.CreatedAt.Format(timeLayout),
	}
}
