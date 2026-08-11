package paho

import (
	"context"
	"log/slog"
	"sync"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// withRouterClock injects the clock sourcing the startup grace window.
// A nil clock keeps the clock.System default.
func withRouterClock(clk clock.Clock) routerOption {
	return func(r *router) {
		if clk != nil {
			r.clk = clk
		}
	}
}

// withUnmatchedGrace sets the startup grace window. Values <= 0 keep the
// DefaultUnmatchedGrace default.
func withUnmatchedGrace(d time.Duration) routerOption {
	return func(r *router) {
		if d > 0 {
			r.graceWindow = d
		}
	}
}

// withUnsubscribe installs the callback the router uses to unsubscribe an
// orphan topic identified past the grace window. Nil disables the
// unsubscribe-on-resume hygiene (ack-and-drop still happens).
func withUnsubscribe(fn func(topic string)) routerOption {
	return func(r *router) { r.unsubscribe = fn }
}

// withCovered installs the predicate the router uses to tell a REAL
// live-route loss (a still-desired subscription whose handler registered
// late) from benign orphan cleanup when it acks-and-drops a publish past
// the grace window. Nil treats every post-grace drop as an orphan
// (the legacy/test behaviour).
func withCovered(fn func(topic string) bool) routerOption {
	return func(r *router) { r.covered = fn }
}

// withSessionTag records the session's client_id so router loss/drop metrics
// carry a session_id tag. Empty values are ignored (the tag is omitted).
func withSessionTag(id string) routerOption {
	return func(r *router) { r.sessionID = id }
}

func withDispatchCapacity(capacity int) routerOption {
	return func(r *router) {
		if capacity > 0 {
			r.dispatchSize = capacity
		}
	}
}

func withMaxPayloadBytes(maxPayloadBytes uint32) routerOption {
	return func(r *router) { r.maxPayloadBytes = maxPayloadBytes }
}

func newRouter(logger *slog.Logger, metrics ports.MetricsExporter, opts ...routerOption) *router {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	r := &router{
		handlers:          make(map[string]routerHandler),
		pendingChanged:    make(chan struct{}),
		queueChanged:      make(chan struct{}),
		queueReservations: make(map[*pahov5.Publish]struct{}),
		pendingLimit:      defaultPendingLimit,
		pendingBytesLimit: defaultPendingBytesLimit,
		dispatchSize:      defaultDispatchSize,
		clk:               clock.System,
		graceWindow:       DefaultUnmatchedGrace,
		unsubscribed:      make(map[string]struct{}),
		coveredWarned:     make(map[string]struct{}),
		poisonLogged:      make(map[string]struct{}),
		stop:              make(chan struct{}),
		logger:            logger,
		metrics:           metrics,
	}
	for _, opt := range opts {
		opt(r)
	}
	// Seed the grace deadline so an unmatched publish arriving before the
	// first beginGrace (no connection yet) is buffered, not dropped.
	r.graceDeadline = r.clk.Now().Add(r.graceWindow)
	return r
}

// setPendingLimit bounds the pending buffer; values < 1 keep the
// current limit. Called by NewSession with the session's effective
// Receive Maximum.
func (r *router) setPendingLimit(n int) {
	if n < 1 {
		return
	}

	r.mu.Lock()
	r.pendingLimit = n
	r.mu.Unlock()
}

func (r *router) ingressMemoryStats() (depth, capacity int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.dispatchCh != nil {
		depth = len(r.dispatchCh)
	}
	return depth, r.dispatchSize
}

// sessionTag returns the session_id metric tag for this router's drops, or
// nil when the router was built without a session (legacy/test). Spread it
// into a metrics Counter call: r.metrics.Counter(name, n, r.sessionTag()...).
func (r *router) sessionTag() []shared.Tag {
	if r.sessionID == "" {
		return nil
	}
	return []shared.Tag{{Key: shared.TagKeySessionID, Value: r.sessionID}}
}

// shutdown terminates the grace and dispatch worker goroutines and marks
// the router closing so RegisterFiltered's pending-flush stops producing
// into r.wg. Idempotent; called by Session.Close BEFORE awaitDispatchLoop
// + Wait.
func (r *router) shutdown() {
	r.mu.Lock()
	r.closing = true
	clear(r.queueReservations)
	r.queueReserved = 0
	close(r.queueChanged)
	r.queueChanged = make(chan struct{})
	r.mu.Unlock()
	r.stopOnce.Do(func() { close(r.stop) })
}

// Wait blocks until all in-flight handler goroutines have returned.
func (r *router) Wait() { r.wg.Wait() }

// Register adds a match-all handler with LEGACY auto-settle semantics:
// the protocol ack (when the publish carries one) fires as soon as the
// handler returns. Production receivers use RegisterEnvelope /
// RegisterFiltered so the ack can be deferred to delivery settlement;
// Register exists for tests and diagnostic taps.
func (r *router) Register(id string, h func(*pahov5.Publish)) {
	r.RegisterFiltered(id, nil, func(pub *pahov5.Publish, ack func() error) {
		h(pub)
		if ack != nil {
			_ = ack()
		}
	})
}

// RegisterFiltered adds a settlement-aware handler for the given ID,
// dispatched only for topics matching filters (empty filters = match
// all). Handlers receive an independent copy of the Publish struct,
// Payload, and Properties per invocation when multiple handlers match a
// publish. Handlers should treat Properties as read-only. Pending
// (pre-registration) publishes matching the filters are flushed to the
// new handler in arrival order.
func (r *router) RegisterFiltered(id string, filters []string, h func(*pahov5.Publish, func() error)) {
	entry := routerHandler{
		fn:       h,
		filters:  filters,
		emitMu:   &sync.Mutex{},
		inflight: &sync.WaitGroup{},
	}
	r.mu.Lock()
	r.handlers[id] = entry
	flush := r.takePendingLocked(filters)
	// Skip the flush once Close has begun (r.closing set under r.mu by
	// shutdown): the flush goroutine's r.wg.Add would otherwise race
	// Close's r.wg.Wait. The mutex orders this decision against shutdown,
	// so either we Add before shutdown-before-Wait (safe) or we observe
	// closing and skip entirely. Skipped pending stays un-acked → the
	// broker redelivers to the next owner (at-least-once preserved).
	doFlush := len(flush) > 0 && !r.closing
	if doFlush {
		// Lock emitMu WHILE still holding r.mu (uncontended — brand-new
		// handler) so any live dispatch that observes this handler after
		// r.mu is released blocks on emitMu until the flush of the (older)
		// buffered publishes completes. That funnels flush and live
		// dispatch through ONE serialized, in-order path (ports.Receiver
		// sequential-emit contract). inflight/wg track the flush so
		// Unregister and Session.Close both await it.
		entry.emitMu.Lock()
		entry.inflight.Add(1)
		r.wg.Add(1)
		r.addCallbacksLocked(1)
	}
	r.mu.Unlock()

	if doFlush {
		go func() {
			defer r.wg.Done()
			defer r.callbackDone()
			defer entry.inflight.Done()
			defer entry.emitMu.Unlock()
			r.mu.Lock()
			ready := flush
			if r.discarding {
				for _, pending := range flush {
					r.releaseQueueReservationLocked(pending.pub)
				}
				// Flush entries taken before the recycle began: same
				// recycle-window discard class, same metric.
				r.stalePurged.Add(int64(len(flush)))
				r.metrics.Counter(MetricMQTTRouterStalePurged, int64(len(flush)), r.sessionTag()...)
				ready = nil // old socket ACKs die with the recycled generation
			} else if r.quiesced || len(r.managedCleanupFilters) > 0 {
				ready = make([]pendingPublish, 0, len(flush))
				blocked := make([]pendingPublish, 0, len(flush))
				for _, pending := range flush {
					if r.quiesced || matchesAnyFilter(r.managedCleanupFilters, pending.pub.Topic) {
						blocked = append(blocked, pending)
						r.pendingBytes += pubBytes(pending.pub)
					} else {
						ready = append(ready, pending)
					}
				}
				if len(blocked) > 0 {
					r.pending = append(blocked, r.pending...)
				}
			}
			r.mu.Unlock()
			for _, pending := range ready {
				r.releaseQueueReservation(pending.pub)
				r.emitOne(entry, pending.pub, pending.ack)
			}
		}()
	}

	if logging.DebugEnabled(r.logger) {
		flushed := 0
		if doFlush {
			flushed = len(flush)
		}
		r.logger.Log(context.Background(), logging.LevelDebug, "mqtt: handler registered",
			"handler_id", id,
			"filter_count", len(filters),
			"flushed_pending", flushed,
			"total_handlers", r.HandlerCount(),
		)
	}
}

// emitOne invokes handler.fn with an immutable shallow Publish copy, recovering
// panics into the handler-panic metric. Payload/properties backing is shared and
// must remain read-only. It does NOT touch emitMu,
// inflight or the shared WaitGroup — the caller owns that bookkeeping.
// Used by the pending-flush path, where a single handler consumes the
// publish so its Properties pointer may be shared safely.
func (r *router) emitOne(handler routerHandler, pub *pahov5.Publish, ack func() error) {
	p := *pub
	defer func() {
		if rv := recover(); rv != nil {
			r.metrics.Counter(MetricMQTTHandlerPanics, 1)
			if r.logger != nil {
				r.logger.Error("mqtt: handler panicked",
					"recovered", rv,
					"topic", p.Topic,
				)
			}
		}
	}()
	handler.fn(&p, ack)
}

// takePendingLocked removes and returns — preserving arrival order —
// every pending publish whose topic matches filters. Caller holds r.mu.
func (r *router) takePendingLocked(filters []string) []pendingPublish {
	if len(r.pending) == 0 || r.quiesced || r.discarding {
		return nil
	}
	var flush []pendingPublish
	keep := r.pending[:0]
	for _, p := range r.pending {
		if matchesAnyFilter(filters, p.pub.Topic) && (len(r.managedCleanupFilters) == 0 || !matchesAnyFilter(r.managedCleanupFilters, p.pub.Topic)) {
			flush = append(flush, p)
			r.pendingBytes -= pubBytes(p.pub)
		} else {
			keep = append(keep, p)
		}
	}
	r.pending = keep
	return flush
}

func (r *router) Unregister(id string) {
	r.mu.Lock()
	entry, ok := r.handlers[id]
	delete(r.handlers, id)
	r.mu.Unlock()

	if ok && entry.inflight != nil {
		// Await any in-flight dispatch to this handler so emit is never
		// invoked after Unregister returns — and therefore never after
		// the owning Receiver.Run has returned (ports.Receiver emit
		// lifetime). The delete above (under r.mu) guarantees no new
		// dispatch can add to inflight, so this Wait drains only the
		// already-started emits.
		entry.inflight.Wait()
	}

	// Re-arm the unmatched grace window: a receiver being unregistered is
	// often a restart (its Run exited and the supervisor will re-register),
	// so publishes arriving in the unregister→re-register gap must be
	// BUFFERED for the replacement handler, not acked-and-dropped past a
	// long-expired grace deadline (covered-topic QoS 1/2 loss window).
	r.rearmGrace()

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelDebug, "mqtt: handler unregistered",
			"handler_id", id,
		)
	}
}
