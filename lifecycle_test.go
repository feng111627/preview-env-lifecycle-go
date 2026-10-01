package previewenv

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock 是可手工推进的时钟。
type fakeClock struct{ t atomic.Int64 }

func newFakeClock(start time.Time) *fakeClock {
	c := &fakeClock{}
	c.t.Store(start.UnixNano())
	return c
}
func (c *fakeClock) now() time.Time { return time.Unix(0, c.t.Load()) }
func (c *fakeClock) advance(d time.Duration) time.Time {
	return time.Unix(0, c.t.Add(int64(d)))
}

func newTestManager(t *testing.T) (*Manager, *Store, *fakeClock) {
	t.Helper()
	clk := newFakeClock(time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC))
	store := NewStore(100)
	m := NewManager(store, time.Hour, WithClock(clk.now),
		WithClaimTimeout(30*time.Second), WithRecentGrace(15*time.Second))
	return m, store, clk
}

func TestManager_CreateClaimDestroyLifecycle(t *testing.T) {
	m, store, _ := newTestManager(t)
	m.SetQuota("alice", Quota{MaxEnvironments: 3, MaxUnits: 20})
	ctx := context.Background()

	env, err := m.Create(ctx, CreateRequest{Name: "feat-x", Owner: "alice", Units: 5})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("input: name=feat-x owner=alice units=5; 判定: 创建成功 id=%s state=%s ns=%s",
		env.ID, env.State, env.Resources.Namespace)
	if env.State != StateReserved {
		t.Fatalf("新环境应为 reserved, got %s", env.State)
	}
	if store.OccupiedUnits() != 5 {
		t.Fatalf("占用单位应为 5, got %d", store.OccupiedUnits())
	}

	// 重复（串行）提交只能拿到已存在的环境与可区分原因。
	dup, err := m.Create(ctx, CreateRequest{Name: "feat-x", Owner: "alice", Units: 5})
	if err == nil || ReasonOf(err) != ReasonAlreadyExists {
		t.Fatalf("重复创建应得到 %s, got %v", ReasonAlreadyExists, err)
	}
	if dup.ID != env.ID {
		t.Fatalf("重复创建应返回同一份环境, got %s want %s", dup.ID, env.ID)
	}
	t.Logf("input: 重复提交 feat-x; 判定: reason=%s, 返回同一 id=%s（不会出现两份）",
		ReasonAlreadyExists, dup.ID)

	// 非所有者不能领取。
	if _, err := m.Claim(ctx, env.ID, "bob"); err == nil || ReasonOf(err) != ReasonPermissionDenied {
		t.Fatalf("非所有者领取应得到 %s, got %v", ReasonPermissionDenied, err)
	}
	t.Logf("input: bob 领取 alice 的环境; 判定: owner=alice != bob -> reason=%s", ReasonPermissionDenied)

	// 所有者领取成功。
	active, err := m.Claim(ctx, env.ID, "alice")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if active.State != StateActive {
		t.Fatalf("领取后应为 active, got %s", active.State)
	}
	t.Logf("input: alice 领取; 判定: 状态 %s -> %s, expires=%s",
		StateReserved, active.State, active.ExpiresAt.Format(time.RFC3339))

	// 非所有者不能销毁。
	if err := m.Destroy(ctx, env.ID, "bob"); err == nil || ReasonOf(err) != ReasonPermissionDenied {
		t.Fatalf("非所有者销毁应得到 %s, got %v", ReasonPermissionDenied, err)
	}
	if err := m.Destroy(ctx, env.ID, "alice"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if store.OccupiedUnits() != 0 || len(store.Snapshot()) != 0 {
		t.Fatalf("销毁后资源应全部归还: units=%d namespaces=%v", store.OccupiedUnits(), store.Snapshot())
	}
	t.Log("判定: 所有者销毁后 used=0、namespace 全部空闲，无泄漏")

	// 销毁后重复销毁/领取得到 not_found。
	if err := m.Destroy(ctx, env.ID, "alice"); err == nil || ReasonOf(err) != ReasonNotFound {
		t.Fatalf("重复销毁应得到 %s, got %v", ReasonNotFound, err)
	}
	if _, err := m.Get(ctx, env.ID); err == nil || ReasonOf(err) != ReasonNotFound {
		t.Fatalf("销毁后 Get 应得到 %s, got %v", ReasonNotFound, err)
	}
	t.Logf("input: 重复销毁/查询已回收环境; 判定: reason=%s（重复回收安全）", ReasonNotFound)
}

func TestManager_CreateRollbackOnFailure(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC))
	store := NewStore(100)
	var called atomic.Int32
	m := NewManager(store, time.Hour, WithClock(clk.now),
		WithFailHook(func(envID, step string) error {
			called.Add(1)
			t.Logf("故障注入触发: env=%s step=%s", envID, step)
			return newError("test", ReasonResourceUnavailable, envID, "", "boom", nil)
		}))
	m.SetQuota("alice", Quota{MaxEnvironments: 5, MaxUnits: 50})

	_, err := m.Create(context.Background(), CreateRequest{Name: "fail-me", Owner: "alice", Units: 6})
	if err == nil || ReasonOf(err) != ReasonResourceUnavailable {
		t.Fatalf("注入失败应得到 %s, got %v", ReasonResourceUnavailable, err)
	}
	if called.Load() != 1 {
		t.Fatalf("fail hook 应被调用 1 次, got %d", called.Load())
	}
	if store.OccupiedUnits() != 0 || len(store.Snapshot()) != 0 {
		t.Fatalf("失败回滚后不应有任何占用: units=%d ns=%v", store.OccupiedUnits(), store.Snapshot())
	}
	t.Logf("input: 创建 fail-me units=6 且 post_allocate 注入失败; 判定: reason=%s, 回滚后 used=%d ns=%d",
		ReasonResourceUnavailable, store.OccupiedUnits(), len(store.Snapshot()))

	// 失败后立即重试：命名空间已归还，应当成功，不出现重复占用。
	m2 := NewManager(store, time.Hour, WithClock(clk.now))
	m2.SetQuota("alice", Quota{MaxEnvironments: 5, MaxUnits: 50})
	env, err := m2.Create(context.Background(), CreateRequest{Name: "fail-me", Owner: "alice", Units: 6})
	if err != nil {
		t.Fatalf("失败后重试应成功: %v", err)
	}
	if owner := store.NamespaceOwner("ns-fail-me"); owner != env.ID {
		t.Fatalf("重试后命名空间占用者=%s want %s", owner, env.ID)
	}
	if store.OccupiedUnits() != 6 {
		t.Fatalf("重试后占用应为 6, got %d", store.OccupiedUnits())
	}
	t.Logf("input: 失败后重新提交 fail-me; 判定: 重试成功 id=%s, 唯一占用 ns-fail-me -> %s, used=%d",
		env.ID, env.ID, store.OccupiedUnits())
}

func TestManager_QuotaDeniedDistinctFromPermission(t *testing.T) {
	m, _, _ := newTestManager(t)
	// carol 未配置配额；dave 配额极紧。
	m.SetQuota("dave", Quota{MaxEnvironments: 1, MaxUnits: 5})
	ctx := context.Background()

	_, err := m.Create(ctx, CreateRequest{Name: "c-1", Owner: "carol", Units: 1})
	if err == nil || ReasonOf(err) != ReasonQuotaExceeded {
		t.Fatalf("无配额用户应得到 %s, got %v", ReasonQuotaExceeded, err)
	}
	t.Logf("input: carol（未配置配额）创建环境; 判定: reason=%s（拒绝来自配额规则）", ReasonQuotaExceeded)

	if _, err := m.Create(ctx, CreateRequest{Name: "d-1", Owner: "dave", Units: 5}); err != nil {
		t.Fatalf("dave 首次创建应成功: %v", err)
	}
	// 环境数量超限。
	_, err = m.Create(ctx, CreateRequest{Name: "d-2", Owner: "dave", Units: 0})
	if err == nil || ReasonOf(err) != ReasonQuotaExceeded {
		t.Fatalf("环境数超限应得到 %s, got %v", ReasonQuotaExceeded, err)
	}
	t.Logf("input: dave 创建第 2 个环境; 判定: count=1>=max=1 -> reason=%s", ReasonQuotaExceeded)

	// 权限错误必须与配额错误可区分：alice 尝试操作 dave 的环境。
	m.SetQuota("alice", Quota{MaxEnvironments: 5, MaxUnits: 50})
	pErr := m.Destroy(ctx, "env-d-1", "alice")
	if pErr == nil || ReasonOf(pErr) != ReasonPermissionDenied {
		t.Fatalf("越权销毁应得到 %s, got %v", ReasonPermissionDenied, pErr)
	}
	le, _ := AsLifecycleError(pErr)
	t.Logf("input: alice 销毁 dave 的 env-d-1; 判定: reason=%s 与配额原因 %s 可严格区分, msg=%q",
		le.Reason, ReasonQuotaExceeded, le.Message)
}
