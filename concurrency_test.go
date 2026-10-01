package previewenv

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConcurrentCreate_SameNameProducesOne(t *testing.T) {
	m, store, _ := newTestManager(t)
	m.SetQuota("alice", Quota{MaxEnvironments: 50, MaxUnits: 500})

	const n = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	var okCount, dupCount int64
	var mu sync.Mutex
	ids := make(map[string]int)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			env, err := m.Create(context.Background(), CreateRequest{
				Name: "hot", Owner: "alice", Units: 3,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okCount++
				ids[env.ID]++
				return
			}
			if ReasonOf(err) == ReasonAlreadyExists {
				dupCount++
				if env != nil {
					ids[env.ID]++
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	t.Logf("input: %d 个 goroutine 并发创建同名环境 hot; 判定: 成功=%d 已存在=%d id 分布=%v",
		n, okCount, dupCount, ids)
	if okCount != 1 {
		t.Fatalf("并发创建必须恰好 1 次成功, got %d", okCount)
	}
	if dupCount != n-1 {
		t.Fatalf("其余 %d 次应得到 already_exists, got %d", n-1, dupCount)
	}
	if len(ids) != 1 {
		t.Fatalf("所有响应必须指向同一个环境 ID, got %v", ids)
	}
	if store.OccupiedUnits() != 3 {
		t.Fatalf("只能产生一份占用 units=3, got %d", store.OccupiedUnits())
	}
	if len(store.Snapshot()) != 1 || store.NamespaceOwner("ns-hot") != "env-hot" {
		t.Fatalf("命名空间只能有一份占用, got %v", store.Snapshot())
	}
	t.Logf("最终占用一致: used=%d namespaces=%v（无重复占用、无泄漏）",
		store.OccupiedUnits(), store.Snapshot())
}

func TestConcurrentCreate_DifferentOwnersQuotaContention(t *testing.T) {
	m, store, _ := newTestManager(t)
	// alice 最多 2 个环境，bob 最多 10 个单位。
	m.SetQuota("alice", Quota{MaxEnvironments: 2, MaxUnits: 100})
	m.SetQuota("bob", Quota{MaxEnvironments: 10, MaxUnits: 6})

	const perUser = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var wgMu sync.Mutex
	counts := map[string]map[Reason]int{
		"alice": {},
		"bob":   {},
	}
	wg.Add(perUser * 2)
	for u := 0; u < 2; u++ {
		user := []string{"alice", "bob"}[u]
		for i := 0; i < perUser; i++ {
			i := i
			go func() {
				defer wg.Done()
				<-start
				name := user + "-" + string(rune('a'+i))
				_, err := m.Create(context.Background(), CreateRequest{
					Name: name, Owner: user, Units: 2,
				})
				wgMu.Lock()
				defer wgMu.Unlock()
				if err == nil {
					counts[user]["<nil>"]++
				} else {
					counts[user][ReasonOf(err)]++
				}
			}()
		}
	}
	close(start)
	wg.Wait()

	t.Logf("input: alice/bob 各并发提交 %d 个不同环境(每个 units=2); 判定结果: alice=%v bob=%v",
		perUser, counts["alice"], counts["bob"])
	if counts["alice"]["<nil>"] != 2 || counts["alice"][ReasonQuotaExceeded] != 6 {
		t.Fatalf("alice 应 2 成功 6 配额拒绝, got %v", counts["alice"])
	}
	if counts["bob"]["<nil>"] != 3 || counts["bob"][ReasonQuotaExceeded] != 5 {
		t.Fatalf("bob 应 3 成功(6 单位) 5 配额拒绝, got %v", counts["bob"])
	}
	assertStoreConsistent(t, m, store)
}

func TestReclaimAndRecreateRace(t *testing.T) {
	m, store, clk := newTestManager(t)
	m.SetQuota("alice", Quota{MaxEnvironments: 50, MaxUnits: 500})
	ctx := context.Background()

	// 制造一个已空闲到期的环境。
	env, _ := m.Create(ctx, CreateRequest{Name: "race", Owner: "alice", Units: 4})
	if _, err := m.Claim(ctx, env.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * time.Hour) // 远超 TTL 与宽限，等待回收

	const pairs = 40
	start := make(chan struct{})
	var wg sync.WaitGroup
	reclaimedCount := int64(0)
	createdCount := int64(0)
	var cmu sync.Mutex
	reasons := map[Reason]int{}

	// 一半 goroutine 反复尝试回收，一半反复尝试重建同名环境。
	wg.Add(pairs * 2)
	for i := 0; i < pairs; i++ {
		go func() { // reclaimer
			defer wg.Done()
			<-start
			res, err := m.ReclaimExpired(ctx, clk.now())
			if err != nil {
				t.Errorf("reclaim: %v", err)
				return
			}
			for _, r := range res {
				if r.Name == "race" && r.Reclaimed {
					cmu.Lock()
					reclaimedCount++
					reasons[r.Reason]++
					cmu.Unlock()
				}
			}
		}()
		go func() { // recreator
			defer wg.Done()
			<-start
			_, err := m.Create(ctx, CreateRequest{Name: "race", Owner: "alice", Units: 4})
			cmu.Lock()
			defer cmu.Unlock()
			if err == nil {
				createdCount++
			} else {
				r := ReasonOf(err)
				reasons[r]++
				if r != ReasonAlreadyExists && r != ReasonQuotaExceeded {
					t.Errorf("重建只应成功或 already_exists/quota, got %s: %v", r, err)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	final, _ := m.Get(ctx, "env-race")
	t.Logf("input: %d 回收者 + %d 重建者并发竞争已到期环境 race; 判定: 回收成功=%d 创建成功=%d 其他原因=%v 终态=%s",
		pairs, pairs, reclaimedCount, createdCount, reasons, final.State)

	// 不变量 1：回收只能发生一次。
	if reclaimedCount != 1 {
		t.Fatalf("到期回收必须恰好发生 1 次, got %d", reclaimedCount)
	}
	// 不变量 2：最终要么不存在（已回收），要么恰好一份存活占用；绝不允许两份。
	switch final.State {
	case StateDestroyed:
		t.Fatal("重建者已参与并发，终态不应是 destroyed")
	case StateReserved, StateActive:
		if store.NamespaceOwner("ns-race") != "env-race" {
			t.Fatalf("ns-race 占用者异常: %q", store.NamespaceOwner("ns-race"))
		}
	}
	if store.OccupiedUnits() != 4 {
		t.Fatalf("回收/重建竞态后占用必须恰好 4, got %d", store.OccupiedUnits())
	}
	if len(store.Snapshot()) != 1 {
		t.Fatalf("最终只能有一个命名空间占用, got %v", store.Snapshot())
	}
	alive := 0
	for _, e := range m.List(ctx) {
		if e.State != StateDestroyed {
			alive++
		}
	}
	if alive != 1 {
		t.Fatalf("存活环境必须恰好 1 个, got %d", alive)
	}
	t.Logf("最终占用一致: 存活=%d used=%d namespaces=%v（回收/重建竞态无泄漏、无重复）",
		alive, store.OccupiedUnits(), store.Snapshot())
}
