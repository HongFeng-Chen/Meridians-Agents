package service

// 长期记忆的跨用户读写（SEC-006）
//
// 断言两点才算过：
//  1. 别人的记忆读不到（403，而不是 200 + 空对象）；
//  2. 别人的记忆删不掉 —— 只看返回值不够，还要看底层删除有没有被执行。

import (
	"context"
	"errors"
	"testing"
	"time"

	kerrors "github.com/go-kratos/kratos/v3/errors"

	v1 "go_agent/api/memory/v1"
	"go_agent/internal/biz/memory"
	"go_agent/internal/biz/rag/graphrag"
	"go_agent/internal/conf"
)

// stubLongRepo 只实现本用例用到的方法；其余通过嵌入接口占位
// （未实现的方法一旦被调用会 panic，正好能发现意外的依赖）。
type stubLongRepo struct {
	memory.LongTermRepo
	item    *memory.LongTermMemory
	deleted bool
}

func (s *stubLongRepo) Get(_ context.Context, id string) (*memory.LongTermMemory, error) {
	if s.item == nil || s.item.ID != id {
		return nil, errors.New("记忆不存在")
	}
	return s.item, nil
}

func (s *stubLongRepo) Delete(_ context.Context, _ string) error {
	s.deleted = true
	return nil
}

func (s *stubLongRepo) CountBuiltinRules(context.Context) (int64, error) { return 3, nil }

type stubVec struct{ memory.VectorStore }

func (stubVec) EnsureCollection(context.Context) error { return nil }

func (stubVec) DeleteByMemory(context.Context, string) (int64, error) { return 0, nil }

type stubGraph struct{ graphrag.Store }

func (stubGraph) DeleteOrphanNodes(context.Context, string) (int, int, error) { return 0, 0, nil }

func newMemoryServiceForTest(t *testing.T, repo *stubLongRepo) *MemoryService {
	t.Helper()
	uc, err := memory.New(repo, nil, stubVec{}, stubGraph{}, nil, nil, nil, nil, &conf.Data{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	return &MemoryService{uc: uc}
}

func memFor(user string) *memory.LongTermMemory {
	now := time.Now()
	return &memory.LongTermMemory{
		ID: "mem-1", UserID: user, Kind: memory.KindFact, Content: "alice 的私有记忆",
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestGetLongCrossUserDenied(t *testing.T) {
	repo := &stubLongRepo{item: memFor("alice")}
	svc := newMemoryServiceForTest(t, repo)

	// 同用户：可读
	if _, err := svc.GetLong(ctxWith("alice", "jwt"), getLongReq("mem-1")); err != nil {
		t.Fatalf("本人读取应成功: %v", err)
	}

	// 跨用户：403，且不能返回内容
	got, err := svc.GetLong(ctxWith("bob", "jwt"), getLongReq("mem-1"))
	if err == nil {
		t.Fatal("跨用户读取必须失败")
	}
	if got != nil {
		t.Fatal("跨用户读取不能返回记忆内容")
	}
	if code := kerrors.FromError(err).Code; code != 403 {
		t.Fatalf("错误码应为 403，实际 %d（%v）", code, err)
	}
}

func TestDeleteLongCrossUserDenied(t *testing.T) {
	repo := &stubLongRepo{item: memFor("alice")}
	svc := newMemoryServiceForTest(t, repo)

	if _, err := svc.DeleteLong(ctxWith("bob", "jwt"), deleteLongReq("mem-1")); err == nil {
		t.Fatal("跨用户删除必须失败")
	}
	if repo.deleted {
		t.Fatal("跨用户删除不应触达数据层（先校验归属，再执行删除）")
	}

	// 本人删除：放行并真的删
	if _, err := svc.DeleteLong(ctxWith("alice", "jwt"), deleteLongReq("mem-1")); err != nil {
		t.Fatalf("本人删除应成功: %v", err)
	}
	if !repo.deleted {
		t.Fatal("本人删除应触达数据层")
	}
}

func getLongReq(id string) *v1.GetLongRequest { return &v1.GetLongRequest{Id: id} }

func deleteLongReq(id string) *v1.DeleteLongRequest { return &v1.DeleteLongRequest{Id: id} }
