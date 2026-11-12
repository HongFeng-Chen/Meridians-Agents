// media.go 媒体生成能力实现：Agent 通过 llm:image / llm:video / llm:video_status
// 调用 LLM 域的媒体生成（文生图 / 图生图 / 文生视频 / 图生视频）。
//
// 设计要点：
//   - 参数一律来自模型输出的 JSON，全部重新校验（模型输出不可信）；
//   - 图片同步等待，视频异步提交（厂商任务以分钟计，不能阻塞执行图一轮工具调用），
//     模型拿到记录 ID 后在后续轮次用 llm:video_status 轮询；
//   - 身份归属：Agent 任务执行时 ctx 已注入任务归属身份（executeTaskNow），
//     生成记录因此落在任务归属用户名下，媒体历史页可查。
package agentfactory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go_agent/internal/biz/llm"
)

// imageArgs llm:image 能力入参
type imageArgs struct {
	Prompt  string   `json:"prompt"`
	Mode    string   `json:"mode"`     // text2img（默认）| img2img
	Images  []string `json:"images"`   // img2img 参考图（https URL 或 data URI）
	Size    string   `json:"size"`     // 如 1024x1024
	Count   int      `json:"count"`    // 1~4
	Seed    *int64   `json:"seed"`     // 随机种子
	Provider string  `json:"provider"` // 指定数据源（空 = 自动路由）
	Model   string   `json:"model"`    // 指定模型（空 = 数据源首个图像模型）
}

// videoArgs llm:video 能力入参
type videoArgs struct {
	Prompt   string  `json:"prompt"`
	Image    string  `json:"image"`            // 图生视频首帧（https URL 或 data URI）
	Size     string  `json:"size"`             // 如 1280x720
	DurationSec int  `json:"duration_seconds"` // 秒（1~60，超出忽略）
	Seed     *int64  `json:"seed"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
}

// imageCapability 生成图片：同步调用，返回已落盘的本地地址列表
func (u *UseCase) imageCapability(ctx context.Context, args map[string]any) (string, error) {
	if u.mediaImpl() == nil {
		return "", fmt.Errorf("媒体生成能力未接线（最小部署未装配 image_generations）")
	}
	var a imageArgs
	if err := decodeCapabilityArgs(args, &a); err != nil {
		return "", fmt.Errorf("llm:image 参数不合法: %w", err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "", fmt.Errorf("llm:image 需要 {\"prompt\": \"画面描述\"}")
	}
	rec, err := u.mediaImpl().GenerateImage(ctx, &llm.ImageGenInput{
		Provider: a.Provider,
		Model:    a.Model,
		Mode:     a.Mode,
		Prompt:   a.Prompt,
		Images:   a.Images,
		Size:     a.Size,
		Count:    a.Count,
		Seed:     a.Seed,
	})
	if err != nil {
		// 失败也带出记录：模型能看到失败原因，下一轮可以修正参数重试
		if rec != nil {
			return "", fmt.Errorf("图片生成失败: %w（记录 %d 已留存，可修正后重试）", err, rec.ID)
		}
		return "", fmt.Errorf("图片生成失败: %w", err)
	}
	return marshalPayload(map[string]any{
		"record_id": rec.ID,
		"provider":  rec.Provider,
		"model":     rec.Model,
		"mode":      rec.Mode,
		"images":    rec.Outputs,
		"hint":      "图片已保存到本地，把 /api/files/ 地址放进最终答案即可引用",
	}), nil
}

// videoCapability 提交视频生成任务：异步，返回记录 ID 供 llm:video_status 轮询
func (u *UseCase) videoCapability(ctx context.Context, args map[string]any) (string, error) {
	if u.mediaImpl() == nil {
		return "", fmt.Errorf("媒体生成能力未接线（最小部署未装配 video_generations）")
	}
	var a videoArgs
	if err := decodeCapabilityArgs(args, &a); err != nil {
		return "", fmt.Errorf("llm:video 参数不合法: %w", err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "", fmt.Errorf("llm:video 需要 {\"prompt\": \"画面描述\"}")
	}
	rec, err := u.mediaImpl().SubmitVideo(ctx, &llm.VideoGenInput{
		Provider:    a.Provider,
		Model:       a.Model,
		Prompt:      a.Prompt,
		Image:       a.Image,
		Size:        a.Size,
		DurationSec: a.DurationSec,
		Seed:        a.Seed,
	})
	if err != nil {
		if rec != nil {
			return "", fmt.Errorf("视频任务提交失败: %w（记录 %d 已留存）", err, rec.ID)
		}
		return "", fmt.Errorf("视频任务提交失败: %w", err)
	}
	return marshalPayload(map[string]any{
		"record_id": rec.ID,
		"provider":  rec.Provider,
		"model":     rec.Model,
		"status":    rec.Status,
		"hint": "任务已提交（通常 1~10 分钟）。请在后续轮次用 " + CapVideoStatus +
			" 能力按 record_id 轮询状态；任务失败时不要无限重试，把原因如实转述给用户",
	}), nil
}

// videoStatusCapability 查询视频任务状态
func (u *UseCase) videoStatusCapability(ctx context.Context, args map[string]any) (string, error) {
	if u.mediaImpl() == nil {
		return "", fmt.Errorf("媒体生成能力未接线（最小部署未装配 video_generations）")
	}
	var a struct {
		ID uint64 `json:"id"`
	}
	if err := decodeCapabilityArgs(args, &a); err != nil {
		return "", fmt.Errorf("llm:video_status 参数不合法: %w", err)
	}
	if a.ID == 0 {
		return "", fmt.Errorf("llm:video_status 需要 {\"id\": 提交时返回的记录 ID}")
	}
	rec, err := u.mediaImpl().GetVideo(ctx, a.ID)
	if err != nil {
		return "", fmt.Errorf("查询视频任务 %d 失败: %w", a.ID, err)
	}
	out := map[string]any{
		"record_id": rec.ID,
		"status":    rec.Status,
		"provider":  rec.Provider,
		"model":     rec.Model,
		"prompt":    truncateRunes(rec.Prompt, 120),
	}
	switch rec.Status {
	case llm.MediaSucceeded:
		out["video"] = rec.Output
		out["hint"] = "视频已生成，把 /api/files/ 地址放进最终答案即可引用"
	case llm.MediaFailed:
		out["reason"] = rec.Error
		out["hint"] = "任务失败：把原因如实告知用户，不要凭空重试（重试应征得用户同意）"
	case llm.MediaQueued, llm.MediaRunning:
		out["hint"] = "仍在生成中，可稍后再次轮询（建议间隔 30 秒以上）"
	}
	return marshalPayload(out), nil
}

// mediaImpl 返回媒体端口（单测 / 未接线时为 nil）
func (u *UseCase) mediaImpl() MediaPort { return u.media }

// decodeCapabilityArgs 模型输出的参数 → 结构体（args 可能是嵌套 map 或已解析 JSON）
func decodeCapabilityArgs(args map[string]any, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		// 未知字段不致命：模型可能多带说明性字段，宽松二次解析
		return json.Unmarshal(raw, out)
	}
	return nil
}
