package config

import (
	"context"
	"crypto/sha256"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Observe observes exactly one authoritative layer. Overlay absence has no
// defined merge policy, so layered observation is explicitly unsupported.
// Source read/validation faults leave desired and running state untouched.
// A terminated observer is reported as a read fault and restarted with backoff.
// Every Present emission has a distinct Config pointer. Consumers acknowledge
// that emitted pointer via NotifyApplyResult, never the source's original one.
func (m *Manager) Observe(ctx context.Context) (<-chan ports.ConfigObservation, error) {
	if len(m.overlays) != 0 {
		return nil, shared.ErrNotSupported.WithMessage("config observation requires one authoritative layer")
	}
	observer, ok := m.base.Watcher.(ports.ConfigObserver)
	if !ok {
		observer, ok = m.base.Loader.(ports.ConfigObserver)
	}
	if !ok {
		return nil, shared.ErrNotSupported.WithMessage("config layer does not support observation")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return nil, errAlreadyRunning
	}
	watchCtx, cancel := context.WithCancel(ctx)
	m.running, m.stopping = true, false
	m.stopCh, m.doneCh = make(chan struct{}), make(chan struct{})
	m.observationCancel = cancel
	out := make(chan ports.ConfigObservation)
	go m.observationLoop(watchCtx, cancel, observer, out, m.doneCh)
	return out, nil
}

func (m *Manager) observationLoop(ctx context.Context, cancel context.CancelFunc, observer ports.ConfigObserver, out chan ports.ConfigObservation, done chan struct{}) {
	defer func() {
		cancel()
		m.mu.Lock()
		m.running = false
		m.observationCancel = nil
		close(out)
		close(done)
		m.mu.Unlock()
	}()
	forward := func(o ports.ConfigObservation) bool {
		o = m.acceptObservation(o)
		m.observationSequence++
		o.Sequence = m.observationSequence
		select {
		case <-ctx.Done():
			return false
		case out <- o:
			return true
		}
	}
	backoff := watchRetryInitial
	for ctx.Err() == nil {
		ch, err := observer.Observe(ctx)
		if err == nil && ch == nil {
			err = shared.ErrUnavailable.WithMessage("config observer returned a nil channel")
		}
		if err == nil {
			for err == nil {
				select {
				case <-ctx.Done():
					return
				case o, ok := <-ch:
					if !ok {
						err = errWatchEnded
					} else {
						if !forward(o) {
							return
						}
						backoff = watchRetryInitial
					}
				}
			}
		}
		if !forward(ports.ConfigObservation{Kind: ports.ConfigReadError, Err: err}) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-m.clk.After(backoff):
			backoff = nextBackoff(backoff)
		}
	}
}

func (m *Manager) acceptObservation(o ports.ConfigObservation) ports.ConfigObservation {
	switch o.Kind {
	case ports.ConfigPresent:
		if o.Err != nil || o.Config == nil {
			o.Err = shared.ErrInvalidConfig.WithMessage("invalid present config observation")
		} else {
			o.Err = Validate(o.Config)
		}
		if o.Err == nil {
			// The source may reuse an immutable snapshot. Share its immutable
			// graph, but give each emit a fresh identity so an old acknowledgement
			// cannot match a later Present after absence or an observer restart.
			emitted := *o.Config
			o.Config = &emitted
			m.mu.Lock()
			m.configs[m.base.Name] = o.Config
			m.mu.Unlock()
			m.clearWatchError(m.base.Name)
			m.recordAppliedVersion(o.Config) // preserve the exact acknowledgement pointer.
			return o
		}
	case ports.ConfigMissing:
		if o.Config != nil || (o.Err != nil && !initialMissing(o.Err)) {
			o.Err = shared.ErrInvalidConfig.WithMessage("invalid missing config observation")
			break
		}
		o.Err = shared.ErrNotFound
		m.mu.Lock()
		delete(m.configs, m.base.Name)
		delete(m.watchErrs, m.base.Name)
		m.appliedVersion, m.desiredConfig = -1, nil
		m.desiredFingerprint = [sha256.Size]byte{}
		m.desiredHashErr, m.lastApplyErr = nil, nil
		m.mu.Unlock()
		return o
	case ports.ConfigReadError:
		if o.Err == nil {
			o.Err = shared.ErrUnavailable.WithMessage("config observation failed without an error")
		}
	default:
		o.Err = shared.ErrInvalidConfig.WithMessage("unknown config observation kind")
	}
	o.Kind, o.Config = ports.ConfigReadError, nil
	m.setWatchError(m.base.Name, o.Err)
	return o
}

// NotifyIdle acknowledges completed data-plane quiescence. A newer desired
// snapshot may already be queued, so this clears only confirmed RUNNING state.
// Missing observations separately invalidate the old desired acknowledgement.
func (m *Manager) NotifyIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runningVersion = -1
	m.runningFingerprint = [sha256.Size]byte{}
	m.lastApplyErr = nil
}

var _ ports.ConfigObserver = (*Manager)(nil)
