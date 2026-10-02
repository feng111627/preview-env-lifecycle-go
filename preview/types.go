// Package preview 提供按需预览环境的生命周期管理。
package preview

import (
	"errors"
	"time"
)

// Status 表示预览环境的生命周期状态。
type Status string

const (
	StatusProvisioning Status = "provisioning" // 已分配占用，正在创建
	StatusReady        Status = "ready"        // 创建完成，可领取使用
	StatusInUse        Status = "in_use"       // 已被某用户领取使用
	StatusPooled       Status = "pooled"       // 空闲待复用：在共享池中，不计个人占用，仍计全局容量
	StatusResetting    Status = "resetting"    // 已从共享池取出，正在清洗交接，尚未记到新用户头上
	StatusFailed       Status = "failed"       // 创建失败，占用已回滚
	StatusReaped       Status = "reaped"       // 已到期回收
)

// 可区分的失败原因，调用方可用 errors.Is 判定。
var (
	ErrAlreadyExists    = errors.New("preview: environment already exists")
	ErrNotFound         = errors.New("preview: environment not found")
	ErrPermissionDenied = errors.New("preview: permission denied")
	ErrQuotaExceeded    = errors.New("preview: quota exceeded")
	ErrGlobalCapacity   = errors.New("preview: global active capacity exceeded")
	ErrConflict         = errors.New("preview: environment in conflicting state")
	ErrProvisionFailed  = errors.New("preview: provisioning failed")
	ErrReuseFailed      = errors.New("preview: environment reuse failed")
)

// Env 是一个预览环境实例。
type Env struct {
	Name       string
	Owner      string // 当前记在谁的占用账上（池中为空）
	BoundTo    string // 当前绑定的使用者，空表示未绑定
	PrevOwner  string // 上一任占用者，仅用于交接链路溯源与日志
	Status     Status
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsed   time.Time
	FailReason error // 创建失败时的原因
}

// Quota 限制单个用户可同时占用的环境数。
type Quota struct {
	MaxEnvsPerUser int
}

// Config 是管理器的完整配置：个人额度 + 全局容量。
// MaxActiveEnvs <= 0 表示不设全局上限（与旧行为一致）。
type Config struct {
	Quota
	MaxActiveEnvs int
}
