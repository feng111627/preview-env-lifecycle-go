package preview

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newCapTestManager(maxPerUser, maxActive int) (*Manager, *LocalProvisioner) {
	prov := NewLocalProvisioner()
	m := NewManagerWithConfig(
		Config{Quota: Quota{MaxEnvsPerUser: maxPerUser}, MaxActiveEnvs: maxActive},
		time.Hour, prov)
	return m, prov
}

// TestGlobalCapacityReached：全局盘子到顶后，新申请得到明确可辨的 ErrGlobalCapacity，
// 且不与个人额度、权限两类拒绝互相冒充。
func TestGlobalCapacityReached(t *testing.T) {
	m, prov := newCapTestManager(100, 2)
	ctx := context.Background()
	if _, err := m.Create(ctx, "g1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "g2", "bob"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Create(ctx, "g3", "carol") // 不同用户，个人额度都没满
	t.Logf("input: Create g3 with active=%d globalLimit=%d", m.ActiveCount(), 2)
	t.Logf("decision: err=%v (IsGlobal=%v IsQuota=%v IsPerm=%v)",
		err, errors.Is(err, ErrGlobalCapacity), errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrPermissionDenied))
	if !errors.Is(err, ErrGlobalCapacity) {
		t.Fatalf("expected ErrGlobalCapacity, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("global-capacity error must not masquerade as quota/permission: %v", err)
	}
	// 到顶不能静默丢、也不能虚增：账实都应停在 2。
	if got := m.ActiveCount(); got != 2 {
		t.Fatalf("active count must stay at cap 2, got %d", got)
	}
	if got := prov.NamespaceCount(); got != 2 {
		t.Fatalf("namespaces must stay at cap 2, got %d", got)
	}
}

// TestThreeRejectionsDistinct：权限、个人额度、全局容量三种拒绝在同一套配置下可清晰分辨。
func TestThreeRejectionsDistinct(t *testing.T) {
	m, _ := newCapTestManager(1, 1)
	ctx := context.Background()
	if _, err := m.Create(ctx, "p1", "alice"); err != nil {
		t.Fatal(err)
	}
	// 1) 权限：非所有者销毁。
	permErr := m.Destroy(ctx, "p1", "mallory")
	// 2) 个人额度：alice 已占 1，再申请。
	_, quotaErr := m.Acquire(ctx, "alice")
	// 3) 全局容量：bob 个人额度没满，但全局被 alice 占满（池空走新建分支）。
	_, globalErr := m.Acquire(ctx, "bob")
	t.Logf("input: cap=(perUser=1,global=1), one active env by alice")
	t.Logf("decision: perm=%v | quota=%v | global=%v", permErr, quotaErr, globalErr)
	for _, c := range []struct {
		name string
		err  error
		want error
	}{
		{"permission", permErr, ErrPermissionDenied},
		{"quota", quotaErr, ErrQuotaExceeded},
		{"global", globalErr, ErrGlobalCapacity},
	} {
		if !errors.Is(c.err, c.want) {
			t.Fatalf("%s rejection: want %v, got %v", c.name, c.want, c.err)
		}
	}
	if errors.Is(globalErr, ErrQuotaExceeded) || errors.Is(quotaErr, ErrGlobalCapacity) {
		t.Fatal("global-capacity and per-user quota errors must not overlap")
	}
}

// TestPoolReuseTransfersOwnership：空闲 ready 入池后被他人 Acquire 复用，
// 总数不增、占用过户、交接后无残留。
func TestPoolReuseTransfersOwnership(t *testing.T) {
	m, prov := newCapTestManager(10, 10)
	ctx := context.Background()
	if _, err := m.Create(ctx, "shared-1", "alice"); err != nil {
		t.Fatal(err)
	}
	prov.LeaveResidue("shared-1", "alice") // 上一任留下数据

	pooled, err := m.ReleaseToPool("shared-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: alice releases ready shared-1 to pool; aliceUsage=%d pool=%d active=%d",
		m.Usage("alice"), m.PoolSize(), m.ActiveCount())
	if pooled.Status != StatusPooled || pooled.Owner != "" || pooled.PrevOwner != "alice" {
		t.Fatalf("unexpected pooled snapshot: %+v", pooled)
	}
	if got := m.Usage("alice"); got != 0 {
		t.Fatalf("released env must not count against alice, usage=%d", got)
	}

	got, err := m.Acquire(ctx, "bob")
	if err != nil {
		t.Fatalf("bob acquire from pool: %v", err)
	}
	t.Logf("input: bob Acquire; decision: name=%s owner=%s prev=%s status=%s pool=%d active=%d ns=%d residue=%d",
		got.Name, got.Owner, got.PrevOwner, got.Status, m.PoolSize(), m.ActiveCount(),
		prov.NamespaceCount(), prov.ResidueCount())
	if got.Name != "shared-1" || got.Owner != "bob" || got.PrevOwner != "alice" {
		t.Fatalf("handover mismatch: %+v", got)
	}
	if got.Status != StatusReady || got.BoundTo != "" {
		t.Fatalf("reused env must be clean ready and unbound: %+v", got)
	}
	if m.PoolSize() != 0 || m.ActiveCount() != 1 || prov.NamespaceCount() != 1 {
		t.Fatalf("reuse must not grow total: pool=%d active=%d ns=%d", m.PoolSize(), m.ActiveCount(), prov.NamespaceCount())
	}
	if m.Usage("alice") != 0 || m.Usage("bob") != 1 {
		t.Fatalf("occupation must transfer to bob: alice=%d bob=%d", m.Usage("alice"), m.Usage("bob"))
	}
	if prov.HasResidue("shared-1", "alice") || prov.ResidueCount() != 0 {
		t.Fatal("reused env must be cleaned of previous user's residue")
	}
	// 过户后 bob 可正常绑定使用，alice 不能再碰。
	if _, err := m.Bind("shared-1", "bob"); err != nil {
		t.Fatalf("new owner bind: %v", err)
	}
	if _, err := m.Bind("shared-1", "alice"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("previous owner must lose access, got %v", err)
	}
}

// TestConcurrentAcquireNoOverissue：池中 K 份、全局上限 K、N>K 人同时 Acquire。
// 要求恰好 K 人成功、同一环境不会被两人拿到、不超发、账实一致，其余拿到全局容量拒绝。
func TestConcurrentAcquireNoOverissue(t *testing.T) {
	const K, N = 5, 50
	m, prov := newCapTestManager(1, K) // 每人额度 1，杜绝同一用户重复占用
	ctx := context.Background()
	for i := 0; i < K; i++ {
		name := "pool-" + string(rune('a'+i))
		if _, err := m.Create(ctx, name, "owner-"+name); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ReleaseToPool(name, "owner-"+name); err != nil {
			t.Fatal(err)
		}
	}
	if m.PoolSize() != K {
		t.Fatalf("seed pool size=%d want %d", m.PoolSize(), K)
	}

	type result struct {
		env *Env
		err error
	}
	res := make([]result, N)
	done := make(chan struct{}, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		user := "u" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i/676))
		go func(i int, user string) {
			<-start
			e, err := m.Acquire(ctx, user)
			res[i] = result{e, err}
			done <- struct{}{}
		}(i, user)
	}
	close(start)
	for i := 0; i < N; i++ {
		<-done
	}

	success := 0
	global := 0
	owners := map[string]int{}
	envWinners := map[string]string{}
	for _, r := range res {
		switch {
		case r.err == nil:
			success++
			owners[r.env.Owner]++
			if winner, dup := envWinners[r.env.Name]; dup {
				t.Fatalf("env %s handed to both %s and %s (double dispatch)", r.env.Name, winner, r.env.Owner)
			}
			envWinners[r.env.Name] = r.env.Owner
		case errors.Is(r.err, ErrGlobalCapacity):
			global++
		default:
			t.Fatalf("unexpected acquire error: %v", r.err)
		}
	}
	t.Logf("input: pool=%d globalCap=%d perUserQuota=1 acquirers=%d", K, K, N)
	t.Logf("decision: success=%d globalRejected=%d winners=%d active=%d pool=%d ns=%d",
		success, global, len(envWinners), m.ActiveCount(), m.PoolSize(), prov.NamespaceCount())
	if success != K {
		t.Fatalf("exactly %d must succeed (no over/under-issue), got %d", K, success)
	}
	if len(envWinners) != K {
		t.Fatalf("each pooled env handed out at most once: distinct=%d", len(envWinners))
	}
	if m.ActiveCount() != K || prov.NamespaceCount() != K || m.PoolSize() != 0 {
		t.Fatalf("books must match reality: active=%d ns=%d pool=%d", m.ActiveCount(), prov.NamespaceCount(), m.PoolSize())
	}
	for u, c := range owners {
		if c != 1 {
			t.Fatalf("user %s got %d envs, want 1", u, c)
		}
	}
}

// TestReuseFailureCompensates：清洗失败时，Acquire 返回可辨的 ErrReuseFailed，
// 环境必须补偿回共享池，账面占用与底层资源都不变，且可再次复用。
func TestReuseFailureCompensates(t *testing.T) {
	m, prov := newCapTestManager(10, 10)
	ctx := context.Background()
	if _, err := m.Create(ctx, "fix-1", "alice"); err != nil {
		t.Fatal(err)
	}
	prov.LeaveResidue("fix-1", "alice")
	if _, err := m.ReleaseToPool("fix-1", "alice"); err != nil {
		t.Fatal(err)
	}
	prov.FailOn = map[string]error{"fix-1": ErrLocalProvision}

	_, err := m.Acquire(ctx, "bob")
	t.Logf("input: bob Acquire pooled fix-1 with injected clean failure")
	t.Logf("decision: err=%v (IsReuse=%v IsQuota=%v IsGlobal=%v)",
		err, errors.Is(err, ErrReuseFailed), errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrGlobalCapacity))
	if !errors.Is(err, ErrReuseFailed) {
		t.Fatalf("expected ErrReuseFailed, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrGlobalCapacity) {
		t.Fatalf("reuse failure must not look like quota/global rejection: %v", err)
	}
	// 补偿：环境回到池中，仍是 pooled、无人占用、全局计数不变、底层仍在、残留未清。
	if m.PoolSize() != 1 || m.ActiveCount() != 1 || prov.NamespaceCount() != 1 {
		t.Fatalf("after compensation pool=%d active=%d ns=%d, want 1/1/1", m.PoolSize(), m.ActiveCount(), prov.NamespaceCount())
	}
	snap, ok := m.Get("fix-1")
	if !ok || snap.Status != StatusPooled || snap.Owner != "" || snap.PrevOwner != "alice" {
		t.Fatalf("env not restored to pool state: %+v ok=%v", snap, ok)
	}
	if m.Usage("alice") != 0 || m.Usage("bob") != 0 {
		t.Fatalf("failed handover must not charge anyone: alice=%d bob=%d", m.Usage("alice"), m.Usage("bob"))
	}
	if !prov.HasResidue("fix-1", "alice") {
		t.Fatal("residue must remain when cleaning failed")
	}

	// 故障排除后，同一份环境可被再次 Acquire 并成功过户。
	delete(prov.FailOn, "fix-1")
	got, err := m.Acquire(ctx, "bob")
	if err != nil {
		t.Fatalf("retry acquire after compensation: %v", err)
	}
	t.Logf("input: bob retries after failure cleared; decision: name=%s owner=%s pool=%d ns=%d residue=%d",
		got.Name, got.Owner, m.PoolSize(), prov.NamespaceCount(), prov.ResidueCount())
	if got.Name != "fix-1" || got.Owner != "bob" {
		t.Fatalf("retry must reuse the same env for bob, got %+v", got)
	}
	if prov.HasResidue("fix-1", "alice") {
		t.Fatal("residue must be cleaned on successful handover")
	}
}

// blockingCleaner 包装本地模拟器，让指定环境的 Clean 阻塞，用于构造交接进行中。
type blockingCleaner struct {
	*LocalProvisioner
	block   chan struct{}
	blockOn string
}

func (b *blockingCleaner) Clean(ctx context.Context, env *Env, prev string) error {
	if env.Name == b.blockOn {
		<-b.block
	}
	return b.LocalProvisioner.Clean(ctx, env, prev)
}

// TestResetInFlightProtected：环境处于 resetting（交接清洗中）时，
// 不能被到期回收、也不能被他人销毁/腾挪；清洗完成后正常完成过户。
func TestResetInFlightProtected(t *testing.T) {
	lp := NewLocalProvisioner()
	bc := &blockingCleaner{LocalProvisioner: lp, block: make(chan struct{}), blockOn: "hot-1"}
	m := NewManagerWithConfig(Config{Quota: Quota{MaxEnvsPerUser: 10}, MaxActiveEnvs: 10}, 50*time.Millisecond, bc)
	ctx := context.Background()
	if _, err := m.Create(ctx, "hot-1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReleaseToPool("hot-1", "alice"); err != nil {
		t.Fatal(err)
	}

	acqErr := make(chan error, 1)
	acqEnv := make(chan *Env, 1)
	go func() {
		e, err := m.Acquire(ctx, "bob")
		acqErr <- err
		acqEnv <- e
	}()
	// 等环境进入 resetting（已从池中弹出、清洗中）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := m.Get("hot-1"); ok && e.Status == StatusResetting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if e, _ := m.Get("hot-1"); e == nil || e.Status != StatusResetting {
		t.Fatalf("env never reached resetting: %+v", e)
	}

	time.Sleep(80 * time.Millisecond) // 越过 ttl，环境按到期判定本应被回收
	reaped, err := m.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: env hot-1 resetting and expired; Reap concurrently")
	t.Logf("decision: reaped=%v (expect empty; resetting is protected)", reaped)
	if len(reaped) != 0 {
		t.Fatalf("resetting env must not be reaped, got %v", reaped)
	}
	if err := m.Destroy(ctx, "hot-1", "alice"); !errors.Is(err, ErrConflict) {
		t.Fatalf("destroy during reset must be ErrConflict, got %v", err)
	}
	if _, err := m.Bind("hot-1", "carol"); !errors.Is(err, ErrConflict) {
		t.Fatalf("bind during reset must be ErrConflict, got %v", err)
	}
	if m.PoolSize() != 0 || m.ActiveCount() != 1 {
		t.Fatalf("during reset pool=0/active=1 expected, got pool=%d active=%d", m.PoolSize(), m.ActiveCount())
	}

	close(bc.block) // 放行清洗
	if err := <-acqErr; err != nil {
		t.Fatalf("handover after clean resumed: %v", err)
	}
	got := <-acqEnv
	t.Logf("input: clean resumed; decision: name=%s owner=%s status=%s pool=%d active=%d",
		got.Name, got.Owner, got.Status, m.PoolSize(), m.ActiveCount())
	if got.Owner != "bob" || got.Status != StatusReady {
		t.Fatalf("handover result wrong: %+v", got)
	}
}

// TestPooledExpiryReapAndRebuild：共享池中空闲到期的环境应被回收（从池摘除、底层销毁）；
// 另验证“非池占用占满全局时新申请被明确拒绝”，以及回收后 Acquire 走新建分支。
func TestPooledExpiryReapAndRebuild(t *testing.T) {
	m, prov := newCapTestManager(10, 1)
	ctx := context.Background()

	// 场景一：占用中的环境占满全局，池为空，他人 Acquire 只能走新建 -> 全局容量拒绝。
	if _, err := m.Create(ctx, "busy-1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Bind("busy-1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, "bob"); !errors.Is(err, ErrGlobalCapacity) {
		t.Fatalf("in-use env fills global capacity, expected ErrGlobalCapacity, got %v", err)
	}
	if err := m.Destroy(ctx, "busy-1", "alice"); err != nil {
		t.Fatal(err)
	}

	// 场景二：环境入池后空闲到期，Reap 回收使池清空、容量释放。
	if _, err := m.Create(ctx, "idle-1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReleaseToPool("idle-1", "alice"); err != nil {
		t.Fatal(err)
	}
	// 池非空时新申请优先复用，不触碰全局容量判定。
	if e, err := m.Acquire(ctx, "zoe"); err != nil || e.Name != "idle-1" {
		t.Fatalf("pooled env should be reused first, env=%+v err=%v", e, err)
	}
	// zoe 用完再放回池，制造一个待到期回收的池中环境。
	if _, err := m.ReleaseToPool("idle-1", "zoe"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.now = func() time.Time { return time.Now().Add(time.Hour) }
	m.mu.Unlock()
	reaped, err := m.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: pooled idle-1 expired (globalCap=1); Reap")
	t.Logf("decision: reaped=%v pool=%d active=%d ns=%d", reaped, m.PoolSize(), m.ActiveCount(), prov.NamespaceCount())
	if len(reaped) != 1 || reaped[0] != "idle-1" {
		t.Fatalf("expected idle-1 reaped, got %v", reaped)
	}
	if m.PoolSize() != 0 || m.ActiveCount() != 0 || prov.NamespaceCount() != 0 {
		t.Fatalf("books not cleared: pool=%d active=%d ns=%d", m.PoolSize(), m.ActiveCount(), prov.NamespaceCount())
	}

	// 容量腾出且池空，Acquire 走新建，不会“复活”已回收的环境。
	m.mu.Lock()
	m.now = time.Now
	m.mu.Unlock()
	got, err := m.Acquire(ctx, "bob")
	if err != nil {
		t.Fatalf("acquire after pooled reap should provision new: %v", err)
	}
	t.Logf("input: bob Acquire after pooled reap; decision: name=%s owner=%s active=%d ns=%d",
		got.Name, got.Owner, m.ActiveCount(), prov.NamespaceCount())
	if got.Name == "idle-1" {
		t.Fatal("must provision a fresh env, not resurrect the reaped one")
	}
	if m.ActiveCount() != 1 || prov.NamespaceCount() != 1 {
		t.Fatalf("after rebuild active=1/ns=1 expected, got %d/%d", m.ActiveCount(), prov.NamespaceCount())
	}
}

// TestConcurrentCreateGlobalCapNoOverissue：池空时多人并发 Acquire 走新建分支，
// 全局盘子为 K、个人额度充足，要求恰好 K 个成功、底层资源不超发、账实一致。
func TestConcurrentCreateGlobalCapNoOverissue(t *testing.T) {
	const K, N = 7, 60
	m, prov := newCapTestManager(N, K)
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, N)
	envs := make([]*Env, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		user := "c" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		go func(i int, user string) {
			defer wg.Done()
			<-start
			envs[i], errs[i] = m.Acquire(ctx, user)
		}(i, user)
	}
	close(start)
	wg.Wait()
	success, global := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrGlobalCapacity):
			global++
		default:
			t.Fatalf("unexpected err: %v", err)
		}
	}
	t.Logf("input: empty pool, globalCap=%d, %d concurrent Acquire (perUser quota generous)", K, N)
	t.Logf("decision: success=%d globalRejected=%d active=%d ns=%d pool=%d",
		success, global, m.ActiveCount(), prov.NamespaceCount(), m.PoolSize())
	if success != K {
		t.Fatalf("exactly %d creates should succeed, got %d", K, success)
	}
	if m.ActiveCount() != K || prov.NamespaceCount() != K {
		t.Fatalf("no over-issuance: active=%d ns=%d want %d", m.ActiveCount(), prov.NamespaceCount(), K)
	}
}
