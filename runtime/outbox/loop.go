package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// transientRetryFloor is the minimum wait before re-polling a partition
// after a stale-token or transient-egress failure, so a down broker is not
// hammered every poll interval and a transient outage does not burn the
// replay/poison budget in a few seconds.
const transientRetryFloor = 5 * time.Second

// drainWedgeGrace is how long past a batch's own computed deadline wg.Wait may
// run before the batch is flagged as wedged and ABANDONED (Min2). It is
// deliberately generous: a well-behaved batch returns at or shortly after
// batchTimeout (workCtx's deadline cancels in-flight sends, which honor ctx and
// return promptly), so only a Sender that ignores context cancellation runs
// this far past the deadline. The watchdog never kills the in-flight goroutine
// (senders MUST honor ctx; we cannot force-return a hung Send), but it IS the
// hard ceiling on how long the drainer itself will wait: once it fires the
// drainer stops waiting, records the stall, and proceeds so it can never wedge.
const drainWedgeGrace = 30 * time.Second

// Run polls the outbox for pending records and sends them. It blocks
// until the context is cancelled or a fencing error occurs. The polling
// interval is determined by the configured DrainStrategy.
func (d *Drainer) Run(ctx context.Context) error {
	timer := d.clk.NewTimer(d.strategy.NextInterval(0))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			if drainErr := d.finalDrain(ctx); drainErr != nil {
				d.log(ctx, slog.LevelWarn, "final drain error during shutdown", "error", drainErr)
			}
			return ctx.Err()
		case <-timer.C():
			token, hasLease := d.tokenFn()
			if !hasLease {
				// Finding 11: a non-exclusive session never acquires a lease, so
				// its shared_outbox drainer would skip EVERY cycle and never
				// drain — previously a fully silent stall. Surface it: a counter
				// on every skip and a rate-limited warning so the permanent
				// stall is observable (validation also rejects the static combo).
				d.metrics.Counter(shared.MetricDrainSkippedNoLease, 1,
					shared.Tag{Key: shared.TagKeySessionID, Value: d.partitionKey},
					shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID})
				d.warnDrainSkippedNoLease(ctx)
				timer.Reset(d.strategy.NextInterval(0))
				continue
			}
			// Finding 19: sweep expired-but-unclaimed pending records to the
			// expired terminal state at a slow cadence. Runs under lease
			// ownership (checked above) and independent of egress readiness —
			// expiry is a durability-cleanup concern, not a send concern.
			d.maybeExpire(ctx)
			if d.readyFn != nil && !d.readyFn(ctx) {
				if logging.TraceEnabled(d.logger) {
					d.logger.Log(ctx, logging.LevelTrace, "egress not ready, skipping drain cycle",
						"partition_key", d.partitionKey)
				}
				timer.Reset(d.strategy.NextInterval(0))
				continue
			}
			d.hasDrained = true
			n, transient, err := d.drainBatch(ctx, token)
			if err != nil {
				if errors.Is(err, shared.ErrStaleFencingToken) {
					d.log(ctx, slog.LevelWarn, "stale fencing token, waiting for new lease")
					staleBackoff := max(d.strategy.NextInterval(0), transientRetryFloor)
					timer.Reset(staleBackoff)
					continue
				}
				d.log(ctx, slog.LevelWarn, "drain batch error", "error", err)
			}
			d.adaptBatchSize(n)
			interval := d.strategy.NextInterval(n)
			if transient > 0 {
				// The 5s floor stops broker hammering and the sub-30s
				// poison-DLQ on typical restarts. It throttles the RATE at
				// which transient retries burn replay_count; the wall-clock
				// ReplayBudget (measured from FirstAttemptedAt in
				// replayBudgetExhausted) now bounds the TOTAL burn, so an
				// outage no longer DLQs a good message merely by exhausting
				// the claim count quickly. The age-based root-cause fix
				// (WP-REPLAY-BUDGET) is implemented across the aggregate,
				// snapshot, and all three stores.
				interval = max(interval, transientRetryFloor)
			}
			timer.Reset(interval)
		}
	}
}

// finalDrain performs one last drain after Run's context is cancelled.
// Uses context.WithoutCancel(parent)+WithTimeout so the drain can complete
// during shutdown while retaining trace/correlation values from the parent.
// Claim is bounded by the batch ceiling; in-flight sends may exceed it.
func (d *Drainer) finalDrain(parent context.Context) error {
	if !d.hasDrained {
		return nil
	}
	token, hasLease := d.tokenFn()
	if !hasLease {
		return nil
	}
	// Skip final drain if the egress transport is not connected.
	// Draining into a disconnected sender wastes replay_count budget
	// and the records will be re-claimed on the next healthy drain cycle.
	// parent is typically cancelled at this point; detach cancellation but
	// preserve values so Health can still be queried.
	if d.readyFn != nil && !d.readyFn(context.WithoutCancel(parent)) {
		return nil
	}
	// Use the worst-case ceiling for the final drain because the batch
	// size is only known after Claim. When the caller opted into the
	// scaled formula this resolves to MaxDrainTimeout; otherwise it
	// resolves to the legacy fixed DrainTimeout.
	finalCeiling := d.drainTimeout
	if d.useScaledTimeout {
		finalCeiling = d.maxDrainTimeout
		if finalCeiling <= 0 {
			finalCeiling = defaultMaxDrainTimeout
		}
	}
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), finalCeiling)
	defer cancel()
	if _, _, err := d.drainBatch(drainCtx, token); err != nil {
		return fmt.Errorf("runtime: outbox-drainer: final drain: %w", err)
	}
	return nil
}

// maybeExpire runs OutboxStore.Expire at most once per expireInterval, sweeping
// this partition's pending records whose expires_at has passed into the expired
// terminal state (finding 19). Without this the port's Expire method is dead
// code and expired records that were never claimed linger pending forever. The
// caller has already confirmed lease ownership, so exactly one instance performs
// the sweep per partition. Expire is pending-only (claimed records are reclaimed
// via Claim, never expired out from under a live owner), so the sweep never
// races an in-flight send; it is partition-scoped (M1) so a drainer holding one
// partition's lease can never expire another partition's records.
//
// H1: the bulk sweep is DEFERRED entirely for on_expired:dlq routes. The sweep
// only transitions records to the terminal "expired" state with a counter — it
// destroys the envelope without routing it — so under the default ExpiredDLQ
// policy a broker outage would erase evidence the operator asked to preserve.
// For DLQ-policy routes, expiry MUST flow through the per-record claim path
// (handleExpired) which routes the expired envelope to the DLQ. The sweep
// therefore runs ONLY for drop-policy routes, where the record would be dropped
// anyway so eager accounting loses no evidence.
func (d *Drainer) maybeExpire(ctx context.Context) {
	if d.outboxStore == nil {
		return
	}
	if d.policy.OnExpired == routing.ExpiredDLQ {
		// DLQ-policy expiry is handled per-record by handleExpired (which
		// preserves the envelope by routing it to the DLQ), never by the
		// evidence-destroying bulk sweep.
		return
	}
	now := d.clk.Now()
	if !d.lastExpire.IsZero() && now.Sub(d.lastExpire) < d.expireInterval {
		return
	}
	d.lastExpire = now
	n, err := d.outboxStore.Expire(ctx, now, d.partitionKey)
	if err != nil {
		d.log(ctx, slog.LevelWarn, "outbox expire sweep failed", "error", err)
		return
	}
	if n > 0 {
		// Expired records were received but will never be sent — count them as
		// expired so the conservation law (received = sent + dropped + filtered
		// + expired + dlq + inflight) can close.
		d.metrics.Counter(shared.MetricMessagesExpired, int64(n),
			shared.Tag{Key: shared.TagKeySessionID, Value: d.partitionKey},
			shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID})
		d.log(ctx, slog.LevelInfo, "expired pending outbox records", "count", n)
	}
}

func (d *Drainer) drainBatch(ctx context.Context, token persistence.LeaseToken) (int, int, error) {
	start := d.clk.Now()
	sessionTag := shared.Tag{Key: shared.TagKeySessionID, Value: d.partitionKey}
	routeTag := shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID}

	if logging.DebugEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelDebug, "drain batch starting",
			"partition_key", d.partitionKey,
			"batch_size", d.currentBatchSize,
		)
	}

	records, err := d.outboxStore.Claim(ctx, d.partitionKey, token, d.currentBatchSize)
	if err != nil {
		return 0, 0, err
	}
	// OutboxDepth from the drainer's own poll cadence: emit the claimed count as
	// the depth gauge on every drain cycle this partition actually runs (lease
	// held AND egress ready), independent of MaxOutboxDepth and of the ingress
	// QueryPending path. This makes the gauge continuous while an outbox exists
	// (it emits 0 when caught up) instead of only under a backpressure config.
	// Tagged TagKeyPartition to share the series with the ingress-side emission
	// (runtime.InstrumentedOutboxStore.QueryPending). The value saturates at
	// currentBatchSize (the Claim ceiling), so it signals "backlog at least this
	// deep", not the exact pending total — an exact count primitive is deferred
	// to the store wave. It does NOT emit while standby (no lease) or while
	// egress is not ready; those gaps are covered by DrainSkippedNoLease and the
	// readiness gate respectively.
	d.metrics.Gauge(shared.MetricOutboxDepth, float64(len(records)),
		shared.Tag{Key: shared.TagKeyPartition, Value: d.partitionKey})
	if len(records) == 0 {
		if d.hadPending {
			d.hadPending = false
			d.fireDrained()

			d.idleMu.Lock()
			d.idleSince = d.clk.Now()
			old := d.idleCh
			d.idleCh = make(chan struct{})
			d.idleMu.Unlock()
			close(old)
		}
		return 0, 0, nil
	}

	d.idleMu.Lock()
	d.idleSince = time.Time{}
	d.idleMu.Unlock()

	d.hadPending = true

	if logging.DebugEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelDebug, "claimed records",
			"count", len(records),
			"partition_key", d.partitionKey,
		)
	}

	sem := make(chan struct{}, d.maxConcurrency)
	var wg sync.WaitGroup
	var successCount int64
	var transientReleases int64
	var deferredReleases int64
	var staleDetected atomic.Bool

	// Group first so the batch budget can account for the longest ordering
	// group (whose records send sequentially) as well as the concurrency waves.
	// Records sharing a non-empty ordering key collapse into one group
	// delivered sequentially by a single goroutine; keyless records form
	// singleton groups that retain full concurrency up to maxConcurrency.
	groups := groupByOrderingKey(records)

	// workCtx gives in-flight goroutines a bounded deadline to finish their
	// send+complete operations even after the parent ctx is cancelled. The
	// budget must never undercut a single record's SendTimeout + Complete
	// margin and it scales with the number of sequential send "waves" the batch
	// needs (finding 10). The previous min(1.5×SendTimeout, 10s-ceiling)
	// silently capped any SendTimeout above ~6.7s (and any batch too large for
	// the ceiling), killing in-flight sends every cycle and stranding/poisoning
	// healthy records. The configured MaxDrainTimeout/DrainTimeout may only
	// RAISE this budget now — it can no longer cut it below one full send.
	//
	// workCtx must survive caller ctx cancellation so in-flight sends can
	// settle cleanly, but retains trace/correlation values from parent.
	batchTimeout := d.batchTimeout(len(records), groups)
	workCtx, workCancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)

	// batchCtx is cancelled when a stale fencing token is detected so
	// that sibling goroutines abort quickly instead of continuing to
	// send with an invalid token (preventing duplicate deliveries).
	batchCtx, batchCancel := context.WithCancel(workCtx)

	// unlaunchedFrom records the first group index that was never launched
	// because the batch deadline/cancel fired; those claimed-but-unattempted
	// groups are released after wg.Wait (finding 9).
	unlaunchedFrom := len(groups)

loop:
	for gi := range groups {
		if ctx.Err() != nil || batchCtx.Err() != nil {
			unlaunchedFrom = gi
			break
		}
		group := groups[gi]
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			unlaunchedFrom = gi
			break loop
		case <-batchCtx.Done():
			unlaunchedFrom = gi
			break loop
		}
		wg.Go(func() {
			defer func() { <-sem }()
			var current string
			defer func() {
				if r := recover(); r != nil {
					d.log(batchCtx, slog.LevelError, "panic in drain goroutine",
						"record_id", current, "panic", r)
					d.metrics.Counter(shared.MetricOutboxRecordFailures, 1, routeTag)
				}
			}()
			// Process this group's records sequentially in persisted order.
			// On the first error (stale or transient) stop the group so a
			// later same-key record can never overtake an earlier one that
			// has not yet succeeded; the unsent remainder is re-claimed and
			// retried in order on a subsequent cycle.
			for ri, rec := range group {
				if ctx.Err() != nil || batchCtx.Err() != nil {
					// Batch deadline fired before this record was attempted:
					// release the unattempted tail (finding 9) so it retries
					// next cycle instead of stranding claimed. Use a detached
					// ctx since batchCtx is already done.
					atomic.AddInt64(&deferredReleases, int64(len(group)-ri))
					d.releaseRemainder(context.WithoutCancel(ctx), group[ri:], token)
					return
				}
				current = rec.ID()
				if rec.ReplayCount() > 1 {
					d.metrics.Counter(shared.MetricOutboxReplayCount, 1, routeTag)
				}
				if err := d.processRecord(batchCtx, rec, token); err != nil {
					switch {
					case errors.Is(err, errBatchDeadlineDeferred):
						// processRecord already released THIS record; release the
						// unsent tail too and count the whole run as deferred so
						// the group is re-claimed and retried in order — never
						// counted as success (finding 9).
						atomic.AddInt64(&deferredReleases, int64(len(group)-ri))
						d.releaseRemainder(context.WithoutCancel(ctx), group[ri+1:], token)
					case errors.Is(err, errReleasedForRetry):
						// Transient egress failure: processRecord already
						// released (or left claimed) this record for retry.
						// Stop the group so a later same-key record cannot
						// overtake it, and release the still-claimed remainder
						// so the whole group is re-claimed and retried in
						// order next cycle instead of stranding the unsent
						// tail. Not counted as a success or a hard failure;
						// drives the backoff floor via transientReleases.
						atomic.AddInt64(&transientReleases, 1)
						d.releaseRemainder(batchCtx, group[ri+1:], token)
					case errors.Is(err, errReleaseFailed):
						// Transient egress failure whose Release ALSO failed with
						// a store error (M4). The head stays durably Claimed. Stop
						// the group WITHOUT counting a success and WITHOUT letting
						// a later same-key record overtake the still-claimed head.
						// Do NOT releaseRemainder: the store that just failed
						// Release will fail it again, so version/stale reclaim
						// recovers the still-claimed head AND the unattempted tail
						// together. Drives the backoff floor via transientReleases.
						atomic.AddInt64(&transientReleases, 1)
					case errors.Is(err, shared.ErrStaleFencingToken):
						staleDetected.Store(true)
						batchCancel()
						d.log(batchCtx, slog.LevelWarn, "record processing failed",
							"record_id", rec.ID(), "error", err)
					default:
						d.metrics.Counter(shared.MetricOutboxRecordFailures, 1, routeTag)
						d.log(batchCtx, slog.LevelWarn, "record processing failed",
							"record_id", rec.ID(), "error", err)
					}
					return
				}
				atomic.AddInt64(&successCount, 1)
			}
		})
	}
	// Min2: a Sender that ignores ctx can block wg.Wait indefinitely, which is
	// otherwise indistinguishable from an idle drainer (both emit nothing). Wait
	// under a watchdog: if the batch outlives its own deadline by a generous
	// grace, emit MetricOutboxDrainStalled and a WARN, then STOP waiting and
	// proceed — abandoning the still-in-flight send — so a ctx-ignoring sender
	// can never wedge this drainer (blocking every other record and shutdown).
	// The abandoned record was never Completed (Complete only runs after Send
	// returns nil), so it stays Claimed and is re-drained later: at-least-once
	// is preserved, nothing is falsely completed. Killing the in-flight
	// goroutine is impossible in Go — senders MUST honor ctx.
	d.waitBatch(ctx, &wg, batchTimeout, start, sessionTag, routeTag)
	batchCancel()
	workCancel()

	// Release any groups never launched because the batch deadline/cancel
	// fired — they were claimed this cycle but never attempted (finding 9).
	for _, g := range groups[unlaunchedFrom:] {
		atomic.AddInt64(&deferredReleases, int64(len(g)))
		d.releaseRemainder(context.WithoutCancel(ctx), g, token)
	}

	deferredTotal := int(atomic.LoadInt64(&deferredReleases))
	if deferredTotal > 0 {
		// Deferred records are neither delivered nor lost: they were released
		// back to pending and will be re-claimed. Surface them so a degraded
		// batch (deadline strands) is observable and distinguishable from
		// successful drains.
		d.metrics.Counter(shared.MetricOutboxDeferred, int64(deferredTotal), routeTag)
	}

	duration := d.clk.Since(start)
	d.metrics.Timer(shared.MetricOutboxDrainLatency, duration, sessionTag)

	successTotal := int(atomic.LoadInt64(&successCount))
	d.fireBatchComplete(successTotal)

	if staleDetected.Load() {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "stale fencing token detected",
				"partition_key", d.partitionKey,
				"token_version", token.Version,
				"owner", token.Owner,
			)
		}
		return int(atomic.LoadInt64(&successCount)), int(atomic.LoadInt64(&transientReleases)) + deferredTotal, shared.ErrStaleFencingToken
	}

	if logging.DebugEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelDebug, "drain batch complete",
			"partition_key", d.partitionKey,
			"success_count", atomic.LoadInt64(&successCount),
			"duration", duration,
		)
	}

	return int(atomic.LoadInt64(&successCount)), int(atomic.LoadInt64(&transientReleases)) + deferredTotal, nil
}

// waitBatch blocks until every in-flight drain goroutine returns, under a
// watchdog (Min2). If wg.Wait outlives batchTimeout+drainWedgeGrace it emits
// MetricOutboxDrainStalled and a WARN — the signal that a Sender is ignoring
// ctx — and then RETURNS instead of waiting any longer, so the drainer keeps
// making progress and can shut down. The watchdog is the hard ceiling on how
// long the drainer waits for one batch; it never kills the in-flight goroutine
// (a hung Send cannot be force-returned in Go), so that goroutine may leak, but
// the drainer itself is never wedged. The watchdog timer is Stopped on return
// regardless of path.
func (d *Drainer) waitBatch(ctx context.Context, wg *sync.WaitGroup, batchTimeout time.Duration, start time.Time, tags ...shared.Tag) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	watchdog := d.clk.NewTimer(batchTimeout + drainWedgeGrace)
	defer watchdog.Stop()
	select {
	case <-done:
		return
	case <-watchdog.C():
		d.metrics.Counter(shared.MetricOutboxDrainStalled, 1, tags...)
		d.log(ctx, slog.LevelWarn,
			"drain batch exceeded watchdog; a sender is ignoring context cancellation — abandoning the in-flight send so the drainer does not wedge",
			"batch_timeout", batchTimeout, "waited", d.clk.Since(start))
	}
	// ponytail: watchdog expiry is the HARD CEILING on a single batch. A
	// well-behaved sender honors the already-cancelled workCtx/batchCtx and
	// returns long before this via the <-done case above, so reaching here
	// means a Sender is ignoring context cancellation and its goroutine will
	// never return. We deliberately do NOT block on <-done any longer: doing so
	// wedges this drainer forever — no other record drains and shutdown hangs
	// (the finding). Instead we RETURN and let drainBatch proceed, treating the
	// stuck record as transient: it was never Completed (Complete runs only
	// after Send returns nil, which has not happened), so it stays Claimed and
	// is re-claimed on a later cycle — at-least-once preserved, nothing falsely
	// completed. The abandoned goroutine LEAKS until its hung Send eventually
	// returns (if ever) — an accepted, bounded-per-batch cost that trades one
	// leaked goroutine for a live drainer. Its late atomic increments land on
	// heap-escaped batch counters and are harmless. Senders MUST honor ctx.
}

// orderingGroup is an ordered run of claimed outbox records delivered as
// a unit. Records sharing a non-empty ordering key occupy one group
// (sequential, in persisted order); keyless records each occupy a
// singleton group (full concurrency).
type orderingGroup []*persistence.OutboxRecord

// groupByOrderingKey partitions records into ordering groups, preserving
// claimed (persisted) order both across groups (first-seen key claims
// the slot) and within each group. Records without a non-empty ordering
// key become singleton groups so they keep full per-record concurrency.
func groupByOrderingKey(records []*persistence.OutboxRecord) []orderingGroup {
	groups := make([]orderingGroup, 0, len(records))
	indexByKey := make(map[string]int)
	for _, rec := range records {
		key, ok := rec.OrderingKey()
		if !ok {
			groups = append(groups, orderingGroup{rec})
			continue
		}
		if gi, seen := indexByKey[key]; seen {
			groups[gi] = append(groups[gi], rec)
			continue
		}
		indexByKey[key] = len(groups)
		groups = append(groups, orderingGroup{rec})
	}
	return groups
}

// adaptBatchSize adjusts currentBatchSize based on throughput.
// Called only from the Run loop goroutine — not concurrent.
//
// Scale-up: when a full batch is drained, double (capped at maxBatchSize).
// Scale-down: on each zero-success cycle, halve currentBatchSize
// (floored at the initial batchSize). This progressively shrinks the
// batch from any previously scaled-up value down to the floor.
func (d *Drainer) adaptBatchSize(drained int) {
	if drained >= d.currentBatchSize {
		d.currentBatchSize = min(d.currentBatchSize*2, d.maxBatchSize)
	} else if drained == 0 {
		d.currentBatchSize = max(d.currentBatchSize/2, d.batchSize)
	}
}
