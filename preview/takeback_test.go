// 本文件覆盖“空闲环境进共享池后被直接拿回自用”这条路径，
// 以及它与回收、复用、销毁、并发交叉时的账实一致性。
package preview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ledgerSane 校验账面占用与真实环境记录一致（仅测试辅助）：
// 全局活跃数 == 池内空闲数 + 被各用户认领的活跃数 == 底层命名空间数；
// 且不存在“活跃、非池内、却没有任何认领人”的幽灵占用。
func ledgerSane(t *testing.T, m *Manager, prov *LocalProvisioner) {
	t.Helper()
	m.mu.Lock()
	var pooled, claimed int
	for _, e := range m.envs {
		if !active(e.Status) {
			t.Errorf("env %s still on ledger with terminal status %s", e.Name, e.Status)
			continue
		}
		if e.Pooled {
			pooled++
			if e.Status != StatusReady || e.BoundTo != "" {
				t.Errorf("pooled env %s must be idle ready/unbound, got status=%s boundTo=%q", e.Name, e.Status, e.BoundTo)
			}
			continue
		}
		claimed++
		holder := e.BoundTo
		if holder == "" {
			holder = e.Owner
		}
		if holder == "" {
			t.Errorf("active non-pooled env %s has no claimant (ghost occupation)", e.Name)
		}
	}
	active := m.globalActiveLocked()
	m.mu.Unlock()
	if active != pooled+claimed {
		t.Errorf("ledger split mismatch: active=%d pooled=%d claimed=%d", active, pooled, claimed)
	}
	if ns := prov.NamespaceCount(); ns != active {
		t.Errorf("ledger vs real resources mismatch: active=%d namespaces=%d", active, ns)
	}
}

func TestReleaseThenOwnerTakebackChargesOccupation(t *testing.T) {
	m, prov := newTestManagerCap(5, 10, time.Hour)
	ctx := context.Background()
	if _, err := m.Create(ctx, "env-take", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-take", "alice"); err != nil {
		t.Fatal(err)
	}
	t.Logf("[business=进池后原主直接拿回] input: Create(env-take,alice)->Release->Bind(alice); usage(alice)=%d active=%d pooled=true",
		m.Usage("alice"), m.ActiveCount())

	env, err := m.Bind("env-take", "alice")
	if err != nil {
		t.Fatalf("owner take-back must be accepted: %v", err)
	}
	decision := fmt.Sprintf("env={name=%s owner=%s boundTo=%s status=%s pooled=%v} usage(alice)=%d active=%d namespaces=%d",
		env.Name, env.Owner, env.BoundTo, env.Status, env.Pooled, m.Usage("alice"), m.ActiveCount(), prov.NamespaceCount())
	t.Logf("[business=进池后原主直接拿回] decision: %s", decision)

	if env.Pooled {
		t.Fatal("pooled-idle flag must be cleared on take-back")
	}
	if env.Status != StatusInUse || env.BoundTo != "alice" || env.Owner != "alice" {
		t.Fatalf("take-back env must behave like a normally acquired one: %+v", env)
	}
	if got := m.Usage("alice"); got != 1 {
		t.Fatalf("occupation must be charged to alice, usage=%d", got)
	}
	if m.ActiveCount() != 1 || prov.NamespaceCount() != 1 {
		t.Fatalf("one real env must occupy exactly one global slot: active=%d ns=%d", m.ActiveCount(), prov.NamespaceCount())
	}
	ledgerSane(t, m, prov)

	// 拿回后表现与正常申请一致：别人不能抢、不能被复用、不会被到期回收，且可再次释放/销毁。
	if _, err := m.Bind("env-take", "mallory"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("third party must be denied after take-back, got %v", err)
	}
	if got, err := m.Acquire(ctx, "bob"); err != nil || got.Name == "env-take" {
		t.Fatalf("taken-back env must not be handed out for reuse, got %+v err=%v", got, err)
	}
	t.Logf("[business=拿回后与正常使用一致] decision: mallory Bind 拒绝(权限), bob Acquire 拿到新环境 %s; usage(alice)=%d usage(bob)=%d",
		gotName(m, "bob"), m.Usage("alice"), m.Usage("bob"))
	if err := m.Destroy(ctx, "env-take", "alice"); err != nil {
		t.Fatalf("owner must be able to destroy taken-back env: %v", err)
	}
	t.Logf("[business=拿回后可销毁释放] decision: destroyed; usage(alice)=%d active=%d namespaces=%d",
		m.Usage("alice"), m.ActiveCount(), prov.NamespaceCount())
	if m.Usage("alice") != 0 || m.ActiveCount() != prov.NamespaceCount() {
		t.Fatal("after destroy the slot must be fully released")
	}
	ledgerSane(t, m, prov)
}

// gotName 返回某用户当前持有环境的名字摘要，仅用于日志。
func gotName(m *Manager, user string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.envs {
		if !e.Pooled && active(e.Status) && (e.Owner == user || e.BoundTo == user) {
			return e.Name
		}
	}
	return "-"
}

func TestTakebackQuotaAndPermissionDistinct(t *testing.T) {
	ctx := context.Background()

	// 场景一：原主个人额度已用满，从池中拿回必须被配额拒绝（不新增全局占用，故不是容量错误）。
	m, prov := newTestManagerCap(2, 10, time.Hour)
	for _, name := range []string{"env-a", "env-idle"} {
		if _, err := m.Create(ctx, name, "alice"); err != nil { // 用满 2 个额度
			t.Fatal(err)
		}
	}
	if err := m.Release("env-idle", "alice"); err != nil { // 释放一个，usage=1
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "env-c", "alice"); err != nil { // 再占一个，usage 重新到 2/2
		t.Fatal(err)
	}
	_, err := m.Bind("env-idle", "alice")
	t.Logf("[business=拿回时个人额度用满] input: alice usage=%d/2, pooled env-idle 存在", m.Usage("alice"))
	t.Logf("[business=拿回时个人额度用满] decision: err=%v IsQuota=%v IsCapacity=%v IsPermission=%v",
		err, errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrCapacityExhausted), errors.Is(err, ErrPermissionDenied))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("take-back over quota must be ErrQuotaExceeded, got %v", err)
	}
	if errors.Is(err, ErrCapacityExhausted) || errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("quota rejection must not masquerade: %v", err)
	}
	// 被配额拒绝不得改动任何分配：环境仍在池中、全局名额不丢。
	e, ok := m.Get("env-idle")
	if !ok || !e.Pooled || e.Status != StatusReady || e.BoundTo != "" || e.Owner != "alice" {
		t.Fatalf("rejected take-back must leave env pooled and untouched: %+v ok=%v", e, ok)
	}
	ledgerSane(t, m, prov)
	t.Logf("[business=拒绝不产生半成品] decision: env-idle 仍 pooled/ready/unbound, active=%d namespaces=%d",
		m.ActiveCount(), prov.NamespaceCount())

	// 场景二：非原主不能靠 Bind 绕过复用流程拿走池内环境。
	_, err = m.Bind("env-idle", "bob")
	t.Logf("[business=他人试图Bind绕过Acquire] input: Bind(env-idle,bob), 环境在 alice 的池内")
	t.Logf("[business=他人试图Bind绕过Acquire] decision: err=%v IsPermission=%v IsQuota=%v",
		err, errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrQuotaExceeded))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("non-owner Bind on pooled env must be ErrPermissionDenied, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("permission rejection must not masquerade: %v", err)
	}
	ledgerSane(t, m, prov)

	// 场景三：拒绝后环境仍可走正规 Acquire 被复用（不丢、不重）。
	env, err := m.Acquire(ctx, "carol")
	if err != nil {
		t.Fatalf("pooled env must still be reusable after rejected take-backs: %v", err)
	}
	t.Logf("[business=拒绝后仍可正规复用] decision: carol Acquire -> %s owner=%s pooled=%v active=%d namespaces=%d",
		env.Name, env.Owner, env.Pooled, m.ActiveCount(), prov.NamespaceCount())
	if env.Name != "env-idle" || env.Owner != "carol" || env.Pooled {
		t.Fatalf("env-idle must transfer intact to carol: %+v", env)
	}
	if m.Usage("carol") != 1 || prov.NamespaceCount() != m.ActiveCount() {
		t.Fatal("ledger must stay consistent after reuse")
	}
	ledgerSane(t, m, prov)
}

func TestTakebackCrossLifecycleLedgerConsistency(t *testing.T) {
	ctx := context.Background()

	// 子场景 A：池内空闲环境已到期，被原主拿回后按重新激活处理（续期），
	// 之后 Reap 不能回收它；同一批未被拿回的池内到期环境仍照常回收。
	ttl := time.Hour
	m, prov := newTestManagerCap(5, 10, ttl)
	var clock time.Time
	m.now = func() time.Time { return clock }
	for _, name := range []string{"env-taken", "env-left"} {
		if _, err := m.Create(ctx, name, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := m.Release(name, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	clock = clock.Add(2 * ttl)                              // 两个池内环境都已到期
	if _, err := m.Bind("env-taken", "alice"); err != nil { // 到期瞬间被拿回：重新激活续期
		t.Fatal(err)
	}
	reaped, _ := m.Reap(ctx)
	t.Logf("[business=池内到期后拿回与回收交叉] input: 两个池内环境到期, alice 拿回 env-taken; decision: reaped=%v（未拿回的 env-left 应被回收）", reaped)
	if len(reaped) != 1 || reaped[0] != "env-left" {
		t.Fatalf("only the still-pooled expired env must be reaped, got %v", reaped)
	}
	if got, ok := m.Get("env-taken"); !ok || got.Status != StatusInUse || got.Pooled {
		t.Fatalf("taken-back env must survive reap as in-use non-pooled: %+v ok=%v", got, ok)
	}
	if m.Usage("alice") != 1 || m.ActiveCount() != 1 || prov.NamespaceCount() != 1 {
		t.Fatalf("ledger mismatch after take-back+reap: usage=%d active=%d ns=%d",
			m.Usage("alice"), m.ActiveCount(), prov.NamespaceCount())
	}
	ledgerSane(t, m, prov)

	// 后续在同一组环境上继续交叉：拿回后再次 Release 进池，别人 Acquire 可正常复用。
	if err := m.Release("env-taken", "alice"); err != nil {
		t.Fatal(err)
	}
	env, err := m.Acquire(ctx, "bob")
	if err != nil || env.Name != "env-taken" {
		t.Fatalf("re-release after take-back must be reusable: %+v err=%v", env, err)
	}
	t.Logf("[business=拿回->再进池->他人复用] decision: bob 复用 env-taken owner=%s boundTo=%s pooled=%v sanitized=%d active=%d ns=%d",
		env.Owner, env.BoundTo, env.Pooled, prov.SanitizeCount("env-taken"), m.ActiveCount(), prov.NamespaceCount())
	if env.Owner != "bob" || env.BoundTo != "bob" || env.Status != StatusInUse || env.Pooled {
		t.Fatalf("handover must be complete: %+v", env)
	}
	if m.Usage("alice") != 0 || m.Usage("bob") != 1 || prov.SanitizeCount("env-taken") != 1 {
		t.Fatal("occupation must move alice->bob exactly once")
	}
	ledgerSane(t, m, prov)

	// 复用给 bob 后，alice 已无权销毁，原占用不能“一女两嫁”。
	if err := m.Destroy(ctx, "env-taken", "alice"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("previous owner must lose control after reuse, got %v", err)
	}
	if err := m.Destroy(ctx, "env-taken", "bob"); err != nil {
		t.Fatal(err)
	}
	t.Logf("[business=复用后旧主无权销毁/新主可销毁] decision: alice 销毁拒绝, bob 销毁成功; active=%d ns=%d usage(bob)=%d",
		m.ActiveCount(), prov.NamespaceCount(), m.Usage("bob"))
}

func TestConcurrentTakebackVsAcquireNoDoubleHandout(t *testing.T) {
	m, prov := newTestManagerCap(5, 1, time.Hour) // 全局封顶 1：池里那一个就是全部名额
	ctx := context.Background()
	if _, err := m.Create(ctx, "env-only", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-only", "alice"); err != nil {
		t.Fatal(err)
	}

	const contenders = 12
	var wg sync.WaitGroup
	results := make([]string, contenders) // 每方结果：env 名 / reject / error
	start := make(chan struct{})
	// 一半 goroutine 由原主并发 Bind 拿回；其余不同用户并发 Acquire。
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				if e, err := m.Bind("env-only", "alice"); err != nil {
					results[i] = "reject:" + classErr(err)
				} else {
					results[i] = "takeback:" + e.Name
				}
				return
			}
			user := fmt.Sprintf("user-%d", i)
			if e, err := m.Acquire(ctx, user); err != nil {
				results[i] = "reject:" + classErr(err)
			} else {
				results[i] = "acquire:" + e.Name
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 注意：6 路“拿回”都由同一个原主 alice 发起。首个拿回成功后，其余属于
	// “持有者重复领取”（幂等，不新增占用），与既有 Bind 语义一致，不算两个人拿到。
	// 因此这里按“最终持有者身份”去重：要么 alice 拿回，要么某个 Acquire 用户拿走。
	winHolders := map[string]bool{}
	rejects := map[string]int{}
	for _, r := range results {
		if r == "" {
			t.Fatal("empty result")
		}
		if len(r) > 7 && r[:7] == "reject:" {
			rejects[r[7:]]++
			continue
		}
		switch {
		case r == "takeback:env-only":
			winHolders["alice"] = true
		case len(r) > 8 && r[:8] == "acquire:":
			// 具体持有人以最终环境快照为准，这里先记录“被 Acquire 路径拿走”。
			winHolders["acquire"] = true
		default:
			t.Fatalf("unexpected result %q", r)
		}
	}
	t.Logf("[business=并发拿回与复用争抢同一池内环境] input: %d 路并发(原主Bind拿回/6 同一人, 他人Acquire/6), 池=1 全局上限=1", contenders)
	t.Logf("[business=并发拿回与复用争抢同一池内环境] decision: winPaths=%v rejects=%v active=%d ns=%d usage(alice)=%d",
		winHolders, rejects, m.ActiveCount(), prov.NamespaceCount(), m.Usage("alice"))

	if len(winHolders) != 1 {
		t.Fatalf("env must go to exactly one side (owner take-back XOR one acquire), got %v", winHolders)
	}
	for class, n := range rejects {
		// 容量满 -> capacity；个人额度满 -> quota；
		// 环境已被某个 Acquire 用户拿走后原主再 Bind -> permission（合法）。
		if class != "capacity" && class != "quota" && class != "permission" {
			t.Fatalf("unexpected rejection class %s x%d: %v", class, n, results)
		}
	}
	// 得手方必须独占：若原主拿回，usage 记在 alice 头上；若被 Acquire 拿走，则持有者唯一且 alice=0。
	e, ok := m.Get("env-only")
	if !ok || e.Status != StatusInUse || e.Pooled {
		t.Fatalf("env must end in-use and out of pool: %+v ok=%v", e, ok)
	}
	holders := 0
	for _, name := range append([]string{"alice"}, userList(contenders)...) {
		if n := m.Usage(name); n > 0 {
			if n != 1 {
				t.Fatalf("user %s must hold at most 1, got %d", name, n)
			}
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("env must be charged to exactly one user, got %d", holders)
	}
	if m.ActiveCount() != 1 || prov.NamespaceCount() != 1 {
		t.Fatalf("no oversell/leak: active=%d ns=%d", m.ActiveCount(), prov.NamespaceCount())
	}
	ledgerSane(t, m, prov)
}

// classErr 把拒绝归类为 capacity/quota/permission/other，仅用于日志与断言。
func classErr(err error) string {
	switch {
	case errors.Is(err, ErrCapacityExhausted):
		return "capacity"
	case errors.Is(err, ErrQuotaExceeded):
		return "quota"
	case errors.Is(err, ErrPermissionDenied):
		return "permission"
	default:
		return "other:" + err.Error()
	}
}

func userList(n int) []string {
	out := make([]string, 0, n/2)
	for i := 0; i < n; i++ {
		if i%2 != 0 {
			out = append(out, fmt.Sprintf("user-%d", i))
		}
	}
	return out
}

func TestReuseFailureAfterTakebackCycleCompensates(t *testing.T) {
	ctx := context.Background()
	m, prov := newTestManagerCap(5, 10, time.Hour)

	if _, err := m.Create(ctx, "env-hot", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("env-hot", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Bind("env-hot", "alice"); err != nil { // 原主拿回
		t.Fatal(err)
	}
	if err := m.Release("env-hot", "alice"); err != nil { // 再次进池，供别人复用
		t.Fatal(err)
	}

	// 注入清理失败：bob 的复用交接中途失败。
	prov.FailSanitizeOn = map[string]error{"env-hot": ErrLocalProvision}
	_, err := m.Acquire(ctx, "bob")
	t.Logf("[business=复用交接中途失败需补偿] input: 进池->拿回->再进池 后 Acquire(bob), Sanitize 注入失败")
	t.Logf("[business=复用交接中途失败需补偿] decision: err=%v IsReuse=%v IsQuota=%v IsCapacity=%v IsPermission=%v usage(bob)=%d",
		err, errors.Is(err, ErrReuseFailed), errors.Is(err, ErrQuotaExceeded),
		errors.Is(err, ErrCapacityExhausted), errors.Is(err, ErrPermissionDenied), m.Usage("bob"))
	if !errors.Is(err, ErrReuseFailed) {
		t.Fatalf("handover failure must be ErrReuseFailed, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrCapacityExhausted) || errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("handover failure must not masquerade as rejection: %v", err)
	}
	// 补偿：这笔占用不能算到 bob 头上，环境不丢不重，回到池中。
	if m.Usage("bob") != 0 || m.Usage("alice") != 0 {
		t.Fatalf("failed handover charges nobody: bob=%d alice=%d", m.Usage("bob"), m.Usage("alice"))
	}
	e, ok := m.Get("env-hot")
	if !ok || !e.Pooled || e.Owner != "alice" || e.Status != StatusReady || e.BoundTo != "" {
		t.Fatalf("env must be compensated back to pool under original owner: %+v ok=%v", e, ok)
	}
	ledgerSane(t, m, prov)
	t.Logf("[business=失败补偿账实一致] decision: env-hot 回池(owner=alice,pooled,ready,unbound); active=%d ns=%d",
		m.ActiveCount(), prov.NamespaceCount())

	// 补偿后两条出路都应可用：原主可直接拿回；拿回后也可再次释放给别人复用。
	if _, err := m.Bind("env-hot", "alice"); err != nil {
		t.Fatalf("owner must be able to take a compensated env back: %v", err)
	}
	if m.Usage("alice") != 1 {
		t.Fatalf("take-back after compensation must charge alice: %d", m.Usage("alice"))
	}
	if err := m.Release("env-hot", "alice"); err != nil {
		t.Fatal(err)
	}
	delete(prov.FailSanitizeOn, "env-hot")
	env, err := m.Acquire(ctx, "carol")
	if err != nil || env.Name != "env-hot" || env.Owner != "carol" {
		t.Fatalf("retry reuse after compensation must succeed for carol: %+v err=%v", env, err)
	}
	t.Logf("[business=补偿后拿回与再次复用] decision: alice 拿回成功 -> 再进池 -> carol 复用 env-hot; active=%d ns=%d usage(carol)=%d",
		m.ActiveCount(), prov.NamespaceCount(), m.Usage("carol"))
	ledgerSane(t, m, prov)
}

func TestConcurrentMixedOpsWithTakebackFinalConsistency(t *testing.T) {
	m, prov := newTestManagerCap(3, 6, 20*time.Millisecond)
	ctx := context.Background()
	var wg sync.WaitGroup
	// 8 个用户并发：除了申请/释放/销毁/Touch，还会把自己名下池内环境直接拿回自用。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", i)
			for j := 0; j < 24; j++ {
				env, err := m.Acquire(ctx, user)
				if err != nil {
					continue // 容量/配额拒绝是合法结果
				}
				switch j % 4 {
				case 0:
					_ = m.Release(env.Name, user)
				case 1:
					_ = m.Destroy(ctx, env.Name, user)
				case 2:
					_ = m.Touch(env.Name, user)
				case 3:
					// 先释放进池，再由原主直接拿回自用（本次修复的核心路径）。
					if err := m.Release(env.Name, user); err == nil {
						if _, berr := m.Bind(env.Name, user); berr != nil {
							// 拿回失败只可能是配额/权限等明确拒绝：环境必须仍在池中。
							if e, ok := m.Get(env.Name); ok && !e.Pooled {
								t.Errorf("failed take-back left env non-pooled: %+v err=%v", e, berr)
							}
						}
					}
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
				_, _ = m.Reap(ctx)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	wg.Wait()
	close(stop)
	reaperWg.Wait()
	_, _ = m.Reap(ctx)
	time.Sleep(30 * time.Millisecond) // 让所有可回收的环境到期
	_, _ = m.Reap(ctx)

	activeN := m.ActiveCount()
	ns := prov.NamespaceCount()
	t.Logf("[business=含拿回路径的混合并发账实对账] input: 8 users x 24 rounds (Acquire/Release/Destroy/Touch/Release+Bind拿回) + 后台 Reap")
	t.Logf("[business=含拿回路径的混合并发账实对账] decision: after final reap active=%d namespaces=%d", activeN, ns)
	if activeN != ns {
		t.Fatalf("ledger vs real resources mismatch: active=%d namespaces=%d", activeN, ns)
	}
	// 强不变量：不允许存在 pooled + in_use 的幽灵态，也不允许活跃环境没有认领人。
	m.mu.Lock()
	for _, e := range m.envs {
		if e.Pooled && e.Status == StatusInUse {
			t.Errorf("ghost env %s: pooled but in_use", e.Name)
		}
		if !e.Pooled && active(e.Status) && e.Owner == "" && e.BoundTo == "" {
			t.Errorf("ghost env %s: active with no claimant", e.Name)
		}
	}
	m.mu.Unlock()
	ledgerSane(t, m, prov)
}
