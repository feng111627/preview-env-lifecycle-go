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
	residue    map[string]string // 环境名 -> 当前残留的上一任使用者标识
	FailOn     map[string]error  // 按环境名注入创建/清洗失败
}

// NewLocalProvisioner 创建本地模拟器。
func NewLocalProvisioner() *LocalProvisioner {
	return &LocalProvisioner{
		namespaces: make(map[string]bool),
		residue:    make(map[string]string),
	}
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

// Clean 模拟复用前的清洗：清掉上一任使用者的残留数据，并把底层资源标记为干净。
// 可通过 FailOn 注入失败以测试复用失败后的补偿；失败时保留残留（模拟没洗成）。
func (l *LocalProvisioner) Clean(_ context.Context, env *Env, prevOwner string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err, ok := l.FailOn[env.Name]; ok {
		return err
	}
	if !l.namespaces[env.Name] {
		return errors.New("namespace missing")
	}
	delete(l.residue, env.Name) // 清洗完成：上一任残留彻底清除
	return nil
}

// LeaveResidue 模拟某使用者在环境中留下数据（测试交接洁净度用）。
func (l *LocalProvisioner) LeaveResidue(envName, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.residue[envName] = user
}

// HasResidue 返回环境是否仍残留某使用者的数据。
func (l *LocalProvisioner) HasResidue(envName, user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.residue[envName] == user
}

// ResidueCount 返回仍带残留的环境数（断言交接后无残留）。
func (l *LocalProvisioner) ResidueCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.residue)
}
