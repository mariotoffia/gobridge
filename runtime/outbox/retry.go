package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// errReleasedForRetry signals processRecord released a transiently-failed
// record back to pending (or left it claimed when the store has no
// OutboxReleaser). It is consumed inside drainBatch's group loop — never
// propagated as a drain error — to (1) stop the ordering group so a later
// same-key record cannot overtake the released one, (2) keep the record out
// of the success count, and (3) drive the transient-retry backoff floor.
var errReleasedForRetry = errors.New("outbox: record released for transient retry")

// errReleaseFailed signals that a record failed a transient send AND its
// subsequent Release back to pending failed with a store error (NOT a stale
// token). The record stays durably Claimed: Release did not transition it and
// Complete was never called. It is consumed inside drainBatch's group loop —
// never propagated as a drain error — to STOP the ordering group WITHOUT
// counting the still-claimed head as a success and WITHOUT letting a later
// same-key record overtake it (M4). The group loop deliberately does NOT
// releaseRemainder for this case: a store that just failed Release will fail it
// again, so the still-claimed head and the unattempted tail are recovered
// together by version/stale reclaim. It increments transientReleases so it
// drives the transient-retry backoff floor — this is a transient store hiccup.
var errReleaseFailed = errors.New("outbox: record release failed after transient send")

// errBatchDeadlineDeferred signals processRecord aborted a send because the
// batch deadline (workCtx/batchCtx) fired mid-flight. The record was NOT
// delivered and has been released back to pending (finding 9): it must be
// counted as DEFERRED, never as a success, and drives the transient backoff
// floor so the next cycle does not immediately re-hammer an overloaded batch.
//
// Note the deferral re-claims the record on the next cycle, which increments
// its ReplayCount even though no send failed. Replay count therefore counts
// claims, not failures, which is why the poison gate (processRecord) AND-checks
// poisonAgeReached — see poisonMinAge.
var errBatchDeadlineDeferred = errors.New("outbox: record deferred: batch deadline aborted send")

func (d *Drainer) completeCtx(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := d.policy.SendTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

// completeBudget mirrors completeCtx's bounded Complete/Release window so the
// batch timeout can reserve margin for it on top of each send (finding 10).
func (d *Drainer) completeBudget() time.Duration {
	timeout := d.policy.SendTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	return timeout
}

// emitDLQ counts a durable DLQ write from the drain path (finding 15). Without
// it, drainer-side poison/expiry/permanent DLQ writes were invisible to the
// conservation law even though the ingress route path emits the same counter.
func (d *Drainer) emitDLQ(category string) {
	d.metrics.Counter(shared.MetricDLQEntries, 1,
		shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID},
		shared.Tag{Key: shared.TagKeyCategory, Value: category},
	)
}

// emitDrop counts a terminal DROP from the drain path: a record settled WITHOUT
// a DLQ write and without a successful send, because the route's policy is
// OnPermanentFailure=drop or no DLQ store is configured (H3). It mirrors the
// route runner's emitDrop (route/dispatch.go) so drainer-side drops feed the
// same MessagesDropped series as ingress-side drops — dimensioned by
// TagKeyReason so the one series stays queryable across both paths and closes
// the conservation law received = sent + dropped + filtered + expired + dlq +
// inflight. Its counterpart emitDLQ counts DLQ writes; the two are mutually
// exclusive per record.
func (d *Drainer) emitDrop(category string) {
	d.metrics.Counter(shared.MetricMessagesDropped, 1,
		shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID},
		shared.Tag{Key: shared.TagKeyReason, Value: category},
	)
}

// completeTerminal Completes a terminally-settled record and fires the delivery
// hook's OnSettled, but only AFTER the store transition durably succeeds (M3).
//
// Ordering is load-bearing. The terminal metric (emitDLQ / emitDrop /
// MessagesExpired) is emitted by the CALLER BEFORE this, and is a per-write /
// per-event count: a DLQ write that already succeeded is real evidence and must
// be counted at-least-once even if the subsequent Complete fails. The hook, by
// contrast, is per-completed-record and must fire EXACTLY ONCE: if Complete
// fails the record stays claimed and is re-claimed and re-settled on a later
// cycle, so firing OnSettled now would double-count hook-driven billing/audit.
// On a Complete failure the hook is therefore deferred to the successful retry
// and completeErr is returned so the drain loop treats the record as unfinished.
// This keeps the metric (at-least-once) and the hook (exactly-once) mutually
// consistent instead of both firing on a settlement whose Complete never landed.
func (d *Drainer) completeTerminal(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken, outcome ports.DeliveryOutcome) error {
	completeCtx, completeCancel := d.completeCtx(ctx)
	completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID()}, token)
	completeCancel()
	if completeErr != nil {
		return completeErr
	}
	d.hook.OnSettled(ctx, outcome)
	return nil
}

// poisonAgeReached reports whether a replay-exhausted record is old enough to
// be poisoned to the DLQ (finding 8). The age gate prevents a transient egress
// outage — which can burn the small replay budget in seconds — from poisoning
// otherwise-healthy records: a record is only poisoned once it has ALSO reached
// poisonMinAge. When CreatedAt is unknown (zero), the age cannot be checked, so
// we fail OPEN and allow poisoning — preserving the pre-age-gate behaviour
// (poison on replay exhaustion) rather than retrying a genuinely poisoned record
// forever. Production records always carry a persist timestamp, so the age gate
// is effective there; only ageless (e.g. hand-rehydrated) records fail open.
func (d *Drainer) poisonAgeReached(rec *persistence.OutboxRecord) bool {
	created := rec.CreatedAt()
	if created.IsZero() {
		return true
	}
	return d.clk.Since(created) >= d.poisonMinAge
}

// replayBudgetExhausted reports whether a replay-exhausted record has ALSO spent
// its wall-clock replay budget and may therefore be poisoned to the DLQ
// (WP-REPLAY-BUDGET). The budget is measured from the record's FIRST delivery
// attempt (FirstAttemptedAt), so a transient egress outage that merely burns the
// replay COUNT quickly cannot poison an otherwise-healthy record until real time
// — replayBudget — has actually elapsed since delivery was first attempted.
//
// Records with a zero FirstAttemptedAt (persisted before the replay-budget
// schema, or never yet claimed) fall back BIT-FOR-BIT to the legacy CreatedAt
// age gate (poisonAgeReached / poisonMinAge), so upgrading in place changes no
// poison decision for pre-existing records.
func (d *Drainer) replayBudgetExhausted(rec *persistence.OutboxRecord) bool {
	first := rec.FirstAttemptedAt()
	if first.IsZero() {
		return d.poisonAgeReached(rec)
	}
	return d.clk.Since(first) >= d.replayBudget
}

// releaseOne best-effort returns a single claimed record to pending using the
// optional OutboxReleaser capability (finding 9). Stores without the capability
// keep the legacy leave-claimed behavior and recover via version/stale reclaim.
func (d *Drainer) releaseOne(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken) {
	releaser, ok := d.outboxStore.(ports.OutboxReleaser)
	if !ok {
		return
	}
	releaseCtx, cancel := d.completeCtx(ctx)
	err := releaser.Release(releaseCtx, []string{rec.ID()}, token)
	cancel()
	if err != nil {
		d.log(ctx, slog.LevelWarn, "failed to release batch-deadline-aborted record",
			"record_id", rec.ID(), "error", err)
	}
}

func (d *Drainer) processRecord(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken) error {
	// outbound is an isolated deep clone of the record's envelope; the
	// aggregate's internal envelope is never aliased. The logical Subject
	// is preserved and the destination address travels via
	// OutboundMessage.Address.
	outbound := rec.Snapshot()
	routeTag := shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID}
	attempt := rec.ReplayCount() + 1

	if outbound.HasExpiry() && outbound.IsExpired(d.clk) {
		d.metrics.Counter(shared.MetricOutboxExpiredBeforeSend, 1, routeTag)
		return d.handleExpired(ctx, rec, token)
	}

	// ReplayCount counts CLAIMS, not failed sends (deferrals and reclaims bump
	// it too), so replay exhaustion alone is not proof of poison. Poisoning is
	// gated on replay exhaustion AND the replay budget being spent
	// (replayBudgetExhausted): both must hold. A record is poisoned only once
	// wall-clock time since its FIRST attempt (FirstAttemptedAt) has reached
	// ReplayBudget; legacy records carrying a zero FirstAttemptedAt fall back
	// bit-for-bit to the CreatedAt/poisonMinAge age gate. Both conditions are a
	// hard AND, never an OR.
	if rec.ReplayCount() > d.policy.MaxReplayAttempts && d.replayBudgetExhausted(rec) {
		return d.handlePoison(ctx, rec, token)
	}

	if dh := rec.DispatchHeaders(); dh != nil {
		outbound.StampHeaders(messaging.MergeHeaders(outbound.Headers(), dh, true))
	}

	// Re-check lease before sending to minimize duplicate delivery window.
	if _, hasLease := d.tokenFn(); !hasLease {
		return shared.ErrStaleFencingToken
	}

	sendCtx, sendCancel := context.WithTimeout(ctx, d.policy.SendTimeout)
	defer sendCancel()

	sendErr := d.sender.Send(sendCtx, ports.OutboundMessage{Envelope: outbound, Address: rec.Address()})

	d.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID(),
		Address:     rec.Address(),
		Envelope:    outbound,
		Attempt:     attempt,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         sendErr,
	})

	if sendErr == nil {
		completeCtx, completeCancel := d.completeCtx(ctx)
		completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID()}, token)
		completeCancel()
		if completeErr != nil {
			d.metrics.Counter(shared.MetricOutboxDuplicateRisk, 1, routeTag)
			d.log(ctx, slog.LevelError, "complete failed after successful send, message may be re-delivered",
				"record_id", rec.ID(), "error", completeErr)
			return completeErr
		}
		d.metrics.Counter(shared.MetricOutboxCompletions, 1, routeTag)
		d.metrics.Counter(shared.MetricMessagesSent, 1, routeTag)
		d.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID(),
			Address:     rec.Address(),
			Envelope:    outbound,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Terminal:    true,
		})
		return nil
	}

	be, ok := shared.AsBridgeError(sendErr)
	if ok && be.Class != shared.ErrorTransient {
		outcome := ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID(),
			Address:     rec.Address(),
			Envelope:    outbound,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Err:         sendErr,
			Terminal:    true,
		}
		// H3: honor OnPermanentFailure=drop and a missing DLQ store (both legal
		// configs) by dropping-with-metric instead of writing a DLQ entry the
		// operator opted out of. Mirrors route.poisonReplayCapExceeded. Without
		// this gate a drop-policy route miscounts drops as DLQ entries, and with
		// no store it silently Completes the record while emitting a DLQEntries
		// counter that has nothing behind it.
		if d.policy.OnPermanentFailure == routing.FailureDrop || !d.dlq.HasStore() {
			d.emitDrop("permanent")
			d.log(ctx, slog.LevelWarn, "permanent send failure dropped: OnPermanentFailure=drop or no DLQ store",
				"record_id", rec.ID(), "error", sendErr)
			return d.completeTerminal(ctx, rec, token, outcome)
		}
		if dlqErr := d.dlq.Route(ctx, outbound, d.routeID, rec.BindingID(), rec.Address(), rec.SessionID(), "", sendErr, rec.ReplayCount()); dlqErr != nil {
			d.log(ctx, slog.LevelError, "DLQ write failed, will not complete record",
				"record_id", rec.ID(), "dlq_error", dlqErr)
			return dlqErr
		}
		d.emitDLQ("permanent")
		return d.completeTerminal(ctx, rec, token, outcome)
	}

	if ctx.Err() != nil {
		// The batch deadline (workCtx/batchCtx) — or a stale-token cancel —
		// fired mid-send. The record was NOT delivered: releasing it (finding
		// 9) returns it to pending so this owner re-claims and retries it next
		// cycle, instead of the previous behaviour that returned nil and let
		// drainBatch count the stranded, never-sent record as a success.
		d.log(ctx, slog.LevelDebug, "send aborted by batch deadline; deferring record",
			"record_id", rec.ID(), "error", sendErr)
		d.releaseOne(ctx, rec, token)
		return errBatchDeadlineDeferred
	}

	// Genuine transient failure (not shutdown cancellation). If the store
	// implements the optional OutboxReleaser capability, return the record
	// to pending so this same live owner can re-claim and retry it on the
	// very next drain — no fencing-version bump or wall-clock stale-claim
	// wait required (A4). Stores that do NOT implement it keep the legacy
	// leave-claimed behavior and rely on version/stale reclaim.
	releaser, ok := d.outboxStore.(ports.OutboxReleaser)
	if !ok {
		d.log(ctx, slog.LevelWarn, "transient send failure, will retry on next drain",
			"record_id", rec.ID(), "error", sendErr)
		return errReleasedForRetry
	}

	// Reuse completeCtx so the release survives parent cancellation with a
	// bounded deadline, exactly like the Complete calls above.
	releaseCtx, releaseCancel := d.completeCtx(ctx)
	releaseErr := releaser.Release(releaseCtx, []string{rec.ID()}, token)
	releaseCancel()
	switch {
	case releaseErr == nil:
		d.log(ctx, slog.LevelDebug, "released transiently-failed record for retry",
			"record_id", rec.ID(), "error", sendErr)
		return errReleasedForRetry
	case errors.Is(releaseErr, shared.ErrStaleFencingToken):
		// The owner lost the lease mid-flight. Surface the stale token so
		// the drain loop cancels sibling sends instead of retrying blind.
		d.log(ctx, slog.LevelWarn, "release after transient failure found stale fencing token",
			"record_id", rec.ID(), "error", releaseErr)
		return shared.ErrStaleFencingToken
	default:
		// Release failed for some reason OTHER than a stale token (e.g. a
		// store write/I/O error). The record was NOT sent and Release did NOT
		// transition it, so it stays durably Claimed. Return errReleaseFailed
		// (M4) so the group loop STOPS this ordering group without counting the
		// head as a success and without letting a later same-key record
		// overtake it. The loop does NOT releaseRemainder for this signal: a
		// store that just failed Release will fail it again, so the still-claimed
		// head and the unattempted tail are recovered together by version/stale
		// reclaim once the lease version advances (or the stale-claim window
		// elapses). Not escalated to a stale-token cancel — sibling sends for
		// other keys are unaffected by a localized store hiccup on this record.
		d.log(ctx, slog.LevelWarn, "release after transient failure failed",
			"record_id", rec.ID(), "error", releaseErr)
		return errReleaseFailed
	}
}

// releaseRemainder returns the still-claimed tail of an ordering group to
// pending after an earlier record in the group was released for transient
// retry (errReleasedForRetry). Stopping the group prevents a later same-key
// record from overtaking the released one, but the remainder was already
// claimed this cycle; without releasing it the unsent tail would strand as
// claimed on version-only stores (memory, SQLite) — the very A4 defect —
// because the same owner at the same fencing version cannot re-claim a
// claimed record. Each id is released individually, honoring OutboxReleaser's
// single-record contract. Best-effort: stores without OutboxReleaser keep the
// legacy leave-claimed behavior, and a release error is logged but never
// escalated (the record stays claimed and is recovered by version/stale
// reclaim).
func (d *Drainer) releaseRemainder(ctx context.Context, recs []*persistence.OutboxRecord, token persistence.LeaseToken) {
	releaser, ok := d.outboxStore.(ports.OutboxReleaser)
	if !ok {
		return
	}
	for _, rec := range recs {
		releaseCtx, cancel := d.completeCtx(ctx)
		err := releaser.Release(releaseCtx, []string{rec.ID()}, token)
		cancel()
		if err != nil {
			d.log(ctx, slog.LevelWarn, "failed to release unsent ordering-group remainder for retry",
				"record_id", rec.ID(), "error", err)
		}
	}
}

func (d *Drainer) handleExpired(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken) error {
	env := rec.Snapshot()
	routeTag := shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID}
	if d.policy.OnExpired == routing.ExpiredDLQ && d.dlq.HasStore() {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID(), rec.Address(), rec.SessionID(), "", shared.ErrMessageExpired, rec.ReplayCount()); dlqErr != nil {
			return dlqErr
		}
		d.emitDLQ("expired")
	} else {
		// Expired-drop, or on_expired=dlq with no DLQ store wired. The latter is
		// rejected by validateTerminalFailureSink at Start; the HasStore guard
		// mirrors the permanent/poison branches here and dispatch.go for defense
		// in depth, so we never emit a phantom DLQ metric for a Route whose
		// dlq.Route would silently no-op. Count the loss (finding 15) so the
		// conservation law can attribute it.
		d.metrics.Counter(shared.MetricMessagesExpired, 1, routeTag)
	}
	// M3: completeTerminal fires OnSettled only after Complete durably lands;
	// the MessagesExpired / DLQ count above is per-write and already stands.
	return d.completeTerminal(ctx, rec, token, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID(),
		Address:     rec.Address(),
		Envelope:    env,
		Attempt:     rec.ReplayCount() + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         shared.ErrMessageExpired,
		Terminal:    true,
	})
}

func (d *Drainer) handlePoison(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken) error {
	env := rec.Snapshot()
	// A record reaches this poison DLQ only by crossing MaxReplayAttempts AND
	// spending its wall-clock ReplayBudget since FirstAttemptedAt (a permanent
	// send error DLQs immediately via the non-transient branch in
	// processRecord). The replay budget is the A4-R1 root-cause fix: a transient
	// egress outage that merely burns the replay COUNT quickly no longer poisons
	// a healthy record, because poisoning now requires real time — measured from
	// the FIRST attempt — to have elapsed. The transientRetryFloor still bounds
	// how fast the count burns (rate); the budget bounds the TOTAL burn. Legacy
	// records with a zero FirstAttemptedAt fall back to the CreatedAt/poisonMinAge
	// age gate. Emit an explicit WARN carrying the age evidence
	// (first_attempted_at, replay_budget) so a genuine budget-exhaustion loss is
	// observable at the point of loss instead of surfacing only as a generic,
	// reason-less DLQ entry.
	d.log(ctx, slog.LevelWarn, "outbox record poisoned: replay attempts exhausted, routing to DLQ",
		"record_id", rec.ID(),
		"replay_count", rec.ReplayCount(),
		"max_replay_attempts", d.policy.MaxReplayAttempts,
		"first_attempted_at", rec.FirstAttemptedAt(),
		"replay_budget", d.replayBudget)
	poisonErr := shared.NewBridgeError(shared.ErrCodePoisonMessage, shared.ErrorPermanent, "replay count exceeded")
	outcome := ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID(),
		Address:     rec.Address(),
		Envelope:    env,
		Attempt:     rec.ReplayCount() + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         poisonErr,
		Terminal:    true,
	}
	// H3: honor OnPermanentFailure=drop / no DLQ store (mirror the permanent
	// branch). A poison under a drop policy — or with no store to write to — is
	// dropped-with-metric, not written to a DLQ the operator opted out of and
	// not counted as a DLQ entry with nothing behind it.
	if d.policy.OnPermanentFailure == routing.FailureDrop || !d.dlq.HasStore() {
		d.emitDrop("poison")
		// The point-of-loss detection WARN above records the poison with full
		// replay-budget evidence (and auto route/partition attrs); this line
		// records the DROP disposition that overrides the default DLQ intent.
		d.log(ctx, slog.LevelWarn, "poison dropped: OnPermanentFailure=drop or no DLQ store",
			"record_id", rec.ID(), "error", poisonErr)
		return d.completeTerminal(ctx, rec, token, outcome)
	}
	if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID(), rec.Address(), rec.SessionID(), "", poisonErr, rec.ReplayCount()); dlqErr != nil {
		return dlqErr
	}
	d.emitDLQ("poison")
	// M3: OnSettled fires only after Complete durably lands; emitDLQ above is
	// per-write and already stands even if Complete later fails.
	return d.completeTerminal(ctx, rec, token, outcome)
}
