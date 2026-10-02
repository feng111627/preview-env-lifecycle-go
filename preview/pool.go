package preview

import (
	"context"
	"fmt"
)

// Cleaner 是“把环境清洗到对新使用者干净可用”的底层能力。
// 复用时必须由 Provisioner 实现该接口，否则不允许交接。
type Cleaner interface {
	Clean(ctx context.Context, env *Env, prevOwner string) error
}

// ReleaseToPool 把当前用户空闲（ready）的环境放入共享池待复用。
// 占用从用户身上让出：不再计其个人额度，但底层资源仍在、仍占全局容量。
// 仅环境当前所有者可操作；使用中/创建中/交接中的环境不能入池。
func (m *Manager) ReleaseToPool(name, requester string) (*Env, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envs[name]
	if !ok || !backed(env.Status) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if env.Owner != requester {
		return nil, fmt.Errorf("%w: user %q cannot release %q", ErrPermissionDenied, requester, name)
	}
	switch env.Status {
	case StatusPooled:
		// 幂等：已在池中，直接返回当前快照。
		snap := *env
		return &snap, nil
	case StatusReady:
		// 仅空闲就绪的环境可让出。
	case StatusInUse:
		return nil, fmt.Errorf("%w: %q is in use, release it first", ErrConflict, name)
	default: // provisioning / resetting
		return nil, fmt.Errorf("%w: %q is not idle (state=%s)", ErrConflict, name, env.Status)
	}

	now := m.now()
	env.PrevOwner = env.Owner // 记下原主人，便于溯源与销毁鉴权
	env.Owner = ""            // 占用让出：不计任何个人额度
	env.BoundTo = ""
	env.Status = StatusPooled
	env.LastUsed = now
	env.ExpiresAt = now.Add(m.ttl) // 从入池时刻重新给一个空闲存活窗
	m.pool = append(m.pool, env)   // 池中登记，恰有一份
	snap := *env
	return &snap, nil
}

// Acquire 为用户获取一个环境。
// 优先复用共享池中已就绪的空闲环境（清洗后过户，总数不增）；
// 池空时才在个人额度与全局容量内新建。
// 拒绝原因严格区分：无权限 ErrPermissionDenied、个人额度满 ErrQuotaExceeded、
// 全局容量满 ErrGlobalCapacity，三者互不冒充；复用/腾挪失败返回 ErrReuseFailed
// 并把已做出的分配补偿回去。
func (m *Manager) Acquire(ctx context.Context, user string) (*Env, error) {
	// 个人额度先判：额度已满的用户不应触发任何取出或清洗。
	m.mu.Lock()
	if m.quota.MaxEnvsPerUser > 0 && m.usageLocked(user) >= m.quota.MaxEnvsPerUser {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: user %q reached limit %d", ErrQuotaExceeded, user, m.quota.MaxEnvsPerUser)
	}

	// 分支一：共享池有空闲环境，取出复用。
	if len(m.pool) > 0 {
		env := m.pool[0]
		m.pool = m.pool[1:] // 先从池里摘除，保证不会被第二个人同时拿到
		prevOwner := env.PrevOwner
		env.Status = StatusResetting
		m.mu.Unlock()

		return m.resetAndHandover(ctx, env, user, prevOwner)
	}
	m.mu.Unlock()

	// 分支二：池空，走新建。先个人额度、再全局容量。
	m.mu.Lock()
	if m.quota.MaxEnvsPerUser > 0 && m.usageLocked(user) >= m.quota.MaxEnvsPerUser {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: user %q reached limit %d", ErrQuotaExceeded, user, m.quota.MaxEnvsPerUser)
	}
	if m.maxActive > 0 {
		if n := m.activeCountLocked(); n >= m.maxActive {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: active=%d limit=%d", ErrGlobalCapacity, n, m.maxActive)
		}
	}
	now := m.now()
	name := m.autoNameLocked()
	env := &Env{
		Name:      name,
		Owner:     user,
		Status:    StatusProvisioning,
		CreatedAt: now,
		ExpiresAt: now.Add(m.ttl),
		LastUsed:  now,
	}
	m.envs[name] = env // 占位计入全局容量，防止并发超发
	m.mu.Unlock()

	if err := m.prov.Provision(ctx, env); err != nil {
		m.mu.Lock()
		if cur, ok := m.envs[name]; ok && cur == env {
			env.Status = StatusFailed
			env.FailReason = err
			delete(m.envs, name) // 回滚占位，账面与底层都不留
		}
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrProvisionFailed, err)
	}

	m.mu.Lock()
	if cur, ok := m.envs[name]; !ok || cur != env {
		m.mu.Unlock()
		_ = m.prov.Destroy(context.Background(), env)
		return nil, fmt.Errorf("%w: environment was removed while provisioning", ErrConflict)
	}
	env.Status = StatusReady
	snap := *env
	m.mu.Unlock()
	return &snap, nil
}

// resetAndHandover 锁外清洗一个已从池中取出的环境，成功后在锁内过户给新用户；
// 任何失败/不能交接的情形都把环境补偿回共享池，账实一致。
func (m *Manager) resetAndHandover(ctx context.Context, env *Env, user, prevOwner string) (*Env, error) {
	cleaner, ok := m.prov.(Cleaner)
	if !ok {
		m.compensateToPool(env, prevOwner)
		return nil, fmt.Errorf("%w: provisioner does not implement Cleaner", ErrReuseFailed)
	}
	if err := cleaner.Clean(ctx, env, prevOwner); err != nil {
		m.compensateToPool(env, prevOwner)
		return nil, fmt.Errorf("%w: clean %q for %q: %v", ErrReuseFailed, env.Name, user, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// 清洗期间可能被到期回收/销毁：以表中仍是同一份且仍在交接中为准。
	if cur, ok := m.envs[env.Name]; !ok || cur != env || env.Status != StatusResetting {
		// 账面已无此占用，清干净的底层资源也必须销毁，避免泄漏。
		_ = m.prov.Destroy(context.Background(), env)
		return nil, fmt.Errorf("%w: %q removed during reset", ErrConflict, env.Name)
	}
	// 清洗期间用户可能已通过其它申请占满额度：不能强过户，补偿回池。
	if m.quota.MaxEnvsPerUser > 0 && m.usageLocked(user) >= m.quota.MaxEnvsPerUser {
		m.returnToPoolLocked(env, prevOwner)
		return nil, fmt.Errorf("%w: user %q reached limit %d during handover", ErrQuotaExceeded, user, m.quota.MaxEnvsPerUser)
	}

	now := m.now()
	env.Owner = user // 占用记到新用户头上
	env.BoundTo = ""
	env.PrevOwner = prevOwner
	env.Status = StatusReady // 干净可用，等待新使用者 Bind
	env.LastUsed = now
	env.ExpiresAt = now.Add(m.ttl)
	snap := *env
	return &snap, nil
}

// compensateToPool 在锁外清洗失败后，把交接中的环境安全放回共享池。
// 仅当表中仍是同一份且状态仍为 resetting 时才放回（若已被回收/销毁则不动）。
func (m *Manager) compensateToPool(env *Env, prevOwner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.envs[env.Name]; !ok || cur != env || env.Status != StatusResetting {
		return
	}
	m.returnToPoolLocked(env, prevOwner)
}

// returnToPoolLocked 把环境登记回池（调用方须持锁）。
func (m *Manager) returnToPoolLocked(env *Env, prevOwner string) {
	env.Owner = ""
	env.BoundTo = ""
	env.PrevOwner = prevOwner
	env.Status = StatusPooled
	env.LastUsed = m.now()
	env.ExpiresAt = env.LastUsed.Add(m.ttl)
	m.pool = append(m.pool, env)
}

// autoNameLocked 生成一个未被占用的自动名（调用方须持锁）。
func (m *Manager) autoNameLocked() string {
	for {
		m.nameCounter++
		name := fmt.Sprintf("auto-env-%d", m.nameCounter)
		if _, exists := m.envs[name]; !exists {
			return name
		}
	}
}

// PoolSize 返回共享池中当前待复用的环境数。
func (m *Manager) PoolSize() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pool)
}

// ActiveCount 返回当前计入全局容量的活跃环境总数。
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeCountLocked()
}
