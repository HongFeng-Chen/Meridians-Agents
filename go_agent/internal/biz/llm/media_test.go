// media_test.go 媒体生成用例的单测：路由、记录生命周期与轮询收敛。
package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// —— fakes ——

type fakeImageRepo struct {
	recs map[uint64]*ImageGenRecord
	seq  uint64
}

func newFakeImageRepo() *fakeImageRepo { return &fakeImageRepo{recs: map[uint64]*ImageGenRecord{}} }

func (f *fakeImageRepo) Insert(_ context.Context, rec *ImageGenRecord) error {
	f.seq++
	rec.ID = f.seq
	rec.CreatedAt = time.Now()
	rec.UpdatedAt = rec.CreatedAt
	f.recs[rec.ID] = rec
	return nil
}

func (f *fakeImageRepo) Update(_ context.Context, rec *ImageGenRecord) error {
	if _, ok := f.recs[rec.ID]; !ok {
		return ErrMediaNotFound
	}
	rec.UpdatedAt = time.Now()
	f.recs[rec.ID] = rec
	return nil
}

func (f *fakeImageRepo) Get(_ context.Context, id uint64) (*ImageGenRecord, error) {
	if rec, ok := f.recs[id]; ok {
		return rec, nil
	}
	return nil, ErrMediaNotFound
}

func (f *fakeImageRepo) List(_ context.Context, f2 MediaFilter) ([]*ImageGenRecord, int64, error) {
	return nil, 0, nil
}

type fakeVideoRepo struct {
	recs map[uint64]*VideoGenRecord
	seq  uint64
}

func newFakeVideoRepo() *fakeVideoRepo { return &fakeVideoRepo{recs: map[uint64]*VideoGenRecord{}} }

func (f *fakeVideoRepo) Insert(_ context.Context, rec *VideoGenRecord) error {
	f.seq++
	rec.ID = f.seq
	rec.CreatedAt = time.Now()
	rec.UpdatedAt = rec.CreatedAt
	f.recs[rec.ID] = rec
	return nil
}

func (f *fakeVideoRepo) Update(_ context.Context, rec *VideoGenRecord) error {
	if _, ok := f.recs[rec.ID]; !ok {
		return ErrMediaNotFound
	}
	rec.UpdatedAt = time.Now()
	f.recs[rec.ID] = rec
	return nil
}

func (f *fakeVideoRepo) Get(_ context.Context, id uint64) (*VideoGenRecord, error) {
	if rec, ok := f.recs[id]; ok {
		return rec, nil
	}
	return nil, ErrMediaNotFound
}

func (f *fakeVideoRepo) List(_ context.Context, f2 MediaFilter) ([]*VideoGenRecord, int64, error) {
	return nil, 0, nil
}

func (f *fakeVideoRepo) ListActive(_ context.Context, statuses []string, _ time.Time, limit int) ([]*VideoGenRecord, error) {
	want := map[string]bool{}
	for _, s := range statuses {
		want[s] = true
	}
	var out []*VideoGenRecord
	for _, rec := range f.recs {
		if want[rec.Status] && len(out) < limit {
			out = append(out, rec)
		}
	}
	return out, nil
}

type fakeImageClient struct {
	got     *MediaTarget
	outputs []string
	err     error
}

func (f *fakeImageClient) Generate(_ context.Context, t MediaTarget, _ *ImageGenInput) ([]string, error) {
	f.got = &t
	return f.outputs, f.err
}

type fakeVideoClient struct {
	submitTask string
	submitErr  error
	polls      []string // 每次轮询返回的状态序列
	pollOut    []string
	calls      int
}

func (f *fakeVideoClient) Submit(_ context.Context, _ MediaTarget, _ *VideoGenInput) (string, error) {
	return f.submitTask, f.submitErr
}

func (f *fakeVideoClient) Poll(_ context.Context, _ MediaTarget, _ string) (string, string, error) {
	i := f.calls
	f.calls++
	if i < len(f.polls) {
		return f.polls[i], f.pollOut[i], nil
	}
	return MediaRunning, "", nil
}

// —— 测试 ——

func newTestMedia(t *testing.T) (*MediaManager, *fakeImageRepo, *fakeVideoRepo, *fakeImageClient, *fakeVideoClient) {
	t.Helper()
	mgr := NewManager(testConf("https://fallback.example.com/v1", "fallback"), nil, newFakeRepo(), nil, nil)
	imgRepo, vidRepo := newFakeImageRepo(), newFakeVideoRepo()
	imgClient, vidClient := &fakeImageClient{}, &fakeVideoClient{}
	mm := NewMediaManager(mgr, imgRepo, vidRepo, imgClient, vidClient)
	return mm, imgRepo, vidRepo, imgClient, vidClient
}

// 数据源路由：显式 provider 不匹配时报错；自动路由找第一个有图像模型的源
func TestMediaResolveImageProvider(t *testing.T) {
	mm, _, _, imgClient, _ := newTestMedia(t)
	ctx := context.Background()
	mgr := mm.mgr

	if _, err := mgr.CreateProvider(ctx, CreateProviderInput{
		Name: "img-src", BaseURL: "https://api.example.com/v1", Models: []string{"chat-1"},
		ImageModels: []string{"kolors-1", "kolors-2"}, APIKey: "sk-x",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.CreateProvider(ctx, CreateProviderInput{
		Name: "chat-only", BaseURL: "https://api.example.com/v1", Models: []string{"chat-2"},
	}); err != nil {
		t.Fatal(err)
	}

	// 自动路由命中 img-src；未配置的模型名被拒绝
	imgClient.outputs = []string{"/api/files/gen/x.png"}
	res, err := mm.GenerateImage(ctx, &ImageGenInput{Prompt: "一只猫"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "img-src" || res.Model != "kolors-1" {
		t.Errorf("应路由到 img-src/kolors-1: %s/%s", res.Provider, res.Model)
	}
	if _, err := mm.GenerateImage(ctx, &ImageGenInput{Prompt: "x", Provider: "img-src", Model: "other"}); err == nil {
		t.Error("未配置的模型名应报错")
	}
	if _, err := mm.GenerateImage(ctx, &ImageGenInput{Prompt: "x", Provider: "chat-only"}); err == nil {
		t.Error("没有图像模型的数据源应报错")
	}
}

// 必填校验：prompt 缺失、图生图缺参考图
func TestMediaImageValidation(t *testing.T) {
	mm, _, _, _, _ := newTestMedia(t)
	ctx := context.Background()
	if _, err := mm.GenerateImage(ctx, &ImageGenInput{}); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Errorf("prompt 缺失应报错: %v", err)
	}
	if _, err := mm.GenerateImage(ctx, &ImageGenInput{Mode: ModeImg2Img, Prompt: "x"}); err == nil {
		t.Error("图生图缺参考图应报错")
	}
}

// 失败也要落库：记录里留错误与耗时
func TestMediaImageFailureRecorded(t *testing.T) {
	mm, imgRepo, _, imgClient, _ := newTestMedia(t)
	ctx := context.Background()
	if _, err := mm.mgr.CreateProvider(ctx, CreateProviderInput{
		Name: "img-src", BaseURL: "https://api.example.com/v1", Models: []string{"c"},
		ImageModels: []string{"m1"}, APIKey: "sk",
	}); err != nil {
		t.Fatal(err)
	}
	imgClient.err = errors.New("厂商 429")
	res, err := mm.GenerateImage(ctx, &ImageGenInput{Prompt: "x"})
	if err == nil {
		t.Fatal("厂商失败应上抛")
	}
	if len(imgRepo.recs) != 1 {
		t.Fatalf("失败记录应落库: %d", len(imgRepo.recs))
	}
	saved := imgRepo.recs[res.ID]
	if saved.OK || saved.Error == "" || saved.Provider != "img-src" {
		t.Errorf("失败记录不完整: %+v", saved)
	}
}

// 视频任务生命周期：提交 → running → 成功落盘
func TestVideoLifecycle(t *testing.T) {
	mm, _, vidRepo, _, vidClient := newTestMedia(t)
	ctx := context.Background()
	if _, err := mm.mgr.CreateProvider(ctx, CreateProviderInput{
		Name: "vid-src", BaseURL: "https://api.example.com/v1", Models: []string{"c"},
		VideoModels: []string{"wan-1"}, APIKey: "sk",
	}); err != nil {
		t.Fatal(err)
	}
	vidClient.submitTask = "task-1"

	res, err := mm.SubmitVideo(ctx, &VideoGenInput{Prompt: "奔跑的猫"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != MediaRunning || res.ProviderTaskID != "task-1" {
		t.Fatalf("提交后应 running+带任务 ID: %+v", res)
	}

	// 第一次查询仍在跑
	vidClient.polls = []string{MediaRunning, MediaSucceeded}
	vidClient.pollOut = []string{"", "/api/files/gen/v.mp4"}
	rec, err := mm.GetVideo(ctx, res.ID)
	if err != nil || rec.Status != MediaRunning {
		t.Fatalf("首轮查询应 running: %+v / %v", rec, err)
	}
	// 第二次查询收到成功
	rec, err = mm.GetVideo(ctx, res.ID)
	if err != nil || rec.Status != MediaSucceeded || rec.Output != "/api/files/gen/v.mp4" {
		t.Fatalf("二次查询应成功: %+v / %v", rec, err)
	}
	if rec.FinishedAt.IsZero() {
		t.Error("终态应有 FinishedAt")
	}
	saved, _ := vidRepo.Get(ctx, res.ID)
	if saved.Status != MediaSucceeded {
		t.Errorf("终态应落库: %s", saved.Status)
	}
}

// 提交失败：记录直接落成 failed，错误可见
func TestVideoSubmitFailureRecorded(t *testing.T) {
	mm, _, vidRepo, _, vidClient := newTestMedia(t)
	ctx := context.Background()
	if _, err := mm.mgr.CreateProvider(ctx, CreateProviderInput{
		Name: "vid-src", BaseURL: "https://api.example.com/v1", Models: []string{"c"},
		VideoModels: []string{"wan-1"}, APIKey: "sk",
	}); err != nil {
		t.Fatal(err)
	}
	vidClient.submitErr = errors.New("余额不足")
	res, err := mm.SubmitVideo(ctx, &VideoGenInput{Prompt: "x"})
	if err == nil {
		t.Fatal("提交失败应上抛")
	}
	if res.Status != MediaFailed {
		t.Errorf("记录应落成 failed: %s", res.Status)
	}
	saved, _ := vidRepo.Get(ctx, res.ID)
	if saved.Error == "" {
		t.Error("失败原因应落库")
	}
}

// 无媒体数据源时的明确报错（而非静默）
func TestMediaNoProvider(t *testing.T) {
	mm, _, _, _, _ := newTestMedia(t)
	ctx := context.Background()
	if _, err := mm.GenerateImage(ctx, &ImageGenInput{Prompt: "x"}); !errors.Is(err, ErrMediaNoProvider) {
		t.Errorf("无图像源应返回 ErrMediaNoProvider: %v", err)
	}
	if _, err := mm.SubmitVideo(ctx, &VideoGenInput{Prompt: "x"}); !errors.Is(err, ErrMediaNoProvider) {
		t.Errorf("无视频源应返回 ErrMediaNoProvider: %v", err)
	}
}
