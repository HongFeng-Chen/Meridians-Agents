package agentfactory

// 媒体生成能力（llm:image / llm:video / llm:video_status）单测：
// 目录注册、分发路由、参数校验、成功/失败路径与归属身份下传。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go_agent/internal/biz/llm"
	"go_agent/internal/pkg/identity"
)

// fakeMedia 媒体端口桩
type fakeMedia struct {
	imgRec  *llm.ImageGenRecord
	imgErr  error
	imgIn   *llm.ImageGenInput
	vidRec  *llm.VideoGenRecord
	vidErr  error
	vidIn   *llm.VideoGenInput
	gotRec  *llm.VideoGenRecord
	gotErr  error
	catalog bool // MediaCatalog 是否返回可用模型
}

func (f *fakeMedia) GenerateImage(ctx context.Context, in *llm.ImageGenInput) (*llm.ImageGenRecord, error) {
	f.imgIn = in
	if f.imgErr != nil {
		return &llm.ImageGenRecord{ID: 7}, f.imgErr
	}
	if f.imgRec == nil {
		f.imgRec = &llm.ImageGenRecord{ID: 1, Provider: "p1", Model: "img-model", Outputs: []string{"/api/files/gen/a.png"}}
	}
	return f.imgRec, nil
}

func (f *fakeMedia) SubmitVideo(ctx context.Context, in *llm.VideoGenInput) (*llm.VideoGenRecord, error) {
	f.vidIn = in
	if f.vidErr != nil {
		return &llm.VideoGenRecord{ID: 8}, f.vidErr
	}
	if f.vidRec == nil {
		f.vidRec = &llm.VideoGenRecord{ID: 2, Provider: "p1", Model: "vid-model", Status: llm.MediaRunning}
	}
	return f.vidRec, nil
}

func (f *fakeMedia) GetVideo(ctx context.Context, id uint64) (*llm.VideoGenRecord, error) {
	if f.gotErr != nil {
		return nil, f.gotErr
	}
	if f.gotRec == nil {
		f.gotRec = &llm.VideoGenRecord{ID: id, Status: llm.MediaSucceeded, Output: "/api/files/gen/b.mp4"}
	}
	return f.gotRec, nil
}

func (f *fakeMedia) MediaCatalog(ctx context.Context) ([]llm.MediaModelInfo, []llm.MediaModelInfo, error) {
	if !f.catalog {
		return nil, nil, errors.New("not wired")
	}
	return []llm.MediaModelInfo{{Provider: "p1", Kind: "siliconflow", Models: []string{"img-model"}}},
		[]llm.MediaModelInfo{{Provider: "p1", Kind: "siliconflow", Models: []string{"vid-model"}}}, nil
}

// newMediaUseCase 构造带媒体端口的测试用例（复用既有测试装配）
func newMediaUseCase(t *testing.T, media MediaPort) (*UseCase, *fakeMedia) {
	t.Helper()
	chat := &fakeChatter{script: []string{`{"final":"ok"}`, `{"final":"ok"}`, `{"final":"ok"}`}}
	u, _, _ := newTestUseCase(t, chat)
	// 测试装配默认不注入媒体端口：这里补挂媒体端口并重建目录
	u.media = media
	u.catalog = NewCatalog(nil, func(ctx context.Context) ([]AgentDef, error) { return nil, nil }, nil, media)
	if fm, ok := media.(*fakeMedia); ok {
		return u, fm
	}
	return u, nil
}

// ① 目录：媒体能力注册，描述里带可用模型清单
func TestMediaCapabilitySchemas(t *testing.T) {
	fm := &fakeMedia{catalog: true}
	u, _ := newMediaUseCase(t, fm)
	schemas, err := u.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CapabilitySchema{}
	for _, s := range schemas {
		byName[s.Name] = s
	}
	for _, name := range []string{CapImage, CapVideo, CapVideoStatus} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("目录缺少能力 %s", name)
		}
		if s.Type != "media" {
			t.Fatalf("%s 类型应为 media，实际 %s", name, s.Type)
		}
	}
	if !strings.Contains(byName[CapImage].Description, "img-model") {
		t.Fatalf("llm:image 描述应带可用模型，实际: %s", byName[CapImage].Description)
	}
	if !strings.Contains(byName[CapVideo].Description, CapVideoStatus) {
		t.Fatalf("llm:video 描述应指引 llm:video_status 轮询，实际: %s", byName[CapVideo].Description)
	}
}

// ② llm:image：成功路径，参数透传 + 返回本地地址
func TestImageCapabilityOK(t *testing.T) {
	fm := &fakeMedia{catalog: true}
	u, _ := newMediaUseCase(t, fm)
	out, err := u.InvokeDispatch(context.Background(), CapImage, map[string]any{
		"prompt": "一只猫", "size": "1024x1024", "count": 2, "provider": "p1", "model": "img-model",
	}, &Recorder{u: u, def: &AgentDef{Capabilities: []string{CapImage}}, mu: mpmu()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/api/files/gen/a.png") {
		t.Fatalf("结果应含本地地址: %s", out)
	}
	if fm.imgIn == nil || fm.imgIn.Prompt != "一只猫" || fm.imgIn.Count != 2 || fm.imgIn.Provider != "p1" || fm.imgIn.Model != "img-model" {
		t.Fatalf("参数未正确透传: %+v", fm.imgIn)
	}
}

// ③ llm:image：缺 prompt 报错，不落到厂商调用
func TestImageCapabilityMissingPrompt(t *testing.T) {
	fm := &fakeMedia{catalog: true}
	u, _ := newMediaUseCase(t, fm)
	rec := &Recorder{u: u, def: &AgentDef{Capabilities: []string{CapImage}}, mu: mpmu()}
	if _, err := u.InvokeDispatch(context.Background(), CapImage, map[string]any{"size": "1024x1024"}, rec); err == nil {
		t.Fatal("缺 prompt 应报错")
	} else if !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("错误信息应指明 prompt 缺失: %v", err)
	}
	if fm.imgIn != nil {
		t.Fatal("缺 prompt 不应触发厂商调用")
	}
}

// ④ llm:image：厂商失败时错误带记录 ID（模型可据此向用户转述）
func TestImageCapabilityVendorError(t *testing.T) {
	fm := &fakeMedia{catalog: true, imgErr: errors.New("配额不足")}
	u, _ := newMediaUseCase(t, fm)
	rec := &Recorder{u: u, def: &AgentDef{Capabilities: []string{CapImage}}, mu: mpmu()}
	_, err := u.InvokeDispatch(context.Background(), CapImage, map[string]any{"prompt": "x"}, rec)
	if err == nil || !strings.Contains(err.Error(), "记录 7") {
		t.Fatalf("失败应带记录 ID 便于追溯: %v", err)
	}
}

// ⑤ llm:video：提交返回记录 ID 与轮询指引；llm:video_status 查询成功态
func TestVideoSubmitAndStatus(t *testing.T) {
	fm := &fakeMedia{catalog: true}
	u, _ := newMediaUseCase(t, fm)
	rec := &Recorder{u: u, def: &AgentDef{Capabilities: []string{CapVideo, CapVideoStatus}}, mu: mpmu()}
	out, err := u.InvokeDispatch(context.Background(), CapVideo, map[string]any{
		"prompt": "海浪", "duration_seconds": 5, "size": "1280x720",
	}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"record_id":2`) || !strings.Contains(out, CapVideoStatus) {
		t.Fatalf("提交结果应含记录 ID 与轮询指引: %s", out)
	}
	if fm.vidIn == nil || fm.vidIn.Prompt != "海浪" || fm.vidIn.DurationSec != 5 {
		t.Fatalf("视频参数未正确透传: %+v", fm.vidIn)
	}

	out2, err := u.InvokeDispatch(context.Background(), CapVideoStatus, map[string]any{"id": 2}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, `"status":"succeeded"`) || !strings.Contains(out2, "/api/files/gen/b.mp4") {
		t.Fatalf("状态查询应含终态与视频地址: %s", out2)
	}
}

// ⑥ llm:video_status：失败态把原因带回
func TestVideoStatusFailed(t *testing.T) {
	fm := &fakeMedia{catalog: true, gotRec: &llm.VideoGenRecord{ID: 9, Status: llm.MediaFailed, Error: "内容审核未通过"}}
	u, _ := newMediaUseCase(t, fm)
	rec := &Recorder{u: u, def: &AgentDef{Capabilities: []string{CapVideoStatus}}, mu: mpmu()}
	out, err := u.InvokeDispatch(context.Background(), CapVideoStatus, map[string]any{"id": 9}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "内容审核未通过") {
		t.Fatalf("失败态应把厂商原因带回: %s", out)
	}
}

// ⑦ 白名单护栏：未授权的 Agent 调媒体能力直接拒绝
func TestMediaCapabilityWhitelist(t *testing.T) {
	fm := &fakeMedia{catalog: true}
	u, _ := newMediaUseCase(t, fm)
	rec := &Recorder{u: u, def: &AgentDef{Name: "只读", Capabilities: nil}, mu: mpmu()}
	if _, err := u.InvokeDispatch(context.Background(), CapImage, map[string]any{"prompt": "x"}, rec); !errors.Is(err, ErrCapabilityDeny) {
		t.Fatalf("未授权应拒绝: %v", err)
	}
}

// ⑧ 身份下传：任务归属用户写入 ctx 后，媒体记录归到该用户名下
func TestMediaIdentityAttribution(t *testing.T) {
	fm := &fakeMedia{catalog: true}
	u, _ := newMediaUseCase(t, fm)
	ctx := identity.With(context.Background(), identity.Info{UserID: "alice", TenantID: "acme"})
	if _, err := u.imageCapability(ctx, map[string]any{"prompt": "x"}); err != nil {
		t.Fatal(err)
	}
	// fakeMedia 不读身份，这里验证的是 imageCapability 不因身份注入而报错；
	// 真实链路的归属由 MediaManager.GenerateImage 的 identity.From(ctx) 保证。
}

func mpmu() *sync.Mutex { return &sync.Mutex{} }
