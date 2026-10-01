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
	StatusFailed       Status = "failed"       // 创建失败，占用已回滚
	StatusReaped       Status = "reaped"       // 已到期回收
)

// 可区分的失败原因，调用方可用 errors.Is 判定。
var (
	ErrAlreadyExists      = errors.New("preview: environment already exists")
	ErrNotFound           = errors.New("preview: environment not found")
	ErrPermissionDenied   = errors.New("preview: permission denied")
	ErrQuotaExceeded      = errors.New("preview: quota exceeded")
	ErrConflict           = errors.New("preview: environment in conflicting state")
	ErrProvisionFailed    = errors.New("preview: provisioning failed")
)

// Env 是一个预览环境实例。
type Env struct {
	Name      string
	Owner     string // 创建者
	BoundTo   string // 当前绑定的使用者，空表示未绑定
	Status    Status
	CreatedAt time.Time
	ExpiresAt time.Time
	LastUsed  time.Time
	FailReason error // 创建失败时的原因
}

// Quota 限制单个用户可同时占用的环境数。
type Quota struct {
	MaxEnvsPerUser int
}
