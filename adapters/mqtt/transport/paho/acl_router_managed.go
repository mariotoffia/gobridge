package paho

import (
	"context"
	"sort"
)

// setManagedCleanupFilters atomically replaces the exact history-minus-desired
// gate. No handler match can race past a newly installed stale-filter gate.
func (r *router) setManagedCleanupFilters(filters []string) {
	if r == nil {
		return
	}
	copyFilters := append([]string(nil), filters...)
	sort.Strings(copyFilters)
	r.mu.Lock()
	r.managedCleanupFilters = copyFilters
	r.mu.Unlock()
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

// pendingMatching reports how many buffered publishes match exact managed
// filters. It never settles or removes entries; migration uses it after a
// cleanup recycle to detect broker-pinned QoS1 deliveries and fail closed.
func (r *router) pendingMatching(filters []string) int {
	if r == nil || len(filters) == 0 {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, pending := range r.pending {
		if matchesAnyFilter(filters, pending.pub.Topic) {
			count++
		}
	}
	return count
}

// awaitManagedReplay waits through the current connection startup-grace window
// for a broker-pinned replay matching filters. The managed gate remains active,
// so any match stays buffered and unacknowledged. Routers without a live grace
// generation (unit fakes/direct dispatch) return their immediate snapshot.
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
			if matchesAnyFilter(filters, pending.pub.Topic) {
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
