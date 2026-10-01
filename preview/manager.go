package preview

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Provisioner 模拟底层资源（命名空间）的创建与销毁。
type Provisioner interface {
	Provision(ctx context.Context, env *Env) error
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
func (m *Manager) usageLocked(user string) int {
	n := 0
	for _, e := range m.envs {
		if active(e.Status) && (e.Owner == user || e.BoundTo == user) {
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
	if env.BoundTo != "" && env.BoundTo != user {
		return nil, fmt.Errorf("%w: %q is bound to %q", ErrPermissionDenied, name, env.BoundTo)
	}
	if env.Owner != user && env.BoundTo == "" {
		// 非所有者领取他人环境：拒绝（环境是 owner 的私有资源）。
		return nil, fmt.Errorf("%w: user %q does not own %q", ErrPermissionDenied, user, name)
	}
	// 已占用该环境的人（所有者或现绑定人）重复领取不计新配额。
	alreadyOccupies := env.Owner == user || env.BoundTo == user
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
