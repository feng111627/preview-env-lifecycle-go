package previewenv

import (
	"testing"
)

func TestStore_AllocateAndRelease(t *testing.T) {
	s := NewStore(10)

	if err := s.Allocate("env-1", Resources{Namespace: "ns-1", Units: 4}); err != nil {
		t.Fatalf("allocate env-1: %v", err)
	}
	t.Logf("input: env=env-1 ns=ns-1 units=4; 判定: 占用成功, used=%d cap=%d", s.OccupiedUnits(), 10)

	// 同一命名空间冲突必须被拒绝，且不能产生部分占用。
	err := s.Allocate("env-2", Resources{Namespace: "ns-1", Units: 1})
	if r := ReasonOf(err); r != ReasonResourceUnavailable {
		t.Fatalf("重复命名空间应得到 %s, got reason=%s err=%v", ReasonResourceUnavailable, r, err)
	}
	t.Logf("input: env=env-2 ns=ns-1; 判定: 命名空间已被 env-1 占用 -> reason=%s, used 保持=%d",
		ReasonOf(err), s.OccupiedUnits())

	// 超出总容量。
	if err := s.Allocate("env-3", Resources{Namespace: "ns-3", Units: 7}); err == nil {
		t.Fatal("超出容量的分配应当失败")
	} else if r := ReasonOf(err); r != ReasonResourceUnavailable {
		t.Fatalf("容量超限应得到 %s, got %s", ReasonResourceUnavailable, r)
	}
	t.Logf("input: env=env-3 units=7; 判定: used=4+7>cap=10 -> reason=%s", ReasonResourceUnavailable)

	// 同一 envID 重复分配被拒绝。
	if err := s.Allocate("env-1", Resources{Namespace: "ns-x", Units: 1}); err == nil {
		t.Fatal("同一 envID 重复分配应当失败")
	} else if r := ReasonOf(err); r != ReasonAlreadyExists {
		t.Fatalf("重复 envID 应得到 %s, got %s", ReasonAlreadyExists, r)
	}

	if err := s.Release("env-1"); err != nil {
		t.Fatalf("release env-1: %v", err)
	}
	if got := s.OccupiedUnits(); got != 0 {
		t.Fatalf("释放后占用应为 0, got %d", got)
	}
	if got := s.NamespaceOwner("ns-1"); got != "" {
		t.Fatalf("释放后命名空间应空闲, got owner=%q", got)
	}
	t.Log("判定: release 后 used=0 且 ns-1 空闲，无泄漏")

	// 重复释放必须返回可区分的 not_found。
	if err := s.Release("env-1"); err == nil || ReasonOf(err) != ReasonNotFound {
		t.Fatalf("重复释放应得到 %s, got %v", ReasonNotFound, err)
	}
	t.Logf("input: 重复 release env-1; 判定: reason=%s（可安全重试/幂等）", ReasonNotFound)
}
