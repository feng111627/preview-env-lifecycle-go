package preview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestManagerCap(maxPerUser, maxGlobal int, ttl time.Duration) (*Manager, *LocalProvisioner) {
	prov := NewLocalProvisioner()
	return NewManager(Quota{MaxEnvsPerUser: maxPerUser, MaxGlobalEnvs: maxGlobal}, ttl, prov), prov
}

func TestGlobalCapacityExhausted(t *testing.T) {
	m, _ := newTestManagerCap(5, 2, time.Hour)
	if _, err := m.Create(context.Background(), "env-g1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), "env-g2", "bob"); err != nil {
		t.Fatal(err)
	}
	// carol 个人额度充足，但全局总盘子已满。
	_, err := m.Create(context.Background(), "env-g3", "carol")
	t.Logf("input: Create(env-g3, carol) with MaxGlobalEnvs=2 already full, carol usage=0/%d", 5)
	t.Logf("decision: err=%v (IsCapacity=%v IsQuota=%v IsPermission=%v)", err,
		errors.Is(err, ErrCapacityExhausted), errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrPermissionDenied))
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("expected ErrCapacityExhausted, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("capacity failure must not masquerade as quota/permission: %v", err)
	}
	// 反向：个人额度错误也不能冒充全局容量错误。
	m2, _ := newTestManagerCap(1, 100, time.Hour)
	if _, err := m2.Create(context.Background(), "env-q1", "dave"); err != nil {
		t.Fatal(err)
	}
	_, err = m2.Create(context.Background(), "env-q2", "dave")
	t.Logf("input: second Create by dave with MaxEnvsPerUser=1, MaxGlobalEnvs=100")
	t.Logf("decision: err=%v (IsQuota=%v IsCapacity=%v)", err,
		errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrCapacityExhausted))
	if !errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("quota failure must stay distinct from capacity failure: %v", err)
	}
}

func TestReleaseThenAcquireReusesIdleEnv(t *testing.T) {
	m, prov := newTestManagerCap(5, 10, time.Hour)
	if _, err := m.Create(context.Background(), "env-shared", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Bind("env-shared", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-shared", "alice"); err != nil {
		t.Fatal(err)
	}
	t.Logf("input: alice created+bound+released env-shared; alice usage after release=%d", m.Usage("alice"))
	if got := m.Usage("alice"); got != 0 {
		t.Fatalf("pooled env must not count against alice, usage=%d", got)
	}
	before := prov.NamespaceCount()
	env, err := m.Acquire(context.Background(), "bob")
	t.Logf("input: Acquire(bob) with one pooled idle env")
	t.Logf("decision: got env=%s owner=%s boundTo=%s status=%s sanitized=%d namespaces=%d (before=%d)",
		env.Name, env.Owner, env.BoundTo, env.Status, prov.SanitizeCount("env-shared"), prov.NamespaceCount(), before)
	if err != nil {
		t.Fatal(err)
	}
	if env.Name != "env-shared" {
		t.Fatalf("expected reuse of env-shared, got %q", env.Name)
	}
	if prov.SanitizeCount("env-shared") != 1 {
		t.Fatalf("env must be sanitized before handover, got %d", prov.SanitizeCount("env-shared"))
	}
	if env.Owner != "bob" || env.BoundTo != "bob" || env.Status != StatusInUse {
		t.Fatalf("occupation must transfer to bob: %+v", env)
	}
	if got := m.Usage("bob"); got != 1 {
		t.Fatalf("bob usage expected 1, got %d", got)
	}
	if got := m.Usage("alice"); got != 0 {
		t.Fatalf("alice usage expected 0 after handover, got %d", got)
	}
	if got := prov.NamespaceCount(); got != before {
		t.Fatalf("reuse must not create new resource: before=%d after=%d", before, got)
	}
	if got := m.ActiveCount(); got != 1 {
		t.Fatalf("global active count must stay 1, got %d", got)
	}
	// 复用不触发全局容量检查：把上限压到当前值也能复用。
	m2, _ := newTestManagerCap(5, 1, time.Hour)
	if _, err := m2.Create(context.Background(), "env-cap1", "carol"); err != nil {
		t.Fatal(err)
	}
	if err := m2.Release("env-cap1", "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Acquire(context.Background(), "dave"); err != nil {
		t.Fatalf("reuse at full global capacity must succeed (count unchanged): %v", err)
	}
	t.Logf("input: Acquire(dave) with MaxGlobalEnvs=1 and one pooled env")
	t.Logf("decision: reuse succeeded, active=%d (cap=1, no inflation)", m2.ActiveCount())
}

func TestReleasePermissionAndIdempotency(t *testing.T) {
	m, _ := newTestManagerCap(5, 10, time.Hour)
	if _, err := m.Create(context.Background(), "env-r", "erin"); err != nil {
		t.Fatal(err)
	}
	err := m.Release("env-r", "mallory")
	t.Logf("input: Release(env-r) by non-owner mallory")
	t.Logf("decision: err=%v (IsPermission=%v)", err, errors.Is(err, ErrPermissionDenied))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got %v", err)
	}
	if err := m.Release("env-r", "erin"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-r", "erin"); err != nil { // 幂等
		t.Fatalf("repeat release must be no-op: %v", err)
	}
	if got := m.ActiveCount(); got != 1 {
		t.Fatalf("double release must not change counts, active=%d", got)
	}
}

func TestConcurrentAcquireNoOversell(t *testing.T) {
	m, prov := newTestManagerCap(1, 2, time.Hour)
	// 两个环境释放进池，全局容量封顶 2，新建不被允许。
	for _, name := range []string{"env-p1", "env-p2"} {
		if _, err := m.Create(context.Background(), name, "owner"); err != nil {
			t.Fatal(err)
		}
		if err := m.Release(name, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	const n = 10
	var wg sync.WaitGroup
	envs := make([]*Env, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			envs[i], errs[i] = m.Acquire(context.Background(), fmt.Sprintf("user-%d", i))
		}(i)
	}
	wg.Wait()
	seen := map[string]int{}
	var wins, capErrs int
	for i := range errs {
		if errs[i] == nil {
			wins++
			seen[envs[i].Name]++
		} else if errors.Is(errs[i], ErrCapacityExhausted) {
			capErrs++
		} else {
			t.Fatalf("unexpected error from goroutine %d: %v", i, errs[i])
		}
	}
	t.Logf("input: %d concurrent Acquire, pool=2, MaxGlobalEnvs=2", n)
	t.Logf("decision: wins=%d capacityRejected=%d envs=%v active=%d namespaces=%d",
		wins, capErrs, seen, m.ActiveCount(), prov.NamespaceCount())
	if wins != 2 || capErrs != n-2 {
		t.Fatalf("expected exactly 2 wins and %d capacity rejections, got wins=%d capErrs=%d", n-2, wins, capErrs)
	}
	for name, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("env %s handed to %d users (must be exactly 1)", name, cnt)
		}
	}
	if got := m.ActiveCount(); got != 2 {
		t.Fatalf("active count must be 2, got %d", got)
	}
	if got := prov.NamespaceCount(); got != 2 {
		t.Fatalf("namespaces must be 2, got %d", got)
	}
}

func TestReuseSanitizeFailureCompensates(t *testing.T) {
	m, prov := newTestManagerCap(5, 10, time.Hour)
	if _, err := m.Create(context.Background(), "env-flaky", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-flaky", "alice"); err != nil {
		t.Fatal(err)
	}
	prov.FailSanitizeOn = map[string]error{"env-flaky": ErrLocalProvision}
	_, err := m.Acquire(context.Background(), "bob")
	t.Logf("input: Acquire(bob) with injected sanitize failure on env-flaky")
	t.Logf("decision: err=%v bobUsage=%d active=%d namespaces=%d",
		err, m.Usage("bob"), m.ActiveCount(), prov.NamespaceCount())
	if !errors.Is(err, ErrReuseFailed) {
		t.Fatalf("expected ErrReuseFailed, got %v", err)
	}
	if got := m.Usage("bob"); got != 0 {
		t.Fatalf("failed reuse must not charge bob, usage=%d", got)
	}
	// 补偿后环境应回到池中，占用不丢不重复。
	env, ok := m.Get("env-flaky")
	if !ok || !env.Pooled || env.Owner != "alice" || env.Status != StatusReady || env.BoundTo != "" {
		t.Fatalf("env must be compensated back to pool: %+v", env)
	}
	if got := m.ActiveCount(); got != 1 {
		t.Fatalf("active count must stay 1 after compensation, got %d", got)
	}
	if got := prov.NamespaceCount(); got != 1 {
		t.Fatalf("namespace must not leak after compensation, got %d", got)
	}
	// 清除故障后可再次复用同一环境。
	delete(prov.FailSanitizeOn, "env-flaky")
	env2, err := m.Acquire(context.Background(), "bob")
	if err != nil {
		t.Fatalf("retry after clearing failure: %v", err)
	}
	t.Logf("input: retry Acquire(bob) after clearing injected failure")
	t.Logf("decision: got env=%s owner=%s active=%d namespaces=%d",
		env2.Name, env2.Owner, m.ActiveCount(), prov.NamespaceCount())
	if env2.Name != "env-flaky" || env2.Owner != "bob" {
		t.Fatalf("expected reuse of env-flaky by bob, got %+v", env2)
	}
	if got := prov.NamespaceCount(); got != 1 {
		t.Fatalf("ledger vs resources mismatch: namespaces=%d", got)
	}
}

func TestPooledEnvReapedWhenExpired(t *testing.T) {
	m, prov := newTestManagerCap(5, 10, 50*time.Millisecond)
	past := time.Now().Add(-time.Hour)
	m.now = func() time.Time { return past }
	if _, err := m.Create(context.Background(), "env-stale", "gina"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-stale", "gina"); err != nil {
		t.Fatal(err)
	}
	m.now = time.Now // 已到期
	reaped, err := m.Reap(context.Background())
	t.Logf("input: Reap with expired pooled env-stale")
	t.Logf("decision: reaped=%v active=%d namespaces=%d", reaped, m.ActiveCount(), prov.NamespaceCount())
	if err != nil || len(reaped) != 1 || reaped[0] != "env-stale" {
		t.Fatalf("expected pooled expired env reaped, got %v err=%v", reaped, err)
	}
	// 池已空，下一次 Acquire 走新建，账实一致。
	env, err := m.Acquire(context.Background(), "henry")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: Acquire(henry) after pooled env reaped")
	t.Logf("decision: new env=%s active=%d namespaces=%d", env.Name, m.ActiveCount(), prov.NamespaceCount())
	if env.Name == "env-stale" {
		t.Fatal("reaped env must not be reused")
	}
	if m.ActiveCount() != prov.NamespaceCount() {
		t.Fatalf("ledger vs resources mismatch: active=%d namespaces=%d", m.ActiveCount(), prov.NamespaceCount())
	}
}

func TestInUseEnvNotReusable(t *testing.T) {
	m, _ := newTestManagerCap(5, 10, time.Hour)
	if _, err := m.Create(context.Background(), "env-busy", "ivan"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Bind("env-busy", "ivan"); err != nil { // InUse，未释放
		t.Fatal(err)
	}
	env, err := m.Acquire(context.Background(), "judy")
	t.Logf("input: Acquire(judy) while env-busy is InUse (not released)")
	t.Logf("decision: got env=%s (must be a new one)", env.Name)
	if err != nil {
		t.Fatal(err)
	}
	if env.Name == "env-busy" {
		t.Fatal("in-use env must never be taken for reuse")
	}
	if got := m.Usage("ivan"); got != 1 {
		t.Fatalf("ivan usage must stay 1, got %d", got)
	}
}

func TestConcurrentMixedOpsFinalConsistency(t *testing.T) {
	m, prov := newTestManagerCap(3, 6, 20*time.Millisecond)
	var wg sync.WaitGroup
	// 8 个用户并发地 申请/释放/销毁，后台同时回收。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", i)
			for j := 0; j < 20; j++ {
				env, err := m.Acquire(context.Background(), user)
				if err != nil {
					continue // 容量/配额拒绝是合法结果
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
	// 等回收循环退出后再做最终对账（先等工作者，再单独等一次回收）。
	_, _ = m.Reap(context.Background())
	time.Sleep(30 * time.Millisecond) // 让所有环境都过期
	_, _ = m.Reap(context.Background())
	active := m.ActiveCount()
	ns := prov.NamespaceCount()
	t.Logf("input: 8 users x 20 rounds of Acquire/Release/Destroy/Touch with concurrent Reap")
	t.Logf("decision: after final reap active=%d namespaces=%d (ledger must equal real resources)", active, ns)
	if active != ns {
		t.Fatalf("ledger vs real resources mismatch: active=%d namespaces=%d", active, ns)
	}
	// 注：剩下的都是 InUse 环境，受回收保护是既定语义；此处核心断言是账实一致。
}
