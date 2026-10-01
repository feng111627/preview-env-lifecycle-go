package previewenv

import (
	"context"
	"testing"
	"time"
)

// assertStoreConsistent 校验本地资源后端与管理器账实一致：
// 每个存活环境的命名空间都恰好被自己占用，且单位数合计相符。
func assertStoreConsistent(t *testing.T, m *Manager, store *Store) {
	t.Helper()
	liveUnits := 0
	liveCount := 0
	for _, env := range m.List(context.Background()) {
		if env.State == StateDestroyed {
			if owner := store.NamespaceOwner(env.Resources.Namespace); owner == env.ID {
				t.Fatalf("已销毁环境 %s 仍占用命名空间 %s", env.ID, env.Resources.Namespace)
			}
			continue
		}
		liveCount++
		liveUnits += env.Resources.Units
		if owner := store.NamespaceOwner(env.Resources.Namespace); owner != env.ID {
			t.Fatalf("存活环境 %s 的命名空间 %s 被 %q 占用（账实不一致）",
				env.ID, env.Resources.Namespace, owner)
		}
	}
	if got := store.OccupiedUnits(); got != liveUnits {
		t.Fatalf("store 占用单位 %d 与存活环境合计 %d 不一致", got, liveUnits)
	}
	t.Logf("账实一致校验通过: 存活环境=%d usedUnits=%d/%d namespace=%v",
		liveCount, liveUnits, store.maxUnits, store.Snapshot())
}

func TestReclaim_ReservedClaimExpiry(t *testing.T) {
	m, store, clk := newTestManager(t)
	m.SetQuota("alice", Quota{MaxEnvironments: 5, MaxUnits: 50})
	ctx := context.Background()

	env, err := m.Create(ctx, CreateRequest{Name: "r-1", Owner: "alice", Units: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: 创建 r-1, claimDeadline=%s; 判定: state=%s",
		env.ClaimDeadline.Format(time.RFC3339), env.State)

	// 领取截止时间刚到之前不回收。
	before := clk.advance(30 * time.Second)
	res, err := m.ReclaimExpired(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Reclaimed {
		t.Fatalf("未到领取截止不应回收, reason=%s", res[0].Reason)
	}
	t.Logf("input: now=%s（恰好 claimDeadline）; 判定: reason=%s 跳过回收",
		before.Format(time.RFC3339), res[0].Reason)

	// 超过领取截止：回收。
	after := clk.advance(time.Second)
	res, _ = m.ReclaimExpired(ctx, after)
	if !res[0].Reclaimed || res[0].Reason != ReasonClaimExpired {
		t.Fatalf("保留超时应回收且原因=%s, got reclaimed=%v reason=%s",
			ReasonClaimExpired, res[0].Reclaimed, res[0].Reason)
	}
	t.Logf("input: now=%s（超过 claimDeadline 1s）; 判定: reclaimed=true reason=%s",
		after.Format(time.RFC3339), res[0].Reason)
	if store.OccupiedUnits() != 0 || store.NamespaceOwner("ns-r-1") != "" {
		t.Fatalf("回收后资源未归还: units=%d ns=%v", store.OccupiedUnits(), store.Snapshot())
	}
	assertStoreConsistent(t, m, store)
}

func TestReclaim_ActiveIdleRules(t *testing.T) {
	m, store, clk := newTestManager(t)
	m.SetQuota("alice", Quota{MaxEnvironments: 5, MaxUnits: 50})
	ctx := context.Background()

	env, _ := m.Create(ctx, CreateRequest{Name: "a-1", Owner: "alice", Units: 3})
	if _, err := m.Claim(ctx, env.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	// 活跃会话进行中：即使 TTL 已过也不回收。
	clk.advance(2 * time.Hour)
	if err := m.BeginSession(ctx, env.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	res, _ := m.ReclaimExpired(ctx, clk.now())
	if res[0].Reclaimed || res[0].Reason != ReasonInUse {
		t.Fatalf("会话进行中应得到 %s, got reclaimed=%v reason=%s",
			ReasonInUse, res[0].Reclaimed, res[0].Reason)
	}
	t.Logf("input: now 超过 expires, activeSessions=1; 判定: reason=%s 不回收（正在使用）", ReasonInUse)

	// 会话刚结束：处于“刚激活宽限”窗口，仍不回收。
	if err := m.EndSession(ctx, env.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	res, _ = m.ReclaimExpired(ctx, clk.now())
	if res[0].Reclaimed || res[0].Reason != ReasonRecentlyActive {
		t.Fatalf("宽限窗口内应得到 %s, got %v/%s", ReasonRecentlyActive, res[0].Reclaimed, res[0].Reason)
	}
	t.Logf("input: 会话刚结束, now-lastAccess=0 < grace=15s; 判定: reason=%s 不误回收", ReasonRecentlyActive)

	// 心跳（重新激活）会把到期时间顺延：推进 50 分钟后 touch，再过 20 分钟也不应回收。
	clk.advance(50 * time.Minute)
	if _, err := m.Touch(ctx, env.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	clk.advance(20 * time.Minute) // 距上次心跳 20 分钟，TTL 1h 未到
	res, _ = m.ReclaimExpired(ctx, clk.now())
	if res[0].Reclaimed || res[0].Reason != ReasonStillActive {
		t.Fatalf("心跳后未到期应得到 %s, got %v/%s", ReasonStillActive, res[0].Reclaimed, res[0].Reason)
	}
	t.Logf("input: touch 后 20m < TTL=1h; 判定: reason=%s（刚被重新激活/使用的环境不回收）", ReasonStillActive)

	// 超过 TTL 且无会话、超出宽限：回收。
	clk.advance(time.Hour)
	res, _ = m.ReclaimExpired(ctx, clk.now())
	if !res[0].Reclaimed || res[0].Reason != ReasonExpired {
		t.Fatalf("空闲到期应回收且原因=%s, got %v/%s", ReasonExpired, res[0].Reclaimed, res[0].Reason)
	}
	t.Logf("input: 距上次心跳 80m > TTL 且无会话; 判定: reclaimed=true reason=%s", res[0].Reason)
	assertStoreConsistent(t, m, store)
}

func TestReclaim_DuplicateIsIdempotent(t *testing.T) {
	m, store, clk := newTestManager(t)
	m.SetQuota("alice", Quota{MaxEnvironments: 5, MaxUnits: 50})
	ctx := context.Background()

	env, _ := m.Create(ctx, CreateRequest{Name: "r-2", Owner: "alice", Units: 2})
	clk.advance(time.Hour) // 保留早已超时

	// 尚未回收时领取：环境还在，但保留已超时，得到可区分的领取失败原因。
	if _, err := m.Claim(ctx, env.ID, "alice"); err == nil || ReasonOf(err) != ReasonClaimExpired {
		t.Fatalf("超时领取应得到 %s, got %v", ReasonClaimExpired, err)
	}
	t.Logf("input: 领取已超时但尚未回收的 r-2; 判定: reason=%s（领取失败原因可区分）", ReasonClaimExpired)

	// 连续两轮回收：第一轮回收，第二轮对已销毁环境给出 not_found，不报错。
	r1, err := m.ReclaimExpired(ctx, clk.now())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := m.ReclaimExpired(ctx, clk.now())
	if err != nil {
		t.Fatalf("重复回收不应报错: %v", err)
	}
	if !r1[0].Reclaimed || r2[0].Reclaimed || r2[0].Reason != ReasonNotFound {
		t.Fatalf("重复回收应幂等: r1=%+v r2=%+v", r1[0], r2[0])
	}
	t.Logf("input: 对 r-2 连续两次 ReclaimExpired; 判定: 第一次 reason=%s 回收, 第二次 reason=%s 幂等",
		r1[0].Reason, r2[0].Reason)
	if store.OccupiedUnits() != 0 {
		t.Fatalf("重复回收后占用应为 0, got %d", store.OccupiedUnits())
	}

	// 已回收环境再领取得到 not_found，生命周期结论明确、无歧义。
	if _, err := m.Claim(ctx, env.ID, "alice"); err == nil || ReasonOf(err) != ReasonNotFound {
		t.Fatalf("回收后领取应得到 %s, got %v", ReasonNotFound, err)
	}
	t.Logf("input: 回收后再领取 r-2; 判定: reason=%s（终态明确）", ReasonNotFound)
	assertStoreConsistent(t, m, store)
}
