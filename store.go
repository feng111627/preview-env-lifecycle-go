package previewenv

import (
	"fmt"
	"sync"
)

// Store 是本地资源后端，模拟“命名空间槽位 + 配额单位”两种资源。
// 所有方法均自带互斥保护；同一时刻同一命名空间只能被一个环境占用。
type Store struct {
	mu sync.Mutex

	namespaces  map[string]string    // namespace -> envID
	allocations map[string]Resources // envID -> 已分配资源
	totalUnits  int
	maxUnits    int
}

// NewStore 创建一个最多容纳 maxUnits 个配额单位的本地资源后端。
func NewStore(maxUnits int) *Store {
	if maxUnits < 0 {
		maxUnits = 0
	}
	return &Store{
		namespaces:  make(map[string]string),
		allocations: make(map[string]Resources),
		maxUnits:    maxUnits,
	}
}

// Allocate 原子地为 envID 占用命名空间与配额；失败时不得留下任何部分占用。
func (s *Store) Allocate(envID string, res Resources) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if envID == "" {
		return newError("store.allocate", ReasonInvalidArgument, envID, "", "empty env id", nil)
	}
	if res.Namespace == "" {
		return newError("store.allocate", ReasonInvalidArgument, envID, "", "empty namespace", nil)
	}
	if res.Units < 0 {
		return newError("store.allocate", ReasonInvalidArgument, envID, "", "negative units", nil)
	}
	if _, exists := s.allocations[envID]; exists {
		return newError("store.allocate", ReasonAlreadyExists, envID, "",
			fmt.Sprintf("env %s already holds resources", envID), nil)
	}
	if holder, taken := s.namespaces[res.Namespace]; taken {
		return newError("store.allocate", ReasonResourceUnavailable, envID, "",
			fmt.Sprintf("namespace %s already occupied by env %s", res.Namespace, holder), nil)
	}
	if s.totalUnits+res.Units > s.maxUnits {
		return newError("store.allocate", ReasonResourceUnavailable, envID, "",
			fmt.Sprintf("not enough units: requested=%d used=%d capacity=%d",
				res.Units, s.totalUnits, s.maxUnits), nil)
	}

	// 全部前置检查通过后一次性落账，保证不存在部分占用。
	s.namespaces[res.Namespace] = envID
	s.allocations[envID] = res
	s.totalUnits += res.Units
	return nil
}

// Release 释放命名空间与配额。环境不存在或重复释放时返回可区分错误。
func (s *Store) Release(envID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, ok := s.allocations[envID]
	if !ok {
		return newError("store.release", ReasonNotFound, envID, "",
			fmt.Sprintf("env %s holds no resources (already released?)", envID), nil)
	}
	delete(s.namespaces, res.Namespace)
	delete(s.allocations, envID)
	s.totalUnits -= res.Units
	if s.totalUnits < 0 {
		// 内部记账不变量，理论上不可达。
		s.totalUnits = 0
	}
	return nil
}

// OccupiedUnits 返回已占用的配额单位总数（供测试断言）。
func (s *Store) OccupiedUnits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalUnits
}

// NamespaceOwner 返回命名空间当前的占用者环境 ID（空串表示空闲）。
func (s *Store) NamespaceOwner(namespace string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.namespaces[namespace]
}

// Snapshot 返回 namespace -> envID 的一致快照（供测试断言）。
func (s *Store) Snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.namespaces))
	for ns, envID := range s.namespaces {
		out[ns] = envID
	}
	return out
}
