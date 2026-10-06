package preview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// snapshot 仅测试辅助：取环境快照，便于日志。
func snapshot(m *Manager, name string) Env {
	e, ok := m.Get(name)
	if !ok {
		return Env{Name: name, Status: "<missing>"}
	}
	return *e
}

// assertLedgerConsistent 账实对账（仅在无在途操作的静止点调用）：
//  1. 不允许“池内空闲 + 使用中”的幽灵状态；
//  2. 账面活跃数 == 底层真实命名空间数；
//  3. 按个人 Usage 求和 + 池内空闲数 == 全局活跃数（每个活跃环境要么算到某人名下，要么在池子里）。
func assertLedgerConsistent(t *testing.T, m *Manager, prov *LocalProvisioner, where string) {
	t.Helper()
	m.mu.Lock()
	var pooled, occupied int
	for _, e := range m.envs {
		if !active(e.Status) {
			continue
		}
		if e.Pooled {
			pooled++
			if e.Status != StatusReady || e.BoundTo != "" {
				t.Fatalf("[%s] ghost pooled env: %+v", where, e)
			}
			if e.Status == StatusInUse {
				t.Fatalf("[%s] pooled env must never be in_use: %+v", where, e)
			}
		} else {
			occupied++
		}
	}
	ledger := len(m.envs)
	m.mu.Unlock()

	active := m.ActiveCount()
	ns := prov.NamespaceCount()
	if active != ns {
		t.Fatalf("[%s] ledger vs real resources mismatch: active=%d namespaces=%d", where, active, ns)
	}
	if active != ledger {
		t.Fatalf("[%s] ActiveCount=%d but map holds %d", where, active, ledger)
	}
	if got := pooled + occupied; got != active {
		t.Fatalf("[%s] pooled(%d)+occupied(%d) != active(%d): an env is claimed by nobody", where, pooled, occupied, active)
	}
	t.Logf("ledger[%s]: active=%d namespaces=%d pooled=%d occupied=%d (consistent)", where, active, ns, pooled, occupied)
}

// TestReleaseThenReclaimByOwner 复现：创建 → 交回共享池 → 原主直接 Bind 拿回自用。
func TestReleaseThenReclaimByOwner(t *testing.T) {
	m, prov := newTestManagerCap(5, 10, time.Hour)
	if _, err := m.Create(context.Background(), "env-back", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-back", "alice"); err != nil {
		t.Fatal(err)
	}
	t.Logf("input: Create(env-back,alice) then Release; pooled state: %+v", snapshot(m, "env-back"))

	// 时间前进 10 分钟，验证拿回会像正常投入使用一样续期。
	m.now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	env, err := m.Bind("env-back", "alice")
	if err != nil {
		t.Fatalf("owner reclaim own pooled env: %v", err)
	}
	t.Logf("input: Bind(env-back, alice) directly after pooling")
	t.Logf("decision: pooled=%v status=%s owner=%s boundTo=%s usage(alice)=%d active=%d namespaces=%d expiresIn=%v",
		env.Pooled, env.Status, env.Owner, env.BoundTo,
		m.Usage("alice"), m.ActiveCount(), prov.NamespaceCount(), time.Until(env.ExpiresAt).Round(time.Minute))

	if env.Pooled {
		t.Fatalf("reclaimed env must not keep pooled idle flag: %+v", env)
	}
	if env.Status != StatusInUse || env.BoundTo != "alice" {
		t.Fatalf("reclaimed env must look like a normally acquired env: %+v", env)
	}
	if got := m.Usage("alice"); got != 1 {
		t.Fatalf("reclaimed env must count against alice, usage=%d", got)
	}
	if got := m.ActiveCount(); got != 1 {
		t.Fatalf("global active must stay 1, got %d", got)
	}
	if d := time.Until(env.ExpiresAt); d < 50*time.Minute {
		t.Fatalf("reclaim must renew ttl like normal use, expires in %v", d)
	}
	assertLedgerConsistent(t, m, prov, "after reclaim")

	// 拿回后与正常申请到的环境一致：可再次交回池子，再被别人复用。
	if err := m.Release("env-back", "alice"); err != nil {
		t.Fatal(err)
	}
	t.Logf("input: re-release reclaimed env; state: %+v", snapshot(m, "env-back"))
	assertLedgerConsistent(t, m, prov, "after re-release")
	got, err := m.Acquire(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "env-back" || got.Owner != "bob" || got.BoundTo != "bob" {
		t.Fatalf("reclaimed-then-released env must be reusable by bob: %+v", got)
	}
	t.Logf("input: Acquire(bob) after re-release; decision: %s -> owner=%s boundTo=%s sanitized=%d",
		got.Name, got.Owner, got.BoundTo, prov.SanitizeCount("env-back"))
	if m.Usage("alice") != 0 || m.Usage("bob") != 1 {
		t.Fatalf("occupation mismatch: alice=%d bob=%d", m.Usage("alice"), m.Usage("bob"))
	}
	assertLedgerConsistent(t, m, prov, "after reuse")
}

// TestReclaimQuotaRejection 原主额度用满时拿回被明确拒绝，环境留在池中不丢不重，
// 拒绝原因是个人额度，不能冒充容量或权限。
func TestReclaimQuotaRejection(t *testing.T) {
	m, prov := newTestManagerCap(1, 10, time.Hour)
	if _, err := m.Create(context.Background(), "env-pool", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-pool", "alice"); err != nil {
		t.Fatal(err)
	}
	// alice 再占一个新环境，把个人额度用满。
	if _, err := m.Create(context.Background(), "env-other", "alice"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Bind("env-pool", "alice")
	t.Logf("input: Bind(env-pool, alice) with alice quota=1 full, env-pool pooled")
	t.Logf("decision: err=%v (IsQuota=%v IsCapacity=%v IsPermission=%v) active=%d namespaces=%d",
		err, errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrCapacityExhausted),
		errors.Is(err, ErrPermissionDenied), m.ActiveCount(), prov.NamespaceCount())
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
	if errors.Is(err, ErrCapacityExhausted) || errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("quota rejection must not masquerade: %v", err)
	}
	if got := m.Usage("alice"); got != 1 {
		t.Fatalf("rejected reclaim must not charge alice, usage=%d", got)
	}
	env, ok := m.Get("env-pool")
	if !ok || !env.Pooled || env.Status != StatusReady || env.BoundTo != "" {
		t.Fatalf("rejected reclaim must leave env untouched in pool: %+v", env)
	}
	assertLedgerConsistent(t, m, prov, "after quota rejection")
	// 仍在全局占一个名额但池不空：别人可正常复用，名额不浪费。
	got, err := m.Acquire(context.Background(), "carol")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "env-pool" {
		t.Fatalf("pooled env must stay reusable after owner hit quota, got %s", got.Name)
	}
	t.Logf("input: Acquire(carol); decision: reused %s, active=%d namespaces=%d", got.Name, m.ActiveCount(), prov.NamespaceCount())
	assertLedgerConsistent(t, m, prov, "after reuse by carol")
}

// TestReclaimPermissionRejection 池内环境不能被非原主直接 Bind 拿走，
// 必须走 Acquire；拒绝后池子不变、仍可被复用。
func TestReclaimPermissionRejection(t *testing.T) {
	m, prov := newTestManagerCap(5, 10, time.Hour)
	if _, err := m.Create(context.Background(), "env-sec", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-sec", "alice"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Bind("env-sec", "mallory")
	t.Logf("input: Bind(env-sec, mallory) on alice's pooled env")
	t.Logf("decision: err=%v (IsPermission=%v IsQuota=%v)", err,
		errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrQuotaExceeded))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("permission rejection must not masquerade: %v", err)
	}
	env, _ := m.Get("env-sec")
	if !env.Pooled || env.Owner != "alice" || env.BoundTo != "" {
		t.Fatalf("rejected bind must leave pooled env untouched: %+v", env)
	}
	assertLedgerConsistent(t, m, prov, "after permission rejection")
	// 正规路径 Acquire 仍可复用。
	got, err := m.Acquire(context.Background(), "bob")
	if err != nil || got.Name != "env-sec" || got.Owner != "bob" {
		t.Fatalf("Acquire reuse after rejected bind: env=%+v err=%v", got, err)
	}
	t.Logf("input: Acquire(bob); decision: reused env-sec owner=%s active=%d", got.Owner, m.ActiveCount())
	assertLedgerConsistent(t, m, prov, "after proper reuse")
}

// TestReclaimThenReleaseThenReap 拿回自用 → 再次交回 → 到期可被回收，账实一致。
func TestReclaimThenReleaseThenReap(t *testing.T) {
	m, prov := newTestManagerCap(5, 10, 50*time.Millisecond)
	past := time.Now().Add(-time.Hour)
	m.now = func() time.Time { return past }
	if _, err := m.Create(context.Background(), "env-cycle", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-cycle", "alice"); err != nil {
		t.Fatal(err)
	}
	// 旧实现下这里会产生“池内+使用中”的幽灵：既不被回收也不能被复用。
	if _, err := m.Bind("env-cycle", "alice"); err != nil {
		t.Fatal(err)
	}
	assertLedgerConsistent(t, m, prov, "reclaimed (would-be ghost under old code)")
	// 在使用中即使到期也受保护（既定语义）。
	m.now = time.Now
	if reaped, _ := m.Reap(context.Background()); len(reaped) != 0 {
		t.Fatalf("in-use reclaimed env must be protected, reaped=%v", reaped)
	}
	t.Logf("input: Reap while reclaimed env is in_use+expired; decision: protected, active=%d", m.ActiveCount())
	// 再次空闲交回后，到期即可被正常回收，名额释放。
	if err := m.Release("env-cycle", "alice"); err != nil {
		t.Fatal(err)
	}
	reaped, err := m.Reap(context.Background())
	t.Logf("input: release then Reap; decision: reaped=%v active=%d namespaces=%d",
		reaped, m.ActiveCount(), prov.NamespaceCount())
	if err != nil || len(reaped) != 1 || reaped[0] != "env-cycle" {
		t.Fatalf("expected env-cycle reaped, got %v err=%v", reaped, err)
	}
	if m.ActiveCount() != 0 || prov.NamespaceCount() != 0 {
		t.Fatalf("global slot must be fully released: active=%d namespaces=%d", m.ActiveCount(), prov.NamespaceCount())
	}
	assertLedgerConsistent(t, m, prov, "after final reap")
}

// TestReclaimRacesAcquireNoDoubleHandout 原主直接拿回 与 他人 Acquire 复用并发：
// 同一个环境只能有一个赢家，其余人要么容量拒绝要么权限拒绝，账实一致。
func TestReclaimRacesAcquireNoDoubleHandout(t *testing.T) {
	m, prov := newTestManagerCap(5, 1, time.Hour) // 全局上限 1：池空后新建一律容量拒绝
	if _, err := m.Create(context.Background(), "env-race", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-race", "alice"); err != nil {
		t.Fatal(err)
	}

	const rivals = 12
	var wg sync.WaitGroup
	mu := sync.Mutex{}
	winners := map[string]string{} // env -> winner
	var capErrs, permErrs int
	win := func(who, via string, env *Env) {
		mu.Lock()
		winners[env.Name] = who + "/" + via
		mu.Unlock()
	}

	wg.Add(1)
	go func() { // 原主直接拿回
		defer wg.Done()
		env, err := m.Bind("env-race", "alice")
		if err == nil {
			win("alice", "bind", env)
		} else if !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("alice bind unexpected err: %v", err)
		} else {
			mu.Lock()
			permErrs++
			mu.Unlock()
		}
	}()
	for i := 0; i < rivals; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", i)
			env, err := m.Acquire(context.Background(), user)
			switch {
			case err == nil:
				win(user, "acquire", env)
			case errors.Is(err, ErrCapacityExhausted):
				mu.Lock()
				capErrs++
				mu.Unlock()
			default:
				t.Errorf("acquirer %d unexpected err: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	t.Logf("input: 1 owner-bind vs %d concurrent Acquire, pool=1 MaxGlobalEnvs=1", rivals)
	t.Logf("decision: winners=%v capacityRejected=%d bindPermissionRejected=%d active=%d namespaces=%d",
		winners, capErrs, permErrs, m.ActiveCount(), prov.NamespaceCount())
	if len(winners) != 1 {
		t.Fatalf("exactly one winner must get env-race, got %v", winners)
	}
	if capErrs+permErrs != rivals {
		t.Fatalf("all %d losers must get a clean rejection, got cap=%d perm=%d", rivals, capErrs, permErrs)
	}
	if m.ActiveCount() != 1 || prov.NamespaceCount() != 1 {
		t.Fatalf("exactly one env must exist: active=%d namespaces=%d", m.ActiveCount(), prov.NamespaceCount())
	}
	assertLedgerConsistent(t, m, prov, "after reclaim/acquire race")
}

// TestReclaimReuseReapMixedConsistency 原主反复“交回/拿回”与他人复用、后台回收交叉，
// 全程不允许出现无人认领又回收不掉的环境，最终账实一致。
func TestReclaimReuseReapMixedConsistency(t *testing.T) {
	m, prov := newTestManagerCap(4, 8, 20*time.Millisecond)
	var wg sync.WaitGroup

	// alice 反复：申请 → 交回 → 直接拿回 → 交回；拿不到时（被别人复用走）走正规 Acquire。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 30; j++ {
			env, err := m.Acquire(context.Background(), "alice")
			if err != nil {
				continue
			}
			if err := m.Release(env.Name, "alice"); err != nil {
				continue
			}
			if j%2 == 0 {
				if _, err := m.Bind(env.Name, "alice"); err == nil {
					_ = m.Release(env.Name, "alice") // 拿回后再交回
				}
			}
		}
	}()
	// 其他用户只做正常 Acquire / Release / Destroy。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", i)
			for j := 0; j < 20; j++ {
				env, err := m.Acquire(context.Background(), user)
				if err != nil {
					continue
				}
				switch j % 3 {
				case 0:
					_ = m.Release(env.Name, user)
				case 1:
					_ = m.Destroy(context.Background(), env.Name, user)
				default:
					_ = m.Touch(env.Name, user)
				}
			}
		}(i)
	}
	stop := make(chan struct{})
	var reaperWg sync.WaitGroup
	reaperWg.Add(1)
	go func() {
		defer reaperWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = m.Reap(context.Background())
				time.Sleep(time.Millisecond)
			}
		}
	}()
	wg.Wait()
	close(stop)
	reaperWg.Wait()
	_, _ = m.Reap(context.Background())
	time.Sleep(30 * time.Millisecond)
	_, _ = m.Reap(context.Background())
	t.Logf("input: mixed reclaim/release/acquire/destroy with concurrent reaper")
	assertLedgerConsistent(t, m, prov, "final after mixed ops")
}
