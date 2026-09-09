package file

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// fileObservation is confined to the existing watch loop (and its synchronous
// initial read). Blocking delivery preserves observed absence across recreation.
type fileObservation struct {
	ctx   context.Context
	out   chan ports.ConfigObservation
	kind  ports.ConfigObservationKind
	fault string
}

// Observe uses the configured notify/poll loop, with an explicit initial read.
// It is mutually exclusive with Watch on this Watcher.
func (w *Watcher) Observe(ctx context.Context) (<-chan ports.ConfigObservation, error) {
	out := make(chan ports.ConfigObservation, 1)
	_, err := w.watch(ctx, &fileObservation{ctx: ctx, out: out})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (w *Watcher) initialObservation() {
	if w.observation == nil {
		return
	}
	data, err := w.readFile(w.path)
	if err != nil {
		w.observeReadError(classifyReadError(w.path, err))
	} else if w.emitParsed(data, nil) {
		w.lastHash = sha256.Sum256(data)
	}
}

func (w *Watcher) observeResult(cfg *ports.BridgeConfig, err error) bool {
	kind := ports.ConfigPresent
	if err != nil {
		kind = ports.ConfigReadError
	}
	return w.observe(kind, cfg, err)
}

func (w *Watcher) observeReadError(err error) bool {
	kind := ports.ConfigReadError
	if errors.Is(err, shared.ErrNotFound) {
		kind = ports.ConfigMissing
	}
	return w.observe(kind, nil, err)
}

func (w *Watcher) observe(kind ports.ConfigObservationKind, cfg *ports.BridgeConfig, err error) bool {
	o := w.observation
	if o == nil {
		return false
	}
	fault := ""
	if err != nil {
		fault = err.Error()
		w.lastHash = [sha256.Size]byte{} // unchanged content must recover health.
		if kind == o.kind && fault == o.fault {
			return true
		}
	}
	next := ports.ConfigObservation{Kind: kind, Config: cfg, Err: err, Sequence: w.observationSequence + 1}
	select {
	case <-o.ctx.Done():
		return false
	case <-w.stopCh:
		return false
	case o.out <- next:
		w.observationSequence++
		o.kind, o.fault = kind, fault
		return true
	}
}

func (w *Watcher) closeObservation() {
	if w.observation != nil {
		close(w.observation.out)
		w.observation = nil
	}
}

var _ ports.ConfigObserver = (*Watcher)(nil)
