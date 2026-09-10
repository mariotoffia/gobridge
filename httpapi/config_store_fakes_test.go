package httpapi

import (
	"context"
	"io/fs"
	"sync"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// casConfigStore can inject a peer write at the persistence boundary.
// Save overwrites it; SaveIfVersion rejects the stale expected version.
type casConfigStore struct {
	mu            sync.Mutex
	current       *ports.BridgeConfig
	concurrentCfg *ports.BridgeConfig
	concurrentAt  int
	saves         []*ports.BridgeConfig
}

func (s *casConfigStore) applyConcurrentLocked() {
	if s.concurrentCfg != nil && len(s.saves) == s.concurrentAt {
		s.current = cloneBridgeConfig(s.concurrentCfg)
		s.concurrentCfg = nil
	}
}

func (s *casConfigStore) Load(_ context.Context) (*ports.BridgeConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, fs.ErrNotExist
	}
	clone := *s.current
	return &clone, nil
}

func (s *casConfigStore) Save(_ context.Context, cfg *ports.BridgeConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyConcurrentLocked()
	clone := *cfg
	clone.Version = 1
	if s.current != nil {
		clone.Version = s.current.Version + 1
	}
	s.current = &clone
	s.saves = append(s.saves, &clone)
	cfg.Version = clone.Version
	return nil
}

func (s *casConfigStore) SaveIfVersion(_ context.Context, cfg *ports.BridgeConfig, expected int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyConcurrentLocked()
	stored := 0
	if s.current != nil {
		stored = s.current.Version
	}
	if stored != expected {
		return shared.ErrVersionMismatch
	}
	clone := *cfg
	clone.Version = expected + 1
	s.current = &clone
	s.saves = append(s.saves, &clone)
	cfg.Version = clone.Version
	return nil
}

func (s *casConfigStore) Validate(_ context.Context, _ *ports.BridgeConfig) ([]string, error) {
	return nil, nil
}

func (s *casConfigStore) Merge(_ context.Context, _, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	clone := *overlay
	return &clone, nil
}

var _ ports.ConditionalConfigStore = (*casConfigStore)(nil)

// configLoadStore injects source errors while preserving the CAS write behavior.
type configLoadStore struct {
	*casConfigStore
	load func(context.Context) (*ports.BridgeConfig, error)
}

func (s *configLoadStore) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	return s.load(ctx)
}

var _ ports.ConditionalConfigStore = (*configLoadStore)(nil)

type initialConfigStore struct{ casConfigStore }

func (s *initialConfigStore) CreateIfAbsent(_ context.Context, cfg *ports.BridgeConfig) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil {
		return false, nil
	}
	copy := *cfg
	copy.Version = 1
	s.current = &copy
	return true, nil
}
