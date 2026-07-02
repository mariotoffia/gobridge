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

func (d *Drainer) completeCtx(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := d.policy.SendTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
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

	if rec.ReplayCount() > d.policy.MaxReplayAttempts {
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
		if dlqErr := d.dlq.Route(ctx, outbound, d.routeID, rec.BindingID(), rec.Address(), rec.SessionID(), "", sendErr, rec.ReplayCount()); dlqErr != nil {
			d.log(ctx, slog.LevelError, "DLQ write failed, will not complete record",
				"record_id", rec.ID(), "dlq_error", dlqErr)
			return dlqErr
		}
		d.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID(),
			Address:     rec.Address(),
			Envelope:    outbound,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Err:         sendErr,
			Terminal:    true,
		})
		completeCtx, completeCancel := d.completeCtx(ctx)
		completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID()}, token)
		completeCancel()
		return completeErr
	}

	if ctx.Err() != nil {
		d.log(ctx, slog.LevelDebug, "send aborted due to context cancellation",
			"record_id", rec.ID(), "error", sendErr)
		return nil
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
		// store write/I/O error). Fall back to today's leave-claimed
		// behavior rather than escalating: return nil so the drain loop
		// does not cancel sibling sends for what is a localized, transient
		// store hiccup.
		//
		// Residual (A4-R2, reviewer-blessed): returning nil makes the
		// group loop count this record as a success and CONTINUE to the
		// next same-key record, so a later record in the ordering group
		// can overtake this still-claimed, never-sent head. There is NO
		// data loss — the head stays durably Claimed (Release did not
		// transition it and we did not Complete it) and is re-claimed and
		// re-sent in persisted order via version/stale reclaim once the
		// lease version advances (or the store's stale-claim window
		// elapses). Closing the ordering window fully would need a
		// distinct "release-failed" signal plus a conditional
		// releaseRemainder — over-engineering for a store error that must
		// land precisely on Release — so it is deliberately deferred.
		d.log(ctx, slog.LevelWarn, "release after transient failure failed",
			"record_id", rec.ID(), "error", releaseErr)
		return nil
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
	if d.policy.OnExpired == routing.ExpiredDLQ {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID(), rec.Address(), rec.SessionID(), "", shared.ErrMessageExpired, rec.ReplayCount()); dlqErr != nil {
			return dlqErr
		}
	}
	d.hook.OnSettled(ctx, ports.DeliveryOutcome{
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
	completeCtx, completeCancel := d.completeCtx(ctx)
	completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID()}, token)
	completeCancel()
	return completeErr
}

func (d *Drainer) handlePoison(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken) error {
	env := rec.Snapshot()
	// Replay-count exhaustion is the ONLY route to this poison DLQ: a
	// permanent send error DLQs immediately (see the non-transient branch
	// in processRecord), so a record only crosses MaxReplayAttempts by
	// being re-claimed and re-attempted repeatedly — i.e. by successive
	// TRANSIENT egress failures. An outage that outlasts the replay budget
	// therefore DLQs an otherwise-good message (A4-R1). The
	// transientRetryFloor bounds how fast that budget burns but does not
	// decouple it from wall-clock age; the age-based root-cause fix is a
	// deferred cross-store change. Emit an explicit WARN so this
	// premature-DLQ-from-outage is observable at the point of loss instead
	// of surfacing only as a generic, reason-less DLQ entry.
	d.log(ctx, slog.LevelWarn, "outbox record poisoned: replay attempts exhausted, routing to DLQ",
		"record_id", rec.ID(),
		"replay_count", rec.ReplayCount(),
		"max_replay_attempts", d.policy.MaxReplayAttempts)
	poisonErr := shared.NewBridgeError(shared.ErrCodePoisonMessage, shared.ErrorPermanent, "replay count exceeded")
	if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID(), rec.Address(), rec.SessionID(), "", poisonErr, rec.ReplayCount()); dlqErr != nil {
		return dlqErr
	}
	d.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID(),
		Address:     rec.Address(),
		Envelope:    env,
		Attempt:     rec.ReplayCount() + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         poisonErr,
		Terminal:    true,
	})
	completeCtx, completeCancel := d.completeCtx(ctx)
	completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID()}, token)
	completeCancel()
	return completeErr
}
