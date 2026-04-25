package runtime

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func (d *OutboxDrainer) processRecord(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	routeTag := domain.Tag{Key: domain.TagKeyRouteID, Value: d.routeID}
	attempt := rec.ReplayCount + 1

	if env.HasExpiry() && env.IsExpired() {
		d.metrics.Counter(domain.MetricOutboxExpiredBeforeSend, 1, routeTag)
		return d.handleExpired(ctx, rec, token)
	}

	if rec.ReplayCount > d.policy.MaxReplayAttempts {
		return d.handlePoison(ctx, rec, token)
	}

	if rec.Address != "" {
		env.Subject = rec.Address
	}
	if rec.DispatchHeaders != nil {
		env.Headers = domain.MergeHeaders(env.Headers, rec.DispatchHeaders, true)
	}

	// Re-check lease before sending to minimize duplicate delivery window.
	if _, hasLease := d.tokenFn(); !hasLease {
		return domain.ErrStaleFencingToken
	}

	sendCtx, sendCancel := context.WithTimeout(ctx, d.policy.SendTimeout)
	defer sendCancel()

	sendErr := d.sender.Send(sendCtx, env)

	d.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Envelope:    env,
		Attempt:     attempt,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         sendErr,
	})

	if sendErr == nil {
		if completeErr := d.outboxStore.Complete(ctx, []string{rec.ID}, token); completeErr != nil {
			d.metrics.Counter(domain.MetricOutboxDuplicateRisk, 1, routeTag)
			d.log(ctx, slog.LevelError, "complete failed after successful send, message may be re-delivered",
				"record_id", rec.ID, "error", completeErr)
			return completeErr
		}
		d.metrics.Counter(domain.MetricOutboxCompletions, 1, routeTag)
		d.metrics.Counter(domain.MetricMessagesSent, 1, routeTag)
		d.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID,
			Envelope:    env,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Terminal:    true,
		})
		return nil
	}

	be, ok := domain.AsBridgeError(sendErr)
	if ok && be.Class != domain.ErrorTransient {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", sendErr, rec.ReplayCount); dlqErr != nil {
			d.log(ctx, slog.LevelError, "DLQ write failed, will not complete record",
				"record_id", rec.ID, "dlq_error", dlqErr)
			return dlqErr
		}
		d.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID,
			Envelope:    env,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Err:         sendErr,
			Terminal:    true,
		})
		return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
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

func (d *OutboxDrainer) handleExpired(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	if d.policy.OnExpired == domain.ExpiredDLQ {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", domain.ErrMessageExpired, rec.ReplayCount); dlqErr != nil {
			return dlqErr
		}
	}
	d.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Envelope:    env,
		Attempt:     rec.ReplayCount + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         domain.ErrMessageExpired,
		Terminal:    true,
	})
	return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
}

func (d *OutboxDrainer) handlePoison(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	poisonErr := domain.NewBridgeError(domain.ErrCodePoisonMessage, domain.ErrorPermanent, "replay count exceeded")
	if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", poisonErr, rec.ReplayCount); dlqErr != nil {
		return dlqErr
	}
	d.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Envelope:    env,
		Attempt:     rec.ReplayCount + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         poisonErr,
		Terminal:    true,
	})
	return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
}
