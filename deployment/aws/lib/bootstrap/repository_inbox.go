package bootstrap

import (
	"sync"

	"github.com/mariotoffia/gobridge/ports"
)

// repositoryInbox never blocks observation intake on runtime construction.
// Every Missing is fenced immediately by intake. The pending drain barrier
// survives later Present events; only the latest snapshot after that barrier
// can activate. Repeated withdrawals before a drain share that drain, while
// their epochs still invalidate every intervening candidate.
type repositoryInbox struct {
	mu      sync.Mutex
	changed chan struct{}
	missing *repositoryEvent
	latest  *repositoryEvent
	closed  bool
}

func newRepositoryInbox() *repositoryInbox {
	return &repositoryInbox{changed: make(chan struct{}, 1)}
}

func (q *repositoryInbox) put(ev repositoryEvent) {
	q.mu.Lock()
	if ev.observation.Kind == ports.ConfigMissing {
		q.missing, q.latest = &ev, nil
	} else {
		q.latest = &ev
	}
	q.mu.Unlock()
	q.signal()
}

func (q *repositoryInbox) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *repositoryInbox) signal() {
	select {
	case q.changed <- struct{}{}:
	default:
	}
}

func (q *repositoryInbox) take() (repositoryEvent, bool, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var next *repositoryEvent
	if q.missing != nil {
		next, q.missing = q.missing, nil
	} else {
		next, q.latest = q.latest, nil
	}
	if q.missing != nil || q.latest != nil || (next != nil && q.closed) {
		q.signal()
	}
	if next == nil {
		return repositoryEvent{}, false, q.closed
	}
	return *next, true, false
}
