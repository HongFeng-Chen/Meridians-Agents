// media.go 媒体生成用例：文生图 / 图生图 / 视频生成的编排层。
//
// 职责：数据源与模型路由（复用数据源配置）、请求校验、记录生命周期管理
// （图片同步终态；视频提交 + 惰性轮询 + 后台轮询器）。厂商协议细节在
// internal/data/image_generations 与 internal/data/video_generations 里。
package llm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go_agent/internal/pkg/identity"

	"github.com/go-kratos/kratos/v3/log"
)

// MediaManager 媒体生成管理器
type MediaManager struct {
	mgr       *Manager // 数据源解析与路由
	imgRepo   ImageGenRepo
	vidRepo   VideoGenRepo
	imgClient ImageGenClient
	vidClient VideoGenClient

	// pollInterval 后台轮询周期；stop 停止信号
	pollInterval time.Duration
	stop         chan struct{}
}

// NewMediaManager 构建媒体生成管理器。
// imgRepo / vidRepo / imgClient / vidClient 任一为 nil 时对应能力降级为报错
// （最小部署可以不带媒体生成）。
func NewMediaManager(mgr *Manager, imgRepo ImageGenRepo, vidRepo VideoGenRepo, imgClient ImageGenClient, vidClient VideoGenClient) *MediaManager {
	return &MediaManager{
		mgr:          mgr,
		imgRepo:      imgRepo,
		vidRepo:      vidRepo,
		imgClient:    imgClient,
		vidClient:    vidClient,
		pollInterval: 5 * time.Second,
		stop:         make(chan struct{}),
	}
}

// StartPolling 启动后台轮询器：把 queued/running 的视频任务推进到终态。
// 没有视频仓库或客户端时是空操作；用 AfterStop 停止。
func (mm *MediaManager) StartPolling() {
	if mm == nil || mm.vidRepo == nil || mm.vidClient == nil {
		return
	}
	go mm.pollLoop()
	log.Info("[media] 视频任务轮询器已启动", "interval", mm.pollInterval.String())
}

// StopPolling 停止后台轮询器
func (mm *MediaManager) StopPolling() {
	if mm == nil {
		return
	}
	select {
	case <-mm.stop:
	default:
		close(mm.stop)
	}
}

// pollActiveSec 只轮询最近 N 小时提交的任务：厂商任务 ID 有时效，
// 更老的任务等用户查询时惰性收尾即可。
const pollActiveSec = 24 * time.Hour

func (mm *MediaManager) pollLoop() {
	t := time.NewTicker(mm.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-mm.stop:
			return
		case <-t.C:
			mm.pollOnce(context.Background())
		}
	}
}

// pollOnce 一轮轮询：所有进行中的任务各查一次，失败不中断其它任务
func (mm *MediaManager) pollOnce(ctx context.Context) {
	active, err := mm.vidRepo.ListActive(ctx,
		[]string{MediaQueued, MediaRunning},
		time.Now().Add(-pollActiveSec), 20)
	if err != nil {
		log.Warn("[media] 拉取进行中的视频任务失败", "err", err)
		return
	}
	for _, rec := range active {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, _ = mm.refreshVideo(c, rec)
		cancel()
	}
}

// resolveImage 解析图片生成的数据源与模型：
// 显式 provider 优先；否则按优先级找第一个配置了图像模型的启用数据源。
func (mm *MediaManager) resolveImage(ctx context.Context, provider, model string) (*Provider, MediaTarget, error) {
	if err := mm.checkMediaRepo(mm.imgRepo, mm.imgClient); err != nil {
		return nil, MediaTarget{}, err
	}
	p, err := mm.pickProvider(ctx, provider, func(p *Provider) bool { return len(p.ImageModels) > 0 })
	if err != nil {
		return nil, MediaTarget{}, err
	}
	if model == "" {
		model = p.ImageModels[0]
	} else if !containsStr(p.ImageModels, model) {
		return nil, MediaTarget{}, fmt.Errorf("%w: 数据源 %s 未配置图像模型 %s（可在数据源里添加后重试）", ErrMediaNoModel, p.Name, model)
	}
	return p, mm.targetFor(p, model), nil
}

// resolveVideo 解析视频生成的数据源与模型（规则同 resolveImage）
func (mm *MediaManager) resolveVideo(ctx context.Context, provider, model string) (*Provider, MediaTarget, error) {
	if err := mm.checkMediaRepo(mm.vidRepo, mm.vidClient); err != nil {
		return nil, MediaTarget{}, err
	}
	p, err := mm.pickProvider(ctx, provider, func(p *Provider) bool { return len(p.VideoModels) > 0 })
	if err != nil {
		return nil, MediaTarget{}, err
	}
	if model == "" {
		model = p.VideoModels[0]
	} else if !containsStr(p.VideoModels, model) {
		return nil, MediaTarget{}, fmt.Errorf("%w: 数据源 %s 未配置视频模型 %s（可在数据源里添加后重试）", ErrMediaNoModel, p.Name, model)
	}
	return p, mm.targetFor(p, model), nil
}

// checkMediaRepo 最小部署兜底：仓库 / 客户端没装配时给出明确报错
func (mm *MediaManager) checkMediaRepo(repo any, client any) error {
	if repo == nil || client == nil {
		return fmt.Errorf("媒体生成能力未装配（image/video_generations 仓库或客户端为空）")
	}
	return nil
}

// pickProvider 数据源路由：显式指定 → 校验存在与启用；未指定 → 第一个满足条件的启用源
func (mm *MediaManager) pickProvider(ctx context.Context, name string, match func(*Provider) bool) (*Provider, error) {
	if strings.TrimSpace(name) != "" {
		p, err := mm.mgr.GetProvider(ctx, strings.TrimSpace(name))
		if err != nil {
			return nil, err
		}
		if !p.Enabled {
			return nil, fmt.Errorf("%w: 数据源 %s 已停用", ErrMediaNoProvider, p.Name)
		}
		if !match(p) {
			return nil, fmt.Errorf("%w: 数据源 %s 没有配置对应类型的生成模型", ErrMediaNoModel, p.Name)
		}
		return p, nil
	}
	providers, err := mm.mgr.ListProviders(ctx, false)
	if err != nil {
		return nil, err
	}
	for i := range providers {
		p := &providers[i]
		if match(p) {
			return p, nil
		}
	}
	return nil, ErrMediaNoProvider
}

// targetFor 数据源 → 厂商调用目标（媒体请求耗时按数据源超时配置放大：
// 生成类接口普遍在 10-60s 量级，对话默认 120s 不够时以配置为准）
func (mm *MediaManager) targetFor(p *Provider, model string) MediaTarget {
	timeout := time.Duration(p.TimeoutSec) * time.Second
	if timeout < 300*time.Second {
		timeout = 300 * time.Second
	}
	return MediaTarget{
		Kind:    p.Kind,
		BaseURL: p.BaseURL,
		APIKey:  p.APIKey,
		Model:   model,
		Timeout: timeout,
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// —— 图片生成 ——

// GenerateImage 同步生成图片：记录先落库（可追溯失败），厂商返回即终态。
func (mm *MediaManager) GenerateImage(ctx context.Context, in *ImageGenInput) (*ImageGenRecord, error) {
	in, err := normalizeImageInput(in)
	if err != nil {
		return nil, err
	}
	p, target, err := mm.resolveImage(ctx, in.Provider, in.Model)
	if err != nil {
		return nil, err
	}
	info := identity.From(ctx)
	rec := &ImageGenRecord{
		TenantID: info.TenantID,
		UserID:   info.UserID,
		Provider: p.Name,
		Model:    target.Model,
		Mode:     in.Mode,
		Prompt:   in.Prompt,
		Size:     in.Size,
		Count:    in.Count,
		Inputs:   redactInputs(in.Images),
	}
	if err := mm.imgRepo.Insert(ctx, rec); err != nil {
		return nil, fmt.Errorf("写入图片生成记录: %w", err)
	}

	start := time.Now()
	outputs, gerr := mm.imgClient.Generate(ctx, target, in)
	rec.Outputs = outputs
	rec.DurationMS = time.Since(start).Milliseconds()
	if gerr != nil {
		rec.Error = truncateErr(gerr)
	} else {
		rec.OK = true
	}
	if uerr := mm.imgRepo.Update(ctx, rec); uerr != nil {
		log.Warn("[media] 图片生成记录更新失败", "id", rec.ID, "err", uerr)
	}
	if gerr != nil {
		return rec, gerr
	}
	return rec, nil
}

// ListImageGenRecords 图片生成历史（userId 空 = 全平台，服务层控制）
func (mm *MediaManager) ListImageGenRecords(ctx context.Context, f MediaFilter) ([]*ImageGenRecord, int64, error) {
	if mm.imgRepo == nil {
		return nil, 0, nil
	}
	return mm.imgRepo.List(ctx, f)
}

// GetImageGenRecord 单条图片生成记录
func (mm *MediaManager) GetImageGenRecord(ctx context.Context, id uint64) (*ImageGenRecord, error) {
	if mm.imgRepo == nil {
		return nil, ErrMediaNotFound
	}
	return mm.imgRepo.Get(ctx, id)
}

func normalizeImageInput(in *ImageGenInput) (*ImageGenInput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: 请求为空", ErrMediaInvalid)
	}
	in.Mode = strings.TrimSpace(in.Mode)
	if in.Mode == "" {
		in.Mode = ModeText2Img
	}
	if in.Mode != ModeText2Img && in.Mode != ModeImg2Img {
		return nil, fmt.Errorf("%w: mode 只支持 text2img | img2img", ErrMediaInvalid)
	}
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		return nil, fmt.Errorf("%w: prompt 不能为空", ErrMediaInvalid)
	}
	if in.Mode == ModeImg2Img {
		in.Images = cleanStrList(in.Images)
		if len(in.Images) == 0 {
			return nil, fmt.Errorf("%w: 图生图需要至少一张参考图", ErrMediaInvalid)
		}
	} else {
		in.Images = nil
	}
	if in.Count < 1 || in.Count > 4 {
		in.Count = 1
	}
	return in, nil
}

// redactInputs 参考图落库脱敏：data URI 可能几 MB，只留协议头供排查
func redactInputs(imgs []string) []string {
	out := make([]string, 0, len(imgs))
	for _, img := range imgs {
		if strings.HasPrefix(img, "data:") {
			head := img
			if i := strings.Index(img, ","); i > 0 && i < 200 {
				head = img[:i] + ",<...data 已省略...>"
			} else {
				head = img[:min(64, len(img))] + "…<data 已省略>"
			}
			out = append(out, head)
		} else {
			out = append(out, img)
		}
	}
	return out
}

// —— 视频生成 ——

// SubmitVideo 提交视频生成任务（异步）：记录落库后立刻返回，
// 之后由 GetVideo / 后台轮询器把任务推进到终态。
func (mm *MediaManager) SubmitVideo(ctx context.Context, in *VideoGenInput) (*VideoGenRecord, error) {
	in, err := normalizeVideoInput(in)
	if err != nil {
		return nil, err
	}
	p, target, err := mm.resolveVideo(ctx, in.Provider, in.Model)
	if err != nil {
		return nil, err
	}
	info := identity.From(ctx)
	rec := &VideoGenRecord{
		TenantID:    info.TenantID,
		UserID:      info.UserID,
		Provider:    p.Name,
		Model:       target.Model,
		Prompt:      in.Prompt,
		Size:        in.Size,
		DurationSec: in.DurationSec,
		Status:      MediaQueued,
		SubmittedAt: time.Now(),
	}
	if in.Image != "" {
		rec.Inputs = redactInputs([]string{in.Image})
	}
	if err := mm.vidRepo.Insert(ctx, rec); err != nil {
		return nil, fmt.Errorf("写入视频生成记录: %w", err)
	}
	taskID, serr := mm.vidClient.Submit(ctx, target, in)
	if serr != nil {
		rec.Status = MediaFailed
		rec.Error = truncateErr(serr)
		rec.DurationMS = 0
		if uerr := mm.vidRepo.Update(ctx, rec); uerr != nil {
			log.Warn("[media] 视频任务失败落库失败", "id", rec.ID, "err", uerr)
		}
		return rec, serr
	}
	rec.ProviderTaskID = taskID
	rec.Status = MediaRunning
	if uerr := mm.vidRepo.Update(ctx, rec); uerr != nil {
		log.Warn("[media] 视频任务 ID 落库失败", "id", rec.ID, "err", uerr)
	}
	return rec, nil
}

// GetVideo 查询视频任务：进行中的任务顺带向厂商轮询一次（惰性收尾），
// 已终态的直接返回，不产生额外厂商调用。
func (mm *MediaManager) GetVideo(ctx context.Context, id uint64) (*VideoGenRecord, error) {
	if mm.vidRepo == nil {
		return nil, ErrMediaNotFound
	}
	rec, err := mm.vidRepo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Status != MediaQueued && rec.Status != MediaRunning {
		return rec, nil
	}
	if mm.vidClient == nil {
		return rec, nil
	}
	changed, _ := mm.refreshVideo(ctx, rec)
	if !changed {
		return rec, nil
	}
	fresh, gerr := mm.vidRepo.Get(ctx, id)
	if gerr != nil {
		return rec, nil
	}
	return fresh, nil
}

// refreshVideo 轮询一次并把终态落库；返回记录是否发生变化
func (mm *MediaManager) refreshVideo(ctx context.Context, rec *VideoGenRecord) (bool, error) {
	p, err := mm.mgr.GetProvider(ctx, rec.Provider)
	if err != nil {
		return false, err
	}
	target := mm.targetFor(p, rec.Model)
	status, output, perr := mm.vidClient.Poll(ctx, target, rec.ProviderTaskID)
	if perr != nil {
		// 轮询报错 ≠ 任务失败（可能瞬时抖动）：任务超龄（超过 24h 无进展）才判死
		if time.Since(rec.SubmittedAt) > pollActiveSec {
			rec.Status = MediaFailed
			rec.Error = truncateErr(fmt.Errorf("任务超时未完成: %v", perr))
			return true, mm.vidRepo.Update(ctx, rec)
		}
		return false, perr
	}
	switch status {
	case MediaSucceeded:
		rec.Status = MediaSucceeded
		rec.Output = output
		rec.DurationMS = time.Since(rec.SubmittedAt).Milliseconds()
		rec.FinishedAt = time.Now()
	case MediaFailed:
		rec.Status = MediaFailed
		rec.Error = output // 失败时 Poll 把原因放在 output 位
		rec.DurationMS = time.Since(rec.SubmittedAt).Milliseconds()
		rec.FinishedAt = time.Now()
	default:
		if rec.Status == MediaQueued && status == MediaRunning {
			rec.Status = MediaRunning
		} else {
			return false, nil
		}
	}
	if uerr := mm.vidRepo.Update(ctx, rec); uerr != nil {
		log.Warn("[media] 视频任务状态落库失败", "id", rec.ID, "err", uerr)
	}
	return true, nil
}

// ListVideoGenRecords 视频生成历史
func (mm *MediaManager) ListVideoGenRecords(ctx context.Context, f MediaFilter) ([]*VideoGenRecord, int64, error) {
	if mm.vidRepo == nil {
		return nil, 0, nil
	}
	return mm.vidRepo.List(ctx, f)
}

func normalizeVideoInput(in *VideoGenInput) (*VideoGenInput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: 请求为空", ErrMediaInvalid)
	}
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		return nil, fmt.Errorf("%w: prompt 不能为空", ErrMediaInvalid)
	}
	in.Image = strings.TrimSpace(in.Image)
	if in.DurationSec < 0 || in.DurationSec > 60 {
		in.DurationSec = 0
	}
	return in, nil
}

func cleanStrList(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncateErr(err error) string {
	msg := err.Error()
	if len(msg) > 480 {
		msg = msg[:480]
	}
	return msg
}
