package previewenv

import "time"

// State 表示预览环境在生命周期中的状态。
type State string

const (
	// StateReserved 已分配本地资源、等待使用者领取。
	StateReserved State = "reserved"
	// StateActive 已被领取且处于可用状态。
	StateActive State = "active"
	// StateDestroyed 已销毁，资源已归还，终态。
	StateDestroyed State = "destroyed"
)

// Resources 描述一个环境占用的本地资源。
type Resources struct {
	// Namespace 本地模拟的命名空间槽位名。
	Namespace string
	// Units 占用的配额单位数（模拟 CPU/内存等）。
	Units int
}

// Environment 是一个预览环境的生命周期快照。
type Environment struct {
	ID             string
	Name           string
	Owner          string
	State          State
	Resources      Resources
	CreatedAt      time.Time
	LastAccessAt   time.Time
	ClaimDeadline  time.Time
	ExpiresAt      time.Time
	DestroyedAt    time.Time
	ActiveSessions int
}

// Quota 描述单个使用者允许占用的上限。
type Quota struct {
	MaxEnvironments int
	MaxUnits        int
}

// CreateRequest 是创建预览环境的输入。
type CreateRequest struct {
	Name  string
	Owner string
	Units int
	// TTL 为 0 时使用管理器默认值。
	TTL time.Duration
}

// ReclaimResult 描述一次回收判定对单个环境的处理结果。
type ReclaimResult struct {
	EnvID     string
	Name      string
	Owner     string
	Reclaimed bool
	Reason    Reason
}

// Clone 返回环境快照，避免调用方在锁外修改内部状态。
func (e *Environment) Clone() *Environment {
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
}
