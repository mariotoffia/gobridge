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
	"github.com/mariotoffia/gobridge/ports"
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

// ErrDrainStalled is returned by Run once the batch watchdog has abandoned a
// send goroutine because a Sender ignored context cancellation (CORE-RES-1).
// Scheduling further batches would leak one parked sender (plus its waiter) per
// batch, unbounded. Returning it from Run stops all further batches immediately
// and, via startBackground's terminal-on-error path, escalates to a runtime
// restart — the only way to reclaim the already-leaked goroutine, since Go
// cannot force-return a hung Send.
var ErrDrainStalled = errors.New("runtime: outbox-drainer: partition stalled; a sender ignored context cancellation")

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
			if expireErr := d.maybeExpire(ctx, token); errors.Is(expireErr, shared.ErrStaleFencingToken) {
				// A successor owns this partition. On a drainer whose egress
				// never becomes ready the readiness gate below returns before
				// Claim, so this is the only place the takeover is ever
				// observed — back off exactly as the claim path does instead of
				// re-sweeping a partition we no longer own every interval.
				d.log(ctx, slog.LevelWarn, "stale fencing token on expiry sweep, waiting for new lease")
				timer.Reset(max(d.strategy.NextInterval(0), transientRetryFloor))
				continue
			}
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
			if d.drainStalled.Load() {
				// CORE-RES-1: the batch watchdog abandoned a send goroutine because a
				// Sender ignored cancellation. Stop scheduling batches (each could leak
				// another) and escalate terminal so a restart reclaims the leaked
				// goroutine — Go cannot force-return the hung Send.
				d.log(ctx, slog.LevelError,
					"outbox partition stalled: a sender ignored context cancellation; stopping drain and escalating terminal to bound the goroutine leak",
					"partition_key", d.partitionKey, "route", d.routeID)
				return ErrDrainStalled
			}
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
	// Use the worst-case ceiling for the final drain: the batch size is only
	// known after Claim, so the scaled per-record term cannot be applied yet.
	finalCeiling := d.maxDrainTimeout
	if finalCeiling <= 0 {
		finalCeiling = defaultMaxDrainTimeout
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
// races an in-flight send; it is partition-scoped so a drainer holding one
// partition's lease can never expire another partition's records.
//
// the bulk sweep is DEFERRED entirely for on_expired:dlq routes. The sweep
// only transitions records to the terminal "expired" state with a counter — it
// destroys the envelope without routing it — so under the default ExpiredDLQ
// policy a broker outage would erase evidence the operator asked to preserve.
// For DLQ-policy routes, expiry MUST flow through the per-record claim path
// (handleExpired) which routes the expired envelope to the DLQ. The sweep
// therefore runs ONLY for drop-policy routes, where the record would be dropped
// anyway so eager accounting loses no evidence.
// The sweep carries the drainer's live fencing token: it is a terminal,
// destructive transition of records a successor could still deliver, so the
// store — not this local lease check alone — is the authority that refuses a
// preempted owner. The token also advances the partition's fencing
// high-water-mark, which matters here because this runs BEFORE the
// egress-readiness gate: on a partition whose egress never becomes ready, this
// is the only fencing call the drainer ever makes.
//
// It returns the store error so the caller can act on it. A
// shared.ErrStaleFencingToken here is the ONLY signal a preempted owner gets on
// such a partition, since the readiness gate stops that drainer before Claim.
func (d *Drainer) maybeExpire(ctx context.Context, token persistence.LeaseToken) error {
	if d.outboxStore == nil {
		return nil
	}
	if d.policy.OnExpired == routing.ExpiredDLQ {
		// DLQ-policy expiry is handled per-record by handleExpired (which
		// preserves the envelope by routing it to the DLQ), never by the
		// evidence-destroying bulk sweep.
		return nil
	}
	now := d.clk.Now()
	if !d.lastExpire.IsZero() && now.Sub(d.lastExpire) < d.expireInterval {
		return nil
	}
	d.lastExpire = now
	// Bound the sweep like Claim: it holds the single drain goroutine for this
	// partition, so a black-holed store would otherwise stop the send path too,
	// not merely the housekeeping. A sweep truncated by that deadline (or
	// aborted mid-way by a fence raise) reports what it DID expire alongside the
	// error, so the count is emitted before the error is handled.
	expireCtx, expireCancel := d.storeOpContext(ctx)
	n, err := d.outboxStore.Expire(expireCtx, now, d.partitionKey, token)
	expireCancel()
	if n > 0 {
		// Expired records were received but will never be sent — count them as
		// expired so the conservation law (received = sent + dropped + filtered
		// + expired + dlq + inflight) can close. A partially completed sweep has
		// already made those transitions durable, so dropping the count on the
		// error path would silently break the law.
		d.metrics.Counter(shared.MetricMessagesExpired, int64(n),
			shared.Tag{Key: shared.TagKeySessionID, Value: d.partitionKey},
			shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID})
		d.log(ctx, slog.LevelInfo, "expired pending outbox records", "count", n)
	}
	if err != nil {
		d.log(ctx, slog.LevelWarn, "outbox expire sweep failed", "error", err, "expired_before_failure", n)
		return err
	}
	return nil
}

// emitDepth reports the outbox backlog for this partition on the drainer's own
// poll cadence. It always emits the honest per-cycle liveness gauge, and emits
// the true backlog gauge according to the store's depth capability:
//
//   - MetricOutboxClaimBatchSize: the honest claimed count (claimedThisCycle) —
//     a liveness/throughput signal that saturates at the claim ceiling. Kept
//     SEPARATE from depth so a full batch cannot masquerade as a shallow
//     backlog. Emitted EVERY cycle (H-OBS).
//   - MetricOutboxDepth: the TRUE pending backlog. When the store implements
//     ports.OutboxDepthReporter (the InstrumentedOutboxStore wrapper forwards
//     it), CountPending is read on EVERY drain cycle — INCLUDING zero-claim
//     cycles — so a backlog that could not be claimed this poll (e.g. Claim
//     returned 0 under contention while records still pend, observable via
//     MetricOutboxClaimConflicts) still reports its real size instead of a
//     false-healthy zero. The COUNT runs on the drain cadence (not a tight
//     loop), so the extra query on caught-up cycles is acceptable.
//
// Error handling distinguishes the two failure modes (H-OBS):
//   - ports.ErrOutboxDepthUnsupported (the inner store has not adopted the
//     capability): benign — fall back to claimedThisCycle, a saturating LOWER
//     BOUND, so the continuously emitted gauge and its breaching-on-missing
//     alarm stay alive with no regression. No DB round-trip occurred (the
//     wrapper returns the sentinel from a cheap type assertion), so unsupported
//     stores add no per-cycle cost.
//   - any OTHER error (a REAL DB/read failure): NOT masked. Skip the depth
//     emission for this cycle so a persistently broken query trips the
//     missing-data alarm, and make the failure observable via
//     MetricOutboxDepthFailures + a structured error log.
//
// A store that does not implement the capability at all (raw store, no wrapper)
// gets the claimed-count fallback with no CountPending call. This is a
// metrics-only observation and never mutates drain/claim state.
func (d *Drainer) emitDepth(ctx context.Context, claimedThisCycle int) {
	partitionTag := shared.Tag{Key: shared.TagKeyPartition, Value: d.partitionKey}
	// Always emit the honest per-cycle claim/liveness gauge.
	d.metrics.Gauge(shared.MetricOutboxClaimBatchSize, float64(claimedThisCycle), partitionTag)

	// Stranded-work gauge: records CLAIMED but not yet terminal. CountPending
	// excludes them, so without this a record left Claimed by a failed release
	// or a dead owner is invisible — depth reads zero while messages wait.
	d.emitClaimedDepth(ctx, partitionTag)

	reporter, ok := d.outboxStore.(ports.OutboxDepthReporter)
	if !ok {
		// Store cannot report depth at all: saturating claimed-count fallback.
		d.metrics.Gauge(shared.MetricOutboxDepth, float64(claimedThisCycle), partitionTag)
		return
	}

	// Bounded like Claim and the expiry sweep: this runs on every drain cycle
	// purely to feed a gauge, so a store that never answers must not be able to
	// hold the drain goroutine hostage for a metric. A DeadlineExceeded falls
	// through to the default branch below and is reported as a real count
	// failure, which is what it is.
	countCtx, countCancel := d.storeOpContext(ctx)
	n, err := reporter.CountPending(countCtx, d.partitionKey)
	countCancel()
	switch {
	case err == nil:
		// True backlog, every cycle including zero-claim.
		d.metrics.Gauge(shared.MetricOutboxDepth, float64(n), partitionTag)
	case errors.Is(err, ports.ErrOutboxDepthUnsupported):
		// Inner store has not adopted the capability: benign saturating fallback
		// (no DB round-trip happened).
		d.metrics.Gauge(shared.MetricOutboxDepth, float64(claimedThisCycle), partitionTag)
	default:
		// REAL count failure. Do NOT mask it behind the claimed-count fallback:
		// skip the depth emission this cycle (missing-data alarm catches a
		// persistently broken query) and make the failure observable.
		d.metrics.Counter(shared.MetricOutboxDepthFailures, 1, partitionTag)
		d.log(ctx, slog.LevelError, "outbox depth query failed; skipping OutboxDepth this cycle",
			"partition_key", d.partitionKey, "error", err.Error())
	}
}

// emitClaimedDepth reports how many records this partition currently holds in
// the CLAIMED state, when the store implements the OPTIONAL
// ports.OutboxClaimedDepthReporter capability. It is a no-op on stores that do
// not, so no backend is forced to pay for a second count.
//
// There is deliberately NO fallback value: the claimed count cannot be
// approximated from anything the drainer knows (the batch it just claimed says
// nothing about work stranded by earlier cycles or other owners), and a wrong
// stranded-work number is worse than none. The call is bounded like the depth
// count so a black-holed store cannot park the drain goroutine for a metric.
func (d *Drainer) emitClaimedDepth(ctx context.Context, partitionTag shared.Tag) {
	reporter, ok := d.outboxStore.(ports.OutboxClaimedDepthReporter)
	if !ok {
		return
	}
	countCtx, countCancel := d.storeOpContext(ctx)
	n, err := reporter.CountClaimed(countCtx, d.partitionKey)
	countCancel()
	switch {
	case err == nil:
		d.metrics.Gauge(shared.MetricOutboxClaimedDepth, float64(n), partitionTag)
	case errors.Is(err, ports.ErrOutboxDepthUnsupported):
		// Capability advertised but the inner store cannot answer: emit nothing.
	default:
		d.metrics.Counter(shared.MetricOutboxDepthFailures, 1, partitionTag)
		d.log(ctx, slog.LevelError, "outbox claimed-depth query failed; skipping OutboxClaimedDepth this cycle",
			"partition_key", d.partitionKey, "error", err.Error())
	}
}

// storeOpContext bounds a single outbox-store housekeeping operation (the
// expiry sweep, the depth count) so a black-holed store call cannot pin this
// drainer partition indefinitely. The bound reuses the route's SendTimeout
// (falling back to the default), a dependency-call bound of the same class; a
// DeadlineExceeded is treated like any transient store error and the partition
// is re-polled on the next cycle. The caller MUST invoke the returned cancel.
//
// Claim uses claimOpContext instead: its cost scales with the batch, this one's
// does not.
func (d *Drainer) storeOpContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d.sendTimeout())
}

func (d *Drainer) sendTimeout() time.Duration {
	if d.policy.SendTimeout <= 0 {
		return routing.DefaultSendTimeout
	}
	return d.policy.SendTimeout
}

const (
	// perRecordClaimBudget is the allowance a claim gets for each record in the
	// batch. A backend that claims one record per remote transaction (the
	// DynamoDB outbox) pays `limit` round trips, so the claim bound has to scale
	// with the batch or a large batch times out mid-loop on every cycle. The
	// value is a generous ceiling for one conditional write, not a target.
	perRecordClaimBudget = 100 * time.Millisecond

	// maxClaimBudget caps the scaled bound. A claim that has not returned inside
	// two minutes is not slow, it is broken, and the drain goroutine must be
	// free to re-poll rather than wait for it.
	maxClaimBudget = 2 * time.Minute
)

// claimOpContext bounds a single Claim. Unlike housekeeping calls, a Claim's
// cost is proportional to `limit` on a backend that claims per record, so the
// bound is max(SendTimeout, limit x perRecordClaimBudget), capped at
// maxClaimBudget.
//
// Sizing this to SendTimeout alone was the defect: with a short send timeout and
// a large batch, every cycle expired part-way through the claim loop. Records
// already claimed are durably this owner's and hidden from the pending-depth
// gauge, so each cycle stranded more of them and charged a replay attempt on
// recovery — a healthy backlog could reach the replay cap and be poisoned to the
// dead-letter queue without a single send. The caller MUST invoke the returned
// cancel.
func (d *Drainer) claimOpContext(ctx context.Context, limit int) (context.Context, context.CancelFunc) {
	timeout := d.sendTimeout()
	if scaled := time.Duration(limit) * perRecordClaimBudget; scaled > timeout {
		timeout = scaled
	}
	// The multiplication above can overflow on a pathological limit; clamp on
	// both the cap and a non-positive result.
	if timeout > maxClaimBudget || timeout <= 0 {
		timeout = maxClaimBudget
	}
	return context.WithTimeout(ctx, timeout)
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

	// bound the Claim so a black-holed store call cannot pin this
	// drainer partition indefinitely. A DeadlineExceeded is returned like any
	// other transient Claim error and the partition is re-polled next cycle.
	claimCtx, claimCancel := d.claimOpContext(ctx, d.currentBatchSize)
	records, err := d.outboxStore.Claim(claimCtx, d.partitionKey, token, d.currentBatchSize)
	claimCancel()
	if err != nil {
		return 0, 0, err
	}
	// Report the partition backlog on the drainer's own poll cadence (H-OBS).
	// Runs on every drain cycle this partition actually runs (lease held AND
	// egress ready), independent of MaxOutboxDepth and of the ingress
	// QueryPending path, so the gauges are continuous while an outbox exists.
	// It does NOT emit while standby (no lease) or while egress is not ready;
	// those gaps are covered by DrainSkippedNoLease and the readiness gate.
	d.emitDepth(ctx, len(records))
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
						// a store error. The head stays durably Claimed. Stop
						// the group WITHOUT counting a success and WITHOUT letting
						// a later same-key record overtake the still-claimed head.
						// Do NOT releaseRemainder: the store that just failed
						// Release will fail it again, so version/stale reclaim
						// recovers the still-claimed head AND the unattempted tail
						// together. Drives the backoff floor via transientReleases.
						atomic.AddInt64(&transientReleases, 1)
					case errors.Is(err, errCompletionFenced):
						// Post-send fence tripped: the batch was abandoned
						// by the watchdog (or cancelled) AFTER this record's Send
						// returned, so this owner did NOT Complete it. The fence's
						// lease check (checked FIRST) already passed, so the lease
						// Version is UNCHANGED — which means on a VERSION-ONLY store
						// (the in-memory backend; ports/stores.go documents same-
						// version stale reclaim as MAY, not MUST) the same owner at
						// the same version can NEVER re-Claim a still-Claimed record
						// (OutboxRecord.Claim requires claim_version < token.Version
						// or Pending). Leaving the head Claimed here would strand it
						// — AND the unattempted group[ri+1:] suffix — Claimed forever
						// (CountPending excludes Claimed rows, so the backlog reads 0
						// while records silently stall). So RELEASE the fenced head
						// plus its unsent suffix back to pending with the CURRENT
						// token; the next cycle re-drains them. releaseRemainder is
						// token-fenced: if the token has since gone stale (another
						// owner reclaimed on a stale-reclaim store), Release fails and
						// the record correctly stays Claimed for that new owner — safe
						// on BOTH store classes. Use a detached ctx because batchCtx
						// is cancelled in this branch. The head's Send already ran
						// (its delivery side-effect happened), so re-draining it is
						// the accepted at-least-once duplicate already counted via
						// MetricOutboxDuplicateRisk at the fence; the suffix was never
						// attempted, so it is a first delivery. Count neither a
						// success nor a hard failure; drive the backoff floor via
						// transientReleases, consistent with errReleasedForRetry.
						atomic.AddInt64(&transientReleases, 1)
						d.releaseRemainder(context.WithoutCancel(ctx), group[ri:], token)
						d.log(batchCtx, slog.LevelWarn, "record left for reclaim: post-send fence tripped after batch abandonment; released head+suffix to pending",
							"record_id", rec.ID())
					case errors.Is(err, shared.ErrStaleFencingToken):
						staleDetected.Store(true)
						batchCancel()
						d.log(batchCtx, slog.LevelWarn, "record processing failed",
							"record_id", rec.ID(), "error", err)
					default:
						// An unclassified per-record failure: a DLQ-store write
						// error, or a post-send Complete the store refused. The
						// head keeps its claim — its Send may already have landed,
						// so a re-drain is the accepted at-least-once duplicate —
						// but group[ri+1:] was NEVER attempted. Leaving that tail
						// Claimed hides it from the pending-depth gauge, makes it
						// unreclaimable until the fencing version moves or the
						// stale window elapses (never, on a version-only store),
						// and charges a replay attempt on every recovery cycle,
						// so a group behind one flaky record can be poisoned to
						// the DLQ without ever having been sent. Release it, with
						// a detached ctx because batchCtx may already be dead.
						// releaseRemainder is token-fenced, so if this owner has
						// since lost the partition the tail correctly stays
						// claimed for its successor.
						d.metrics.Counter(shared.MetricOutboxRecordFailures, 1, routeTag)
						atomic.AddInt64(&deferredReleases, int64(len(group)-ri-1))
						d.releaseRemainder(context.WithoutCancel(ctx), group[ri+1:], token)
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
		// CORE-RES-1: latch the partition stalled so Run stops scheduling further
		// batches. Without this, every later batch could leak another parked
		// sender+waiter (a sender ignoring ctx never returns), unbounded.
		d.drainStalled.Store(true)
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
	// returns (if ever); its late atomic increments land on heap-escaped batch
	// counters and are harmless. CORE-RES-1: we latch drainStalled here so Run
	// stops scheduling further batches (each could leak another parked
	// sender+waiter) and escalates terminal — bounding the leak to the ONE
	// goroutine that tripped the watchdog rather than one-per-batch forever.
	// Senders MUST honor ctx.
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
