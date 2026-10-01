package preview

import (
	"context"
	"errors"
	"sync"
)

// ErrLocalProvision 是本地模拟器被注入的失败原因。
var ErrLocalProvision = errors.New("local provisioner: injected failure")

// LocalProvisioner 用内存 map 模拟命名空间资源，可注入失败以测试回滚。
type LocalProvisioner struct {
	mu         sync.Mutex
	namespaces map[string]bool
	FailOn     map[string]error // 按环境名注入失败
}

// NewLocalProvisioner 创建本地模拟器。
func NewLocalProvisioner() *LocalProvisioner {
	return &LocalProvisioner{namespaces: make(map[string]bool)}
}

// Provision 模拟创建命名空间；重名视为冲突失败。
func (l *LocalProvisioner) Provision(_ context.Context, env *Env) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err, ok := l.FailOn[env.Name]; ok {
		return err
	}
	if l.namespaces[env.Name] {
		return errors.New("namespace already exists")
	}
	l.namespaces[env.Name] = true
	return nil
}

// Destroy 模拟删除命名空间，幂等。
func (l *LocalProvisioner) Destroy(_ context.Context, env *Env) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.namespaces, env.Name)
	return nil
}

// NamespaceCount 返回当前存活的命名空间数，用于断言无泄漏。
func (l *LocalProvisioner) NamespaceCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.namespaces)
}
