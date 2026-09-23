package paho

import (
	"context"
	"sort"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// setManagedCleanupFilters atomically replaces the exact history-minus-desired
// gate and the desired filters it was computed against. No handler match can
// race past a newly installed stale-filter gate.
func (r *router) setManagedCleanupFilters(cleanup, desired []string) {
	if r == nil {
		return
	}
	cleanupCopy := append([]string(nil), cleanup...)
	sort.Strings(cleanupCopy)
	desiredCopy := append([]string(nil), desired...)
	sort.Strings(desiredCopy)
	r.mu.Lock()
	r.managedCleanupFilters = cleanupCopy
	r.managedDesiredFilters = desiredCopy
	r.mu.Unlock()
}

// wantedByDesiredLocked reports whether a still-desired filter covers topic.
// Caller holds r.mu.
func (r *router) wantedByDesiredLocked(topic string) bool {
	return len(r.managedDesiredFilters) > 0 && matchesAnyFilter(r.managedDesiredFilters, topic)
}

// quiesceForRecycle waits for active handler dispatch, purges old-epoch pending
// deliveries without ACK, and makes subsequent old-socket ingress discard-only.
//
// It takes two contexts because the phases fail for different reasons and are
// owned by different layers: teardownCtx bounds the adapter's own work (see
// Session.quiesceForRecycle), while settleCtx bounds the runtime settlement
// barrier, whose duration belongs to the routes. Passing the tighter teardown
// bound to the settlement wait would classify cooperative downstream slowness
// as a drain failure, which the recovery treats as unrecoverable.
func (r *router) quiesceForRecycle(
	teardownCtx, settleCtx context.Context,
	waitSettlement func(context.Context) error,
) error {
	if r == nil {
		return nil
	}
	// The state transition is under the same mutex dispatchCore uses before
	// handler matching. It therefore stops acceptance immediately without
	// waiting behind a stuck callback.
	r.mu.Lock()
	r.quiesced = true
	r.discarding = true
	r.connEpoch++
	r.clearUnsettledLocked()
	r.purgeStalePendingLocked()
	// This closes a generation; it does not open one. The old socket is still
	// live until the session reports it torn down, so nothing arriving before
	// that report may lift the discard window this recycle just raised.
	r.replacementPending = false
	r.generationOpenedByClient = false
	callbacks := r.callbacksInFlight
	idle := r.callbacksIdle
	r.mu.Unlock()

	if callbacks > 0 {
		select {
		case <-idle:
		case <-teardownCtx.Done():
			return teardownCtx.Err()
		}
	}
	// Callback return only means the RouteRunner accepted a Delivery. Its
	// authoritative in-flight counter remains non-zero through processing and
	// settlement, so wait on the runtime-installed barrier as the second phase.
	if waitSettlement != nil {
		if err := waitSettlement(settleCtx); err != nil {
			return err
		}
	}
	return nil
}

// addCallbacksLocked records callbacks before dispatch releases mu, closing the
// match-to-quiesce race where a callback had been selected but not yet added to
// a WaitGroup. Caller must hold r.mu.
func (r *router) addCallbacksLocked(n int) {
	if n <= 0 {
		return
	}
	if r.callbacksInFlight == 0 {
		r.callbacksIdle = make(chan struct{})
	}
	r.callbacksInFlight += n
}

func (r *router) callbackDone() {
	r.mu.Lock()
	// fanout is an internal legacy test seam that can be invoked directly,
	// outside dispatch selection. Such calls are still covered by r.wg for Close
	// but were never accepted into the recycle counter.
	if r.callbacksInFlight > 0 {
		r.callbacksInFlight--
		if r.callbacksInFlight == 0 && r.callbacksIdle != nil {
			close(r.callbacksIdle)
		}
	}
	r.mu.Unlock()
}

// resumeManagedDispatch releases the recycle gate only after replacement
// convergence and routes buffered replacement deliveries before live traffic.
func (r *router) resumeManagedDispatch(ctx context.Context) error {
	if r == nil {
		return nil
	}
	// Keep quiesced true while draining entries that have a handler NOW. An
	// unmatched current-epoch entry stays pending for RegisterFiltered instead of
	// being extracted, rebuffered by dispatchCore, and selected again in a busy
	// loop. Process one entry at a time so cancellation is checked between
	// callbacks without losing an extracted batch.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			return err
		}
		r.discarding = false
		idx := -1
		for i := range r.pending {
			pending := r.pending[i]
			if pending.epoch != r.connEpoch ||
				(len(r.managedCleanupFilters) > 0 && matchesAnyFilter(r.managedCleanupFilters, pending.pub.Topic)) {
				continue
			}
			for _, handler := range r.handlers {
				if matchesAnyFilter(handler.filters, pending.pub.Topic) {
					idx = i
					break
				}
			}
			if idx >= 0 {
				break
			}
		}
		if idx < 0 {
			r.quiesced = false
			r.mu.Unlock()
			return nil
		}
		pending := r.pending[idx]
		copy(r.pending[idx:], r.pending[idx+1:])
		r.pending = r.pending[:len(r.pending)-1]
		r.pendingBytes -= pubBytes(pending.pub)
		epoch := r.connEpoch
		r.mu.Unlock()
		r.dispatchCore(pending.pub, pending.ack, epoch, false, true)
	}
}

// deadLetterPending hands every buffered delivery of the current connection
// generation that matches filters to write, and acknowledges each one only after
// write returned nil. A delivery a still-desired filter also covers is live
// traffic, not a removed filter's residue: it stays buffered for dispatch. An
// acknowledged entry leaves the buffer and gives its dispatch reservation back.
// It stops at the first write or acknowledgement failure and returns it; that
// entry and every later one stay buffered with their acknowledgement intact.
// write and ack run with r.mu released: the acknowledgement wrapper takes r.mu.
func (r *router) deadLetterPending(
	ctx context.Context,
	filters []string,
	write func(context.Context, *messaging.Envelope, string) error,
) error {
	if r == nil || len(filters) == 0 {
		return nil
	}
	type held struct {
		pub    *pahov5.Publish
		ack    func() error
		filter string
	}
	r.mu.RLock()
	var matched []held
	for _, pending := range r.pending {
		if pending.epoch != r.connEpoch || r.wantedByDesiredLocked(pending.pub.Topic) {
			continue
		}
		for _, filter := range filters {
			if matchTopicFilter(filter, pending.pub.Topic) {
				matched = append(matched, held{pub: pending.pub, ack: pending.ack, filter: filter})
				break
			}
		}
	}
	r.mu.RUnlock()

	for _, entry := range matched {
		if err := write(ctx, EnvelopeFromPublish(entry.pub, r.clk, r.metrics), entry.filter); err != nil {
			return err
		}
		if entry.ack != nil {
			if err := entry.ack(); err != nil {
				return err
			}
		}
		r.mu.Lock()
		for i := range r.pending {
			if r.pending[i].pub == entry.pub {
				r.pending = append(r.pending[:i], r.pending[i+1:]...)
				r.pendingBytes -= pubBytes(entry.pub)
				r.releaseQueueReservationLocked(entry.pub)
				break
			}
		}
		r.mu.Unlock()
	}
	return nil
}

// awaitManagedReplay waits through the current connection startup-grace window
// for a broker-pinned replay matching filters. The managed gate remains active,
// so any match stays buffered and unacknowledged. Only deliveries of the
// current connection generation count, the same ones deadLetterPending can
// settle. A delivery a still-desired filter also covers is not a replay of the
// removed filters and is ignored. Routers without a live grace generation (unit
// fakes/direct dispatch) return their immediate snapshot.
func (r *router) awaitManagedReplay(ctx context.Context, filters []string) (bool, error) {
	if r == nil || len(filters) == 0 {
		return false, nil
	}
	r.mu.RLock()
	hook := r.awaitManagedReplayHook
	r.mu.RUnlock()
	if hook != nil {
		hook()
	}
	for {
		r.mu.RLock()
		for _, pending := range r.pending {
			if pending.epoch == r.connEpoch && matchesAnyFilter(filters, pending.pub.Topic) &&
				!r.wantedByDesiredLocked(pending.pub.Topic) {
				r.mu.RUnlock()
				return true, nil
			}
		}
		if !r.graceStarted {
			r.mu.RUnlock()
			return false, nil
		}
		deadline := r.graceDeadline
		changed := r.pendingChanged
		remaining := deadline.Sub(r.clk.Now())
		r.mu.RUnlock()
		if remaining <= 0 {
			return false, nil
		}
		timer := r.clk.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C():
			return false, nil
		}
	}
}
