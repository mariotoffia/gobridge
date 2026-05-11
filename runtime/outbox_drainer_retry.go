package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func (d *OutboxDrainer) completeCtx(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := d.policy.SendTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func (d *OutboxDrainer) processRecord(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken) error {
	env := &rec.Envelope
	routeTag := shared.Tag{Key: shared.TagKeyRouteID, Value: d.routeID}
	attempt := rec.ReplayCount() + 1

	if env.HasExpiry() && env.IsExpired(d.clk) {
		d.metrics.Counter(shared.MetricOutboxExpiredBeforeSend, 1, routeTag)
		return d.handleExpired(ctx, rec, token)
	}

	if rec.ReplayCount() > d.policy.MaxReplayAttempts {
		return d.handlePoison(ctx, rec, token)
	}

	// Build an isolated outbound envelope so we never mutate the persisted
	// record envelope. The logical Subject is preserved; the destination
	// address travels via OutboundMessage.Address.
	outbound := env.Clone()
	if rec.DispatchHeaders != nil {
		outbound.StampHeaders(messaging.MergeHeaders(outbound.Headers(), rec.DispatchHeaders, true))
	}

	// Re-check lease before sending to minimize duplicate delivery window.
	if _, hasLease := d.tokenFn(); !hasLease {
		return shared.ErrStaleFencingToken
	}

	sendCtx, sendCancel := context.WithTimeout(ctx, d.policy.SendTimeout)
	defer sendCancel()

	sendErr := d.sender.Send(sendCtx, ports.OutboundMessage{Envelope: outbound, Address: rec.Address})

	d.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Address:     rec.Address,
		Envelope:    outbound,
		Attempt:     attempt,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         sendErr,
	})

	if sendErr == nil {
		completeCtx, completeCancel := d.completeCtx(ctx)
		completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID}, token)
		completeCancel()
		if completeErr != nil {
			d.metrics.Counter(shared.MetricOutboxDuplicateRisk, 1, routeTag)
			d.log(ctx, slog.LevelError, "complete failed after successful send, message may be re-delivered",
				"record_id", rec.ID, "error", completeErr)
			return completeErr
		}
		d.metrics.Counter(shared.MetricOutboxCompletions, 1, routeTag)
		d.metrics.Counter(shared.MetricMessagesSent, 1, routeTag)
		d.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID,
			Address:     rec.Address,
			Envelope:    outbound,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Terminal:    true,
		})
		return nil
	}

	be, ok := shared.AsBridgeError(sendErr)
	if ok && be.Class != shared.ErrorTransient {
		if dlqErr := d.dlq.Route(ctx, outbound, d.routeID, rec.BindingID, rec.Address, rec.SessionID, "", sendErr, rec.ReplayCount()); dlqErr != nil {
			d.log(ctx, slog.LevelError, "DLQ write failed, will not complete record",
				"record_id", rec.ID, "dlq_error", dlqErr)
			return dlqErr
		}
		d.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID,
			Address:     rec.Address,
			Envelope:    outbound,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Err:         sendErr,
			Terminal:    true,
		})
		completeCtx, completeCancel := d.completeCtx(ctx)
		completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID}, token)
		completeCancel()
		return completeErr
	}

	if ctx.Err() != nil {
		d.log(ctx, slog.LevelDebug, "send aborted due to context cancellation",
			"record_id", rec.ID, "error", sendErr)
	} else {
		d.log(ctx, slog.LevelWarn, "transient send failure, will retry on next drain",
			"record_id", rec.ID, "error", sendErr)
	}
	return nil
}

func (d *OutboxDrainer) handleExpired(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken) error {
	env := &rec.Envelope
	if d.policy.OnExpired == routing.ExpiredDLQ {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.Address, rec.SessionID, "", shared.ErrMessageExpired, rec.ReplayCount()); dlqErr != nil {
			return dlqErr
		}
	}
	d.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Address:     rec.Address,
		Envelope:    env,
		Attempt:     rec.ReplayCount() + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         shared.ErrMessageExpired,
		Terminal:    true,
	})
	completeCtx, completeCancel := d.completeCtx(ctx)
	completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID}, token)
	completeCancel()
	return completeErr
}

func (d *OutboxDrainer) handlePoison(ctx context.Context, rec *persistence.OutboxRecord, token persistence.LeaseToken) error {
	env := &rec.Envelope
	poisonErr := shared.NewBridgeError(shared.ErrCodePoisonMessage, shared.ErrorPermanent, "replay count exceeded")
	if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.Address, rec.SessionID, "", poisonErr, rec.ReplayCount()); dlqErr != nil {
		return dlqErr
	}
	d.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Address:     rec.Address,
		Envelope:    env,
		Attempt:     rec.ReplayCount() + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         poisonErr,
		Terminal:    true,
	})
	completeCtx, completeCancel := d.completeCtx(ctx)
	completeErr := d.outboxStore.Complete(completeCtx, []string{rec.ID}, token)
	completeCancel()
	return completeErr
}
