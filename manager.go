package previewenv

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Clock 抽象当前时间，便于测试推进到期/空闲计时。
type Clock func() time.Time

// FailHook 用于在创建流程的指定步骤注入失败，以验证回滚与重试。
type FailHook func(envID string, step string) error

const (
	defaultClaimTimeout = 30 * time.Second
	defaultRecentGrace  = 15 * time.Second
)

// Manager 管理预览环境的完整生命周期：创建、领取、心跳、
// 销毁、权限/配额判定与到期回收。
//
// 并发约定：所有生命周期状态变更都在 Manager.mu 下串行执行；
// 访问本地资源后端 Store 时锁顺序恒为 Manager.mu -> Store.mu，
// 因此“回收与重建”之间的竞态不会产生泄漏或重复占用。
type Manager struct {
	mu sync.Mutex

	store  *Store
	clock  Clock
	quotas map[string]Quota

	envs         map[string]*Environment
	byName       map[string]string // env name -> envID
	failHook     FailHook
	defaultTTL   time.Duration
	claimTimeout time.Duration
	recentGrace  time.Duration
}

// Option 用于配置 Manager。
type Option func(*Manager)

// WithClock 注入自定义时钟。
func WithClock(c Clock) Option {
	return func(m *Manager) {
		if c != nil {
			m.clock = c
		}
	}
}

// WithFailHook 注入创建流程的故障点。
func WithFailHook(h FailHook) Option {
	return func(m *Manager) { m.failHook = h }
}

// WithClaimTimeout 配置保留态的领取超时时长。
func WithClaimTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.claimTimeout = d
		}
	}
}

// WithRecentGrace 配置“刚被重新激活”的宽限窗口。
func WithRecentGrace(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.recentGrace = d
		}
	}
}

// NewManager 创建生命周期管理器。
func NewManager(store *Store, defaultTTL time.Duration, opts ...Option) *Manager {
	if defaultTTL <= 0 {
		defaultTTL = time.Hour
	}
	m := &Manager{
		store:        store,
		clock:        time.Now,
		quotas:       make(map[string]Quota),
		envs:         make(map[string]*Environment),
		byName:       make(map[string]string),
		defaultTTL:   defaultTTL,
		claimTimeout: defaultClaimTimeout,
		recentGrace:  defaultRecentGrace,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SetQuota 配置某个使用者的配额；未配置配额的使用者创建请求将被拒绝。
func (m *Manager) SetQuota(user string, q Quota) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotas[user] = q
}

func idFor(name string) string { return "env-" + name }
func nsFor(name string) string { return "ns-" + name }

// Create 创建（或复用）一个预览环境并占用本地资源。
// 同名环境并发/重复提交时只会成功一份（其余得到 ReasonAlreadyExists）；
// 资源分配后的注入失败会回滚已占用资源，不留下任何账外占用。
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Environment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Name == "" || req.Owner == "" {
		return nil, newError("create", ReasonInvalidArgument, "", req.Name,
			"name and owner are required", nil)
	}
	if req.Units < 0 {
		return nil, newError("create", ReasonInvalidArgument, "", req.Name,
			"units must be non-negative", nil)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	envID := idFor(req.Name)

	// 同名去重：已存活环境直接复用；已回收的同名环境允许原地重建。
	if existingID, ok := m.byName[req.Name]; ok {
		existing := m.envs[existingID]
		if existing != nil && existing.State != StateDestroyed {
			return existing.Clone(), newError("create", ReasonAlreadyExists, existingID, req.Name,
				fmt.Sprintf("env already exists in state %s, owner=%s", existing.State, existing.Owner), nil)
		}
	}

	// 配额判定（在分配资源之前），并区分“环境数超限”与“单位数超限”。
	quota, ok := m.quotas[req.Owner]
	if !ok {
		return nil, newError("create", ReasonQuotaExceeded, envID, req.Name,
			fmt.Sprintf("user %s has no quota configured", req.Owner), nil)
	}
	count, used := m.usageLocked(req.Owner)
	if quota.MaxEnvironments > 0 && count >= quota.MaxEnvironments {
		return nil, newError("create", ReasonQuotaExceeded, envID, req.Name,
			fmt.Sprintf("user %s environment count %d >= limit %d", req.Owner, count, quota.MaxEnvironments), nil)
	}
	if used+req.Units > quota.MaxUnits {
		return nil, newError("create", ReasonQuotaExceeded, envID, req.Name,
			fmt.Sprintf("user %s units %d+%d > limit %d", req.Owner, used, req.Units, quota.MaxUnits), nil)
	}

	now := m.clock()
	ttl := req.TTL
	if ttl <= 0 {
		ttl = m.defaultTTL
	}
	env := &Environment{
		ID:            envID,
		Name:          req.Name,
		Owner:         req.Owner,
		State:         StateReserved,
		Resources:     Resources{Namespace: nsFor(req.Name), Units: req.Units},
		CreatedAt:     now,
		LastAccessAt:  now,
		ClaimDeadline: now.Add(m.claimTimeout),
		ExpiresAt:     now.Add(ttl),
	}

	// 占用本地资源；Store 自身保证原子性，失败即无任何部分占用。
	if err := m.store.Allocate(envID, env.Resources); err != nil {
		return nil, newError("create", ReasonResourceUnavailable, envID, req.Name,
			"resource allocation failed", err)
	}

	// 故障注入点：资源已占用但环境尚未登记，模拟创建流程中途失败，
	// 必须回滚刚占用的命名空间与配额单位。
	if m.failHook != nil {
		if err := m.failHook(envID, "post_allocate"); err != nil {
			_ = m.store.Release(envID)
			return nil, newError("create", reasonFromHook(err), envID, req.Name,
				"create failed after allocation, resources rolled back", err)
		}
	}

	m.envs[envID] = env
	m.byName[req.Name] = envID
	return env.Clone(), nil
}

// reasonFromHook 允许故障注入显式声明原因，否则视为资源不可用。
func reasonFromHook(err error) Reason {
	if le, ok := AsLifecycleError(err); ok {
		return le.Reason
	}
	return ReasonResourceUnavailable
}

// Claim 将 reserved 环境领取为 active；非所有者得到权限错误，
// 保留超时未领取得到 ReasonClaimExpired（需等待回收后重建）。
func (m *Manager) Claim(ctx context.Context, envID, user string) (*Environment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	env, err := m.liveEnvLocked("claim", envID)
	if err != nil {
		return nil, err
	}
	if env.Owner != user {
		return nil, newError("claim", ReasonPermissionDenied, envID, env.Name,
			fmt.Sprintf("user %s is not owner %s", user, env.Owner), nil)
	}
	if env.State == StateActive {
		return env.Clone(), nil
	}
	now := m.clock()
	if now.After(env.ClaimDeadline) {
		return nil, newError("claim", ReasonClaimExpired, envID, env.Name,
			fmt.Sprintf("reservation expired at %s (now=%s)", env.ClaimDeadline.Format(time.RFC3339), now.Format(time.RFC3339)), nil)
	}
	env.State = StateActive
	env.LastAccessAt = now
	env.ExpiresAt = now.Add(m.defaultTTL)
	return env.Clone(), nil
}

// Touch 记录一次访问心跳并刷新空闲到期时间；非所有者被拒绝。
func (m *Manager) Touch(ctx context.Context, envID, user string) (*Environment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	env, err := m.liveEnvLocked("touch", envID)
	if err != nil {
		return nil, err
	}
	if env.Owner != user {
		return nil, newError("touch", ReasonPermissionDenied, envID, env.Name,
			fmt.Sprintf("user %s is not owner %s", user, env.Owner), nil)
	}
	now := m.clock()
	env.LastAccessAt = now
	if env.State == StateActive {
		env.ExpiresAt = now.Add(m.defaultTTL)
	}
	return env.Clone(), nil
}

// BeginSession 登记一个活跃使用会话（活跃中的环境不会被回收）。
func (m *Manager) BeginSession(ctx context.Context, envID, user string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	env, err := m.liveEnvLocked("begin_session", envID)
	if err != nil {
		return err
	}
	if env.Owner != user {
		return newError("begin_session", ReasonPermissionDenied, envID, env.Name,
			fmt.Sprintf("user %s is not owner %s", user, env.Owner), nil)
	}
	env.ActiveSessions++
	env.LastAccessAt = m.clock()
	return nil
}

// EndSession 结束一个活跃使用会话。
func (m *Manager) EndSession(ctx context.Context, envID, user string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	env, err := m.liveEnvLocked("end_session", envID)
	if err != nil {
		return err
	}
	if env.Owner != user {
		return newError("end_session", ReasonPermissionDenied, envID, env.Name,
			fmt.Sprintf("user %s is not owner %s", user, env.Owner), nil)
	}
	if env.ActiveSessions > 0 {
		env.ActiveSessions--
	}
	return nil
}

// Destroy 销毁环境并归还资源；非所有者被拒绝，重复销毁得到 ReasonNotFound。
func (m *Manager) Destroy(ctx context.Context, envID, user string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	env, err := m.liveEnvLocked("destroy", envID)
	if err != nil {
		return err
	}
	if env.Owner != user {
		return newError("destroy", ReasonPermissionDenied, envID, env.Name,
			fmt.Sprintf("user %s is not owner %s", user, env.Owner), nil)
	}
	return m.destroyLocked(env, "destroy")
}

// destroyLocked 在已持锁状态下回收资源并把环境置为终态。
// Store.Release 报告“已释放”时按幂等成功处理，保证重复回收安全。
func (m *Manager) destroyLocked(env *Environment, op string) error {
	if err := m.store.Release(env.ID); err != nil {
		if le, ok := AsLifecycleError(err); !ok || le.Reason != ReasonNotFound {
			return newError(op, ReasonResourceUnavailable, env.ID, env.Name,
				"failed to release resources", err)
		}
	}
	env.State = StateDestroyed
	env.DestroyedAt = m.clock()
	env.ActiveSessions = 0
	return nil
}

// Get 返回存活环境快照；不存在或已回收时返回 ReasonNotFound。
func (m *Manager) Get(ctx context.Context, envID string) (*Environment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.liveEnvLocked("get", envID)
}

// List 返回所有环境（含已销毁）的快照，供观测与测试断言。
func (m *Manager) List(ctx context.Context) []*Environment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Environment, 0, len(m.envs))
	for _, env := range m.envs {
		out = append(out, env.Clone())
	}
	return out
}

// ReclaimExpired 回收“保留超时未领取 / 空闲超时无人使用”的环境。
//   - reserved 且超过 ClaimDeadline：回收（claim_expired）；
//   - active 且存在活跃会话：跳过（in_use）；
//   - active 且处于最近激活宽限内：跳过（recently_active）；
//   - active 且未到空闲到期时间：跳过（still_active）；
//   - active 且超过 ExpiresAt：回收。
//
// 对已销毁环境重复调用是幂等的（结果原因 not_found，不报错）。
func (m *Manager) ReclaimExpired(ctx context.Context, now time.Time) ([]ReclaimResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.envs))
	for id := range m.envs {
		ids = append(ids, id)
	}
	results := make([]ReclaimResult, 0, len(ids))
	for _, id := range ids {
		env := m.envs[id]
		reclaimed, reason := m.reclaimLocked(env, now)
		results = append(results, ReclaimResult{
			EnvID:     id,
			Name:      env.Name,
			Owner:     env.Owner,
			Reclaimed: reclaimed,
			Reason:    reason,
		})
	}
	return results, nil
}

// liveEnvLocked 返回未销毁环境，否则给出可区分的 not_found 错误。
func (m *Manager) liveEnvLocked(op, envID string) (*Environment, error) {
	env := m.envs[envID]
	if env == nil || env.State == StateDestroyed {
		return nil, newError(op, ReasonNotFound, envID, "", "environment not found or already reclaimed", nil)
	}
	return env, nil
}

// usageLocked 返回某使用者当前未销毁环境的数量与占用单位数。
func (m *Manager) usageLocked(user string) (count, units int) {
	for _, env := range m.envs {
		if env.Owner == user && env.State != StateDestroyed {
			count++
			units += env.Resources.Units
		}
	}
	return count, units
}

// reclaimLocked 在锁内对单个环境做回收判定，返回是否回收及原因。
func (m *Manager) reclaimLocked(env *Environment, now time.Time) (bool, Reason) {
	switch env.State {
	case StateDestroyed:
		return false, ReasonNotFound
	case StateReserved:
		if now.After(env.ClaimDeadline) {
			if err := m.destroyLocked(env, "reclaim"); err != nil {
				return false, ReasonResourceUnavailable
			}
			return true, ReasonClaimExpired
		}
		return false, ReasonStillActive
	case StateActive:
		if env.ActiveSessions > 0 {
			return false, ReasonInUse
		}
		if !now.Before(env.LastAccessAt.Add(m.recentGrace)) {
			// 已离开“刚激活”宽限窗口，继续按空闲到期时间判定。
		} else {
			return false, ReasonRecentlyActive
		}
		if now.Before(env.ExpiresAt) {
			return false, ReasonStillActive
		}
		if err := m.destroyLocked(env, "reclaim"); err != nil {
			return false, ReasonResourceUnavailable
		}
		return true, ReasonExpired
	}
	return false, ReasonInvalidState
}
