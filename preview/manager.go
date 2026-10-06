package preview

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Provisioner 模拟底层资源（命名空间）的创建、清理与销毁。
type Provisioner interface {
	Provision(ctx context.Context, env *Env) error
	// Sanitize 在复用前把环境清理成对新使用者干净可用的状态。
	Sanitize(ctx context.Context, env *Env) error
	Destroy(ctx context.Context, env *Env) error
}

// Manager 管理预览环境的生命周期。
// 所有状态变更都在同一把互斥锁下完成，保证并发下占用计数一致。
type Manager struct {
	mu    sync.Mutex
	envs  map[string]*Env
	quota Quota
	ttl   time.Duration
	prov  Provisioner
	now   func() time.Time
	seq   int // 自动命名计数器
}

// NewManager 创建一个生命周期管理器。
func NewManager(quota Quota, ttl time.Duration, prov Provisioner) *Manager {
	return &Manager{
		envs:  make(map[string]*Env),
		quota: quota,
		ttl:   ttl,
		prov:  prov,
		now:   time.Now,
	}
}

// active 状态仍计入配额占用。
func active(s Status) bool {
	return s == StatusProvisioning || s == StatusReady || s == StatusInUse
}

// usageLocked 统计用户当前占用的活跃环境数（调用方须持锁）。
// 已释放进共享池的环境不计入任何个人。
func (m *Manager) usageLocked(user string) int {
	n := 0
	for _, e := range m.envs {
		if e.Pooled {
			continue
		}
		if active(e.Status) && (e.Owner == user || e.BoundTo == user) {
			n++
		}
	}
	return n
}

// globalActiveLocked 统计全局活跃环境总数，含共享池内的（调用方须持锁）。
func (m *Manager) globalActiveLocked() int {
	n := 0
	for _, e := range m.envs {
		if active(e.Status) {
			n++
		}
	}
	return n
}

// Create 创建环境；重复/并发提交同一名字时幂等返回已有环境。
// 创建失败时回滚占用记录并返回可区分的原因。
func (m *Manager) Create(ctx context.Context, name, owner string) (*Env, error) {
	m.mu.Lock()
	if e, ok := m.envs[name]; ok && active(e.Status) {
		// 幂等：同名环境已存在且活跃，直接返回，不产生第二份。
		snap := *e
		m.mu.Unlock()
		return &snap, nil
	}
	if m.quota.MaxEnvsPerUser > 0 && m.usageLocked(owner) >= m.quota.MaxEnvsPerUser {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: user %q reached limit %d", ErrQuotaExceeded, owner, m.quota.MaxEnvsPerUser)
	}
	if m.quota.MaxGlobalEnvs > 0 && m.globalActiveLocked() >= m.quota.MaxGlobalEnvs {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: global limit %d reached", ErrCapacityExhausted, m.quota.MaxGlobalEnvs)
	}
	now := m.now()
	env := &Env{
		Name:      name,
		Owner:     owner,
		Status:    StatusProvisioning,
		CreatedAt: now,
		ExpiresAt: now.Add(m.ttl),
		LastUsed:  now,
	}
	m.envs[name] = env // 先占位，防止并发重复创建
	m.mu.Unlock()

	// 锁外执行耗时的资源创建，失败则回滚占位。
	if err := m.prov.Provision(ctx, env); err != nil {
		m.mu.Lock()
		// 仅当占位仍是我们写入的那份时才回滚，避免误删重建的结果。
		if cur, ok := m.envs[name]; ok && cur == env {
			env.Status = StatusFailed
			env.FailReason = err
			delete(m.envs, name) // 回滚占用
		}
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrProvisionFailed, err)
	}

	m.mu.Lock()
	// 若创建期间环境已被回收/销毁，则以回收为准，丢弃本次结果，避免泄漏。
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

// Destroy 销毁环境并释放占用；仅所有者可操作。
func (m *Manager) Destroy(ctx context.Context, name, requester string) error {
	m.mu.Lock()
	env, ok := m.envs[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if env.Owner != requester {
		m.mu.Unlock()
		return fmt.Errorf("%w: user %q is not the owner of %q", ErrPermissionDenied, requester, name)
	}
	delete(m.envs, name) // 先释放占用，再销毁底层资源
	m.mu.Unlock()
	return m.prov.Destroy(ctx, env)
}

// Bind 将环境绑定给使用者。所有者或其绑定人可使用；
// 非所有者领取需通过配额校验，拒绝原因区分权限与配额。
// 对共享池内的环境：仅原主可直接拿回自用（视为一次新投入使用，清掉池内
// 空闲标记、重新计入个人占用并过个人额度校验）；他人使用池内环境必须走
// Acquire 复用流程，否则返回 ErrPermissionDenied。
func (m *Manager) Bind(name, user string) (*Env, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envs[name]
	if !ok || !active(env.Status) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if env.Status == StatusProvisioning {
		return nil, fmt.Errorf("%w: %q is still provisioning", ErrConflict, name)
	}
	if env.Pooled {
		// 共享池内环境是待复用资源：只有原主能直接拿回自用；
		// 其他人想使用必须走 Acquire 的复用流程（会做清理与交接）。
		if env.Owner != user {
			return nil, fmt.Errorf("%w: pooled %q can only be reused via Acquire", ErrPermissionDenied, name)
		}
		// 池内环境当前不计入任何人的占用，原主拿回相当于一次新投入使用，
		// 必须清掉池内空闲标记并重新过个人额度校验，避免出现“占着全局名额
		// 却不计到任何人头上”的幽灵占用。
		if m.quota.MaxEnvsPerUser > 0 && m.usageLocked(user) >= m.quota.MaxEnvsPerUser {
			return nil, fmt.Errorf("%w: user %q reached limit %d", ErrQuotaExceeded, user, m.quota.MaxEnvsPerUser)
		}
		env.Pooled = false
	}
	if env.BoundTo != "" && env.BoundTo != user {
		return nil, fmt.Errorf("%w: %q is bound to %q", ErrPermissionDenied, name, env.BoundTo)
	}
	if env.Owner != user && env.BoundTo == "" {
		// 非所有者领取他人环境：拒绝（环境是 owner 的私有资源）。
		return nil, fmt.Errorf("%w: user %q does not own %q", ErrPermissionDenied, user, name)
	}
	// 已占用该环境的人（所有者或现绑定人）重复领取不计新配额。
	alreadyOccupies := !env.Pooled && (env.Owner == user || env.BoundTo == user)
	if m.quota.MaxEnvsPerUser > 0 && !alreadyOccupies && m.usageLocked(user) >= m.quota.MaxEnvsPerUser {
		return nil, fmt.Errorf("%w: user %q reached limit %d", ErrQuotaExceeded, user, m.quota.MaxEnvsPerUser)
	}
	env.BoundTo = user
	env.Status = StatusInUse
	env.LastUsed = m.now()
	// 重新激活：续期，防止刚被领取就被回收。
	env.ExpiresAt = env.LastUsed.Add(m.ttl)
	snap := *env
	return &snap, nil
}

// Touch 记录一次使用并续期。
func (m *Manager) Touch(name, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envs[name]
	if !ok || !active(env.Status) {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if env.Owner != user && env.BoundTo != user {
		return fmt.Errorf("%w: user %q cannot use %q", ErrPermissionDenied, user, name)
	}
	env.LastUsed = m.now()
	env.ExpiresAt = env.LastUsed.Add(m.ttl)
	return nil
}

// Reap 回收到期且无人使用的环境，返回被回收的环境名。
// 正在使用（InUse 且未过期）或刚被重新激活的环境不会被回收。
func (m *Manager) Reap(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	now := m.now()
	var victims []*Env
	for _, e := range m.envs {
		if e.Status == StatusInUse {
			continue // 正在使用，不回收
		}
		if now.Before(e.ExpiresAt) {
			continue // 未到期（含刚被重新激活续期的）
		}
		victims = append(victims, e)
	}
	// 先从占用表中摘除，再销毁底层资源，避免回收与重建竞态造成重复占用。
	names := make([]string, 0, len(victims))
	for _, e := range victims {
		if cur, ok := m.envs[e.Name]; ok && cur == e {
			e.Status = StatusReaped
			delete(m.envs, e.Name)
			names = append(names, e.Name)
		}
	}
	m.mu.Unlock()

	for _, e := range victims {
		if e.Status == StatusReaped {
			_ = m.prov.Destroy(ctx, e)
		}
	}
	return names, nil
}

// Release 把空闲环境释放进共享池，供后续 Acquire 复用。
// 仅所有者可释放；正被他人使用（绑定给别人）的环境不能腾挪。
// 释放后环境不再计入任何个人配额，但仍计入全局容量。
func (m *Manager) Release(name, requester string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envs[name]
	if !ok || !active(env.Status) {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if env.Pooled {
		return nil // 幂等：已在池中
	}
	if env.Owner != requester {
		return fmt.Errorf("%w: user %q is not the owner of %q", ErrPermissionDenied, requester, name)
	}
	if env.BoundTo != "" && env.BoundTo != requester {
		return fmt.Errorf("%w: %q is in use by %q", ErrConflict, name, env.BoundTo)
	}
	env.Pooled = true
	env.BoundTo = ""
	env.Status = StatusReady
	return nil
}

// takeFromPoolLocked 从共享池取一个可复用环境并认领（调用方须持锁）。
// 认领期间置为 InUse，防止被并发 Acquire 重复拿走、被 Reap 回收或被原主销毁。
func (m *Manager) takeFromPoolLocked(user string) (env *Env, prevOwner string) {
	var pick *Env
	for _, e := range m.envs {
		if !e.Pooled || e.Status != StatusReady || e.BoundTo != "" {
			continue
		}
		if pick == nil || e.Name < pick.Name { // 取名字最小者，保证结果可复现
			pick = e
		}
	}
	if pick == nil {
		return nil, ""
	}
	prevOwner = pick.Owner
	pick.Pooled = false
	pick.Owner = user
	pick.BoundTo = user
	pick.Status = StatusInUse
	return pick, prevOwner
}

// Acquire 为 user 申请一个环境：优先复用共享池中的空闲环境，
// 池空时才新建。复用不增加全局占用，新建受全局容量上限约束。
// 拿到手的环境已绑定给 user 并处于 InUse。
func (m *Manager) Acquire(ctx context.Context, user string) (*Env, error) {
	m.mu.Lock()
	if m.quota.MaxEnvsPerUser > 0 && m.usageLocked(user) >= m.quota.MaxEnvsPerUser {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: user %q reached limit %d", ErrQuotaExceeded, user, m.quota.MaxEnvsPerUser)
	}
	if env, prevOwner := m.takeFromPoolLocked(user); env != nil {
		m.mu.Unlock()
		return m.finishReuse(env, prevOwner, user)
	}
	// 池空，走新建：检查全局容量后占位。
	if m.quota.MaxGlobalEnvs > 0 && m.globalActiveLocked() >= m.quota.MaxGlobalEnvs {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: global limit %d reached", ErrCapacityExhausted, m.quota.MaxGlobalEnvs)
	}
	m.seq++
	name := fmt.Sprintf("env-%s-%d", user, m.seq)
	for m.envs[name] != nil { // 极端情况下与已有名字冲突时继续递增
		m.seq++
		name = fmt.Sprintf("env-%s-%d", user, m.seq)
	}
	now := m.now()
	env := &Env{
		Name:      name,
		Owner:     user,
		Status:    StatusProvisioning,
		CreatedAt: now,
		ExpiresAt: now.Add(m.ttl),
		LastUsed:  now,
	}
	m.envs[name] = env
	m.mu.Unlock()

	if err := m.prov.Provision(ctx, env); err != nil {
		m.mu.Lock()
		if cur, ok := m.envs[name]; ok && cur == env {
			env.Status = StatusFailed
			env.FailReason = err
			delete(m.envs, name)
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
	env.Status = StatusInUse
	env.BoundTo = user
	snap := *env
	m.mu.Unlock()
	return &snap, nil
}

// finishReuse 完成复用：锁外清理环境，失败则把认领补偿回池中。
func (m *Manager) finishReuse(env *Env, prevOwner, user string) (*Env, error) {
	if err := m.prov.Sanitize(context.Background(), env); err != nil {
		m.mu.Lock()
		// 补偿：撤销认领，占用记回原主，环境回到池中。
		if cur, ok := m.envs[env.Name]; ok && cur == env {
			env.Owner = prevOwner
			env.BoundTo = ""
			env.Status = StatusReady
			env.Pooled = true
		}
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrReuseFailed, err)
	}
	m.mu.Lock()
	if cur, ok := m.envs[env.Name]; !ok || cur != env {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: environment was removed while being reused", ErrConflict)
	}
	// 重新激活：续期，防止刚交付就被回收。
	env.LastUsed = m.now()
	env.ExpiresAt = env.LastUsed.Add(m.ttl)
	snap := *env
	m.mu.Unlock()
	return &snap, nil
}

// Get 返回环境快照。
func (m *Manager) Get(name string) (*Env, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[name]
	if !ok {
		return nil, false
	}
	snap := *e
	return &snap, true
}

// Usage 返回某用户当前占用的活跃环境数。
func (m *Manager) Usage(user string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usageLocked(user)
}

// ActiveCount 返回全局活跃环境总数（含共享池内的），
// 用于与底层实际资源数对账。
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.globalActiveLocked()
}
