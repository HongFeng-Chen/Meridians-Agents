package harness

// version_test.go 评测可复现的版本绑定（P4）：
// 一次运行必须能说清「哪套用例 + 哪个快照 + 哪个模型」，否则分数无法比较。

import (
	"context"
	"sync"
	"testing"
	"time"
)

// —— 数据集 content hash ——

func TestDatasetChecksum(t *testing.T) {
	base := []Case{
		{ID: 1, Name: "a", Input: "问题一", Expect: "答案一", Enabled: true,
			Checks: []Check{{Kind: CheckContains, Pattern: "答案"}}},
		{ID: 2, Name: "b", Input: "问题二", Enabled: true},
	}
	sum := DatasetChecksum(base)
	if sum == "" || len(sum) != 16 {
		t.Fatalf("checksum 格式不对: %q", sum)
	}

	// 稳定：同样内容算两次必须一致
	if again := DatasetChecksum(base); again != sum {
		t.Fatalf("checksum 不稳定: %q != %q", again, sum)
	}

	// 只改名称/ID/标签 → 不算数据集变更
	renamed := []Case{
		{ID: 11, Name: "新名字", Input: "问题一", Expect: "答案一", Enabled: true,
			Checks: []Check{{Kind: CheckContains, Pattern: "答案"}}},
		{ID: 22, Name: "另一个名字", Input: "问题二", Enabled: true},
	}
	if got := DatasetChecksum(renamed); got != sum {
		t.Fatalf("改名不应改变 checksum: %q != %q", got, sum)
	}

	// 顺序无关
	reordered := []Case{base[1], base[0]}
	if got := DatasetChecksum(reordered); got != sum {
		t.Fatalf("顺序不应改变 checksum: %q != %q", got, sum)
	}

	// 改输入 → 变
	changed := []Case{base[0], {ID: 2, Input: "问题二（改）", Enabled: true}}
	if got := DatasetChecksum(changed); got == sum {
		t.Fatal("输入变更应改变 checksum")
	}

	// 改断言 → 变（判分口径变了，等于换了数据集）
	changedCheck := []Case{base[0], {ID: 2, Input: "问题二", Enabled: true,
		Checks: []Check{{Kind: CheckMinLength, Pattern: "10"}}}}
	if got := DatasetChecksum(changedCheck); got == sum {
		t.Fatal("断言变更应改变 checksum")
	}

	// 停用用例不参与：停掉一条等于数据集变小
	disabled := []Case{base[0], {ID: 2, Input: "问题二", Enabled: false}}
	if got := DatasetChecksum(disabled); got == sum {
		t.Fatal("停用用例应改变 checksum")
	}
}

// —— 运行行绑定 ——

func TestStartRunBindsVersions(t *testing.T) {
	repo := newFakeRepo()
	repo.sets[1] = &Set{ID: 1, Name: "回归集", Target: TargetAgentPrefix + "writer", Enabled: true, Version: 1}
	repo.cases[1] = []Case{
		{ID: 1, SetID: 1, Name: "c1", Input: "写一段", Enabled: true},
	}
	runner := &fakeRunner{output: "写好了", info: TargetInfo{SnapshotID: 7, SnapshotVersion: 3}}
	u := New(repo, runner, nil, nil)

	run, err := u.StartRun(context.Background(), 1, RunConfig{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	waitRun(t, repo, run.ID)

	got := repo.runs[run.ID]
	if got == nil {
		t.Fatal("运行行未落库")
	}
	if got.SnapshotID != 7 || got.SnapshotVersion != 3 {
		t.Fatalf("快照绑定丢失: id=%d version=%d", got.SnapshotID, got.SnapshotVersion)
	}
	// 首次运行只记录 checksum，版本保持 1
	if got.DatasetVersion != 1 || got.DatasetChecksum == "" {
		t.Fatalf("数据集绑定不对: v=%d sum=%q", got.DatasetVersion, got.DatasetChecksum)
	}
	if got.Model != "" {
		t.Fatalf("agent 目标的 model 应为空（由快照决定），得到 %q", got.Model)
	}
}

// 用例内容变了，下一次运行的 dataset 版本 +1，且新旧两次可区分。
func TestDatasetVersionAdvances(t *testing.T) {
	repo := newFakeRepo()
	repo.sets[1] = &Set{ID: 1, Name: "s", Target: TargetModelPrefix + "qwen-max", Enabled: true, Version: 1}
	repo.cases[1] = []Case{{ID: 1, SetID: 1, Input: "q1", Enabled: true}}
	u := New(repo, &fakeRunner{output: "a", info: TargetInfo{Model: "qwen-max"}}, nil, nil)

	r1, err := u.StartRun(context.Background(), 1, RunConfig{})
	if err != nil {
		t.Fatalf("StartRun#1: %v", err)
	}
	waitRun(t, repo, r1.ID)
	v1 := repo.runs[r1.ID].DatasetVersion
	sum1 := repo.runs[r1.ID].DatasetChecksum
	if repo.runs[r1.ID].Model != "qwen-max" {
		t.Fatalf("model 绑定丢失: %q", repo.runs[r1.ID].Model)
	}

	// 改一条用例的内容 → 数据集版本推进
	repo.cases[1][0].Input = "q1（改）"
	r2, err := u.StartRun(context.Background(), 1, RunConfig{})
	if err != nil {
		t.Fatalf("StartRun#2: %v", err)
	}
	waitRun(t, repo, r2.ID)
	v2 := repo.runs[r2.ID].DatasetVersion
	sum2 := repo.runs[r2.ID].DatasetChecksum

	if v2 != v1+1 {
		t.Fatalf("数据集版本应 +1：%d → %d", v1, v2)
	}
	if sum2 == sum1 {
		t.Fatal("内容变了 checksum 应变化")
	}

	// 不改内容再跑一次：版本不动（不能变成「跑一次 +1」）
	r3, err := u.StartRun(context.Background(), 1, RunConfig{})
	if err != nil {
		t.Fatalf("StartRun#3: %v", err)
	}
	waitRun(t, repo, r3.ID)
	if repo.runs[r3.ID].DatasetVersion != v2 {
		t.Fatalf("内容未变不应推进版本: %d != %d", repo.runs[r3.ID].DatasetVersion, v2)
	}
}

// 编辑用例集元信息不能洗掉版本号。
func TestSaveSetKeepsVersion(t *testing.T) {
	repo := newFakeRepo()
	repo.sets[1] = &Set{ID: 1, Name: "旧名", Version: 4, Checksum: "abcdef0123456789", Enabled: true}
	u := New(repo, &fakeRunner{}, nil, nil)

	if _, err := u.SaveSet(context.Background(), SaveSetInput{ID: 1, Name: "新名", Enabled: true}); err != nil {
		t.Fatalf("SaveSet: %v", err)
	}
	got := repo.sets[1]
	if got.Version != 4 || got.Checksum != "abcdef0123456789" {
		t.Fatalf("元信息编辑不该改版本: v=%d sum=%q", got.Version, got.Checksum)
	}
	if got.Name != "新名" {
		t.Fatalf("名称未更新: %q", got.Name)
	}
}

// waitRun 等异步执行结束（fakeRunner 立即返回，轮询足够）
func waitRun(t *testing.T, repo *fakeRepo, id uint64) {
	t.Helper()
	for i := 0; i < 200; i++ {
		repo.mu.Lock()
		r := repo.runs[id]
		done := r != nil && r.Status != RunPending && r.Status != RunRunning
		repo.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("运行 %d 未在预期时间内结束", id)
}

// —— 内存仓储替身 ——

type fakeRepo struct {
	mu    sync.Mutex
	sets  map[uint64]*Set
	cases map[uint64][]Case
	runs  map[uint64]*Run
	items map[uint64][]RunItem
	next  uint64
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		sets:  map[uint64]*Set{},
		cases: map[uint64][]Case{},
		runs:  map[uint64]*Run{},
		items: map[uint64][]RunItem{},
	}
}

func (f *fakeRepo) ListSets(ctx context.Context, flt SetFilter) ([]Set, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Set, 0, len(f.sets))
	for _, s := range f.sets {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeRepo) GetSet(ctx context.Context, id uint64) (*Set, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sets[id]
	if !ok {
		return nil, ErrSetNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakeRepo) CreateSet(ctx context.Context, s *Set) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	s.ID = f.next
	cp := *s
	f.sets[s.ID] = &cp
	return nil
}

func (f *fakeRepo) SaveSet(ctx context.Context, s *Set) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sets[s.ID]; !ok {
		return ErrSetNotFound
	}
	cp := *s
	f.sets[s.ID] = &cp
	return nil
}

func (f *fakeRepo) DeleteSet(ctx context.Context, id uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sets, id)
	delete(f.cases, id)
	return nil
}

func (f *fakeRepo) CountCases(ctx context.Context, setID uint64, enabledOnly bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cases[setID] {
		if !enabledOnly || c.Enabled {
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) ListCases(ctx context.Context, setID uint64, enabledOnly bool) ([]Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Case, 0, len(f.cases[setID]))
	for _, c := range f.cases[setID] {
		if !enabledOnly || c.Enabled {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetCase(ctx context.Context, id uint64) (*Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, list := range f.cases {
		for i := range list {
			if list[i].ID == id {
				cp := list[i]
				return &cp, nil
			}
		}
	}
	return nil, ErrCaseNotFound
}

func (f *fakeRepo) CreateCase(ctx context.Context, c *Case) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	c.ID = f.next
	f.cases[c.SetID] = append(f.cases[c.SetID], *c)
	return nil
}

func (f *fakeRepo) SaveCase(ctx context.Context, c *Case) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.cases[c.SetID]
	for i := range list {
		if list[i].ID == c.ID {
			list[i] = *c
			return nil
		}
	}
	return ErrCaseNotFound
}

func (f *fakeRepo) DeleteCase(ctx context.Context, id uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for setID, list := range f.cases {
		out := list[:0]
		for _, c := range list {
			if c.ID != id {
				out = append(out, c)
			}
		}
		f.cases[setID] = out
	}
	return nil
}

func (f *fakeRepo) CreateRun(ctx context.Context, r *Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	r.ID = f.next
	cp := *r
	f.runs[r.ID] = &cp
	return nil
}

func (f *fakeRepo) SaveRun(ctx context.Context, r *Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.runs[r.ID] = &cp
	return nil
}

func (f *fakeRepo) GetRun(ctx context.Context, id uint64) (*Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, ErrRunNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRepo) ListRuns(ctx context.Context, flt RunFilter) ([]Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Run, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, *r)
	}
	return out, nil
}

func (f *fakeRepo) DeleteRun(ctx context.Context, id uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.runs, id)
	delete(f.items, id)
	return nil
}

func (f *fakeRepo) CreateItem(ctx context.Context, it *RunItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[it.RunID] = append(f.items[it.RunID], *it)
	return nil
}

func (f *fakeRepo) ListItems(ctx context.Context, runID uint64) ([]RunItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RunItem(nil), f.items[runID]...), nil
}

func (f *fakeRepo) DeleteItemsOfRun(ctx context.Context, runID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, runID)
	return nil
}

func (f *fakeRepo) TouchRun(ctx context.Context, runID uint64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	// 与 MySQL 实现同语义：只有 running 的行才续跳
	if r.Status == RunRunning {
		r.HeartbeatAt = at
	}
	return nil
}

func (f *fakeRepo) ReapRuns(ctx context.Context, before time.Time, reason string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, r := range f.runs {
		if r.Status != RunPending && r.Status != RunRunning {
			continue
		}
		beat := r.HeartbeatAt
		if beat.IsZero() {
			beat = r.CreatedAt
		}
		if !beat.Before(before) {
			continue
		}
		r.Status = RunFailed
		r.Error = reason
		r.FinishedAt = time.Now()
		n++
	}
	return n, nil
}
