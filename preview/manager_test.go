package preview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestManager(maxPerUser int, ttl time.Duration) (*Manager, *LocalProvisioner) {
	prov := NewLocalProvisioner()
	return NewManager(Quota{MaxEnvsPerUser: maxPerUser}, ttl, prov), prov
}

func TestConcurrentCreateSameName(t *testing.T) {
	m, prov := newTestManager(10, time.Hour)
	const n = 16
	var wg sync.WaitGroup
	results := make([]*Env, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = m.Create(context.Background(), "env-a", "alice")
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d got unexpected error: %v", i, errs[i])
		}
	}
	t.Logf("input: %d concurrent Create(name=env-a, owner=alice)", n)
	t.Logf("decision: usage=%d namespaces=%d (expect both 1, idempotent dedup)", m.Usage("alice"), prov.NamespaceCount())
	if got := m.Usage("alice"); got != 1 {
		t.Fatalf("expected usage 1, got %d", got)
	}
	if got := prov.NamespaceCount(); got != 1 {
		t.Fatalf("expected 1 namespace, got %d (leak or duplicate)", got)
	}
}

func TestCreateFailureRollsBack(t *testing.T) {
	m, prov := newTestManager(10, time.Hour)
	prov.FailOn = map[string]error{"env-bad": ErrLocalProvision}
	_, err := m.Create(context.Background(), "env-bad", "bob")
	t.Logf("input: Create(env-bad) with injected failure %v", ErrLocalProvision)
	t.Logf("decision: err=%v usage=%d namespaces=%d", err, m.Usage("bob"), prov.NamespaceCount())
	if !errors.Is(err, ErrProvisionFailed) {
		t.Fatalf("expected ErrProvisionFailed, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("failure reason must be distinguishable, got %v", err)
	}
	if got := m.Usage("bob"); got != 0 {
		t.Fatalf("occupation not rolled back, usage=%d", got)
	}
	if got := prov.NamespaceCount(); got != 0 {
		t.Fatalf("namespace leaked after rollback: %d", got)
	}
	// 回滚后同名可重建（清除注入的故障）。
	delete(prov.FailOn, "env-bad")
	if _, err := m.Create(context.Background(), "env-bad", "bob"); err != nil {
		t.Fatalf("recreate after rollback failed: %v", err)
	}
}

func TestQuotaExceededDistinguishable(t *testing.T) {
	m, _ := newTestManager(1, time.Hour)
	if _, err := m.Create(context.Background(), "env-1", "carol"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Create(context.Background(), "env-2", "carol")
	t.Logf("input: second Create by carol with MaxEnvsPerUser=1")
	t.Logf("decision: err=%v (IsQuota=%v IsPermission=%v)", err,
		errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrPermissionDenied))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
	if errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("quota failure must not look like permission failure: %v", err)
	}
}

func TestPermissionDeniedDistinguishable(t *testing.T) {
	m, _ := newTestManager(10, time.Hour)
	if _, err := m.Create(context.Background(), "env-x", "dave"); err != nil {
		t.Fatal(err)
	}
	err := m.Destroy(context.Background(), "env-x", "mallory")
	t.Logf("input: Destroy(env-x) by non-owner mallory")
	t.Logf("decision: err=%v (IsPermission=%v IsQuota=%v)", err,
		errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrQuotaExceeded))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("permission failure must not look like quota failure: %v", err)
	}
	if _, err := m.Bind("env-x", "mallory"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("bind by non-owner: expected ErrPermissionDenied, got %v", err)
	}
}

func TestBindAndQuotaOnClaim(t *testing.T) {
	m, _ := newTestManager(1, time.Hour)
	if _, err := m.Create(context.Background(), "env-b1", "erin"); err != nil {
		t.Fatal(err)
	}
	env, err := m.Bind("env-b1", "erin")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: Bind(env-b1, erin) after create")
	t.Logf("decision: status=%s boundTo=%s", env.Status, env.BoundTo)
	if env.Status != StatusInUse || env.BoundTo != "erin" {
		t.Fatalf("unexpected bind result: %+v", env)
	}
	// 已绑定给他人的环境，第三人不能抢。
	if _, err := m.Bind("env-b1", "frank"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied for third party, got %v", err)
	}
}

func TestReapExpiredOnly(t *testing.T) {
	m, prov := newTestManager(10, 50*time.Millisecond)
	past := time.Now().Add(-time.Hour)
	m.now = func() time.Time { return past }
	if _, err := m.Create(context.Background(), "env-old", "gina"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), "env-fresh", "gina"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Bind("env-fresh", "gina"); err != nil { // InUse，不回收
		t.Fatal(err)
	}
	m.now = time.Now // env-old 到期，env-fresh 刚被激活续期
	reaped, err := m.Reap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: Reap with env-old expired(ready) and env-fresh in-use+renewed")
	t.Logf("decision: reaped=%v remainingNamespaces=%d", reaped, prov.NamespaceCount())
	if len(reaped) != 1 || reaped[0] != "env-old" {
		t.Fatalf("expected only env-old reaped, got %v", reaped)
	}
	if _, ok := m.Get("env-fresh"); !ok {
		t.Fatal("in-use env must not be reaped")
	}
	// 重复回收幂等：第二次不应再回收任何东西。
	reaped2, _ := m.Reap(context.Background())
	t.Logf("input: second Reap (idempotency check)")
	t.Logf("decision: reaped=%v namespaces=%d", reaped2, prov.NamespaceCount())
	if len(reaped2) != 0 {
		t.Fatalf("repeat reap must be no-op, got %v", reaped2)
	}
	if got := prov.NamespaceCount(); got != 1 {
		t.Fatalf("expected 1 namespace left, got %d", got)
	}
}

func TestReapThenRecreateNoLeak(t *testing.T) {
	m, prov := newTestManager(10, time.Millisecond)
	if _, err := m.Create(context.Background(), "env-rc", "henry"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 回收后重建同名环境，不能出现重复占用或泄漏。
	if _, err := m.Create(context.Background(), "env-rc", "henry"); err != nil {
		t.Fatalf("recreate after reap failed: %v", err)
	}
	t.Logf("input: reap env-rc then recreate same name")
	t.Logf("decision: usage=%d namespaces=%d (expect 1/1)", m.Usage("henry"), prov.NamespaceCount())
	if got := m.Usage("henry"); got != 1 {
		t.Fatalf("expected usage 1, got %d", got)
	}
	if got := prov.NamespaceCount(); got != 1 {
		t.Fatalf("expected 1 namespace, got %d", got)
	}
}

func TestConcurrentCreateDistinctNames(t *testing.T) {
	m, prov := newTestManager(100, time.Hour)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("env-%d", i)
			if _, err := m.Create(context.Background(), name, "ivan"); err != nil {
				t.Errorf("create %s: %v", name, err)
			}
		}(i)
	}
	wg.Wait()
	t.Logf("input: %d concurrent Create with distinct names, owner=ivan", n)
	t.Logf("decision: usage=%d namespaces=%d", m.Usage("ivan"), prov.NamespaceCount())
	if got := m.Usage("ivan"); got != n {
		t.Fatalf("expected usage %d, got %d", n, got)
	}
	if got := prov.NamespaceCount(); got != n {
		t.Fatalf("expected %d namespaces, got %d", n, got)
	}
}

func TestDestroyReleasesQuota(t *testing.T) {
	m, prov := newTestManager(1, time.Hour)
	if _, err := m.Create(context.Background(), "env-d", "judy"); err != nil {
		t.Fatal(err)
	}
	if err := m.Destroy(context.Background(), "env-d", "judy"); err != nil {
		t.Fatal(err)
	}
	t.Logf("input: Destroy(env-d) by owner judy, quota=1")
	t.Logf("decision: usage=%d namespaces=%d", m.Usage("judy"), prov.NamespaceCount())
	if got := m.Usage("judy"); got != 0 {
		t.Fatalf("usage not released: %d", got)
	}
	if _, err := m.Create(context.Background(), "env-d2", "judy"); err != nil {
		t.Fatalf("quota should be free after destroy: %v", err)
	}
}
