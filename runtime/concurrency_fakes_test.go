package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/domain"
)

// ConcurrentSender is a test sender that does NOT hold a mutex during
// SendFn execution, allowing true concurrent send calls. This is
// required for tests that measure concurrency peaks — the standard
// FakeSender serializes through its mu.Lock.
type ConcurrentSender struct {
	sendFn    func(*domain.Envelope) error
	mu        sync.Mutex
	sent      []*domain.Envelope
	sentCount int64
}

func NewConcurrentSender(fn func(*domain.Envelope) error) *ConcurrentSender {
	return &ConcurrentSender{sendFn: fn}
}

func (s *ConcurrentSender) Send(_ context.Context, env *domain.Envelope) error {
	if err := s.sendFn(env); err != nil {
		return err
	}
	atomic.AddInt64(&s.sentCount, 1)
	s.mu.Lock()
	s.sent = append(s.sent, env.Clone())
	s.mu.Unlock()
	return nil
}

func (s *ConcurrentSender) SentCount() int {
	return int(atomic.LoadInt64(&s.sentCount))
}

func (s *ConcurrentSender) GetSent() []*domain.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]*domain.Envelope, len(s.sent))
	copy(cp, s.sent)
	return cp
}
