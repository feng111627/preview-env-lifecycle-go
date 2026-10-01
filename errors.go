package previewenv

import (
	"errors"
	"fmt"
)

// Reason 以可区分的字符串说明一次生命周期操作被接受或拒绝的原因。
type Reason string

const (
	ReasonUnknown             Reason = "unknown"
	ReasonAlreadyExists       Reason = "already_exists"       // 同名环境已存在（并发/重复提交）
	ReasonNotFound            Reason = "not_found"            // 环境不存在或已回收
	ReasonPermissionDenied    Reason = "permission_denied"    // 非所有者操作
	ReasonQuotaExceeded       Reason = "quota_exceeded"       // 超出使用者配额
	ReasonResourceUnavailable Reason = "resource_unavailable" // 本地命名空间/配额单位不足
	ReasonInvalidState        Reason = "invalid_state"        // 当前生命周期状态不允许该操作
	ReasonInvalidArgument     Reason = "invalid_argument"     // 入参非法
	ReasonClaimExpired        Reason = "claim_expired"        // 保留超时未领取
	ReasonExpired             Reason = "expired"              // 空闲/TTL 到期被回收
	ReasonInUse               Reason = "in_use"               // 存在活跃会话，跳过回收
	ReasonRecentlyActive      Reason = "recently_active"      // 刚被重新激活/心跳，跳过回收
	ReasonStillActive         Reason = "still_active"         // 未到空闲超时，跳过回收
)

// LifecycleError 携带操作名、环境 ID、原因与底层错误，
// 便于调用方区分“权限问题 / 配额问题 / 资源冲突”等失败类别。
type LifecycleError struct {
	Op      string
	Reason  Reason
	EnvID   string
	Name    string
	Message string
	Err     error
}

func (e *LifecycleError) Error() string {
	base := fmt.Sprintf("previewenv: op=%s reason=%s", e.Op, e.Reason)
	if e.EnvID != "" {
		base += " env=" + e.EnvID
	}
	if e.Name != "" {
		base += " name=" + e.Name
	}
	if e.Message != "" {
		base += ": " + e.Message
	}
	if e.Err != nil {
		base += ": " + e.Err.Error()
	}
	return base
}

func (e *LifecycleError) Unwrap() error { return e.Err }

// newError 构造一个生命周期错误。
func newError(op string, reason Reason, envID, name, msg string, cause error) *LifecycleError {
	return &LifecycleError{Op: op, Reason: reason, EnvID: envID, Name: name, Message: msg, Err: cause}
}

// AsLifecycleError 把任意错误解释为 *LifecycleError。
func AsLifecycleError(err error) (*LifecycleError, bool) {
	var le *LifecycleError
	if errors.As(err, &le) {
		return le, true
	}
	return nil, false
}

// ReasonOf 返回错误对应的可区分原因；nil 返回空串，未知错误返回 ReasonUnknown。
func ReasonOf(err error) Reason {
	if err == nil {
		return ""
	}
	if le, ok := AsLifecycleError(err); ok {
		return le.Reason
	}
	return ReasonUnknown
}
