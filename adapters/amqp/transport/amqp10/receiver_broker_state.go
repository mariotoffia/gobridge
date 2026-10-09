package amqp10

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// EndBrokerStateOnClose implements ports.BrokerStateEnder (ADR 0024). The next
// Close of a durable receiver on a topic (multicast) address that has a link
// attached at that moment, and whose Run has returned, ends its durable
// subscription: it closes the link with a closing detach, which the broker
// takes as an unsubscribe and so deletes the subscription and every message
// kept for it. Every other close keeps the subscription by dropping the
// connection instead (closeLink). A queue (anycast) receiver and a non-durable
// one hold no subscription to end. A non-zero before bounds the closing detach.
func (r *Receiver) EndBrokerStateOnClose(before time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endBrokerStateOnClose = true
	r.endBrokerStateBefore = before
}

// endDurableSubscription closes link with a closing detach, bounded by the
// session's connect timeout and, when non-zero, by before, the local lease
// deadline past which another instance may hold the subscription; once before
// passed nothing is sent. It reports whether the broker acknowledged the
// detach. A failure is logged at Warn and counted on
// shared.MetricBrokerStateEndFailures; the caller then drops the connection as
// on any other close, so the link goes down either way.
func (r *Receiver) endDurableSubscription(link linkReceiver, before time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), r.brokerStateEndTimeout())
	defer cancel()
	if !before.IsZero() {
		var cancelBefore context.CancelFunc
		ctx, cancelBefore = context.WithDeadline(ctx, before)
		defer cancelBefore()
	}
	err := ctx.Err()
	if err == nil {
		err = link.Close(ctx)
	}
	if err != nil {
		r.metrics.Counter(shared.MetricBrokerStateEndFailures, 1,
			shared.Tag{Key: shared.TagKeySessionID, Value: r.cfg.SessionID})
		if r.logger != nil {
			r.logger.Warn("amqp10: could not end the durable subscription the next configuration no longer has; "+
				"the broker may keep it", "session_id", r.cfg.SessionID, "address", redactURL(r.cfg.Address), "error", err)
		}
		return false
	}
	if r.logger != nil {
		r.logger.Info("amqp10: ended the durable subscription the next configuration no longer has",
			"session_id", r.cfg.SessionID, "address", redactURL(r.cfg.Address))
	}
	return true
}

// brokerStateEndTimeout bounds the closing detach that ends a durable
// subscription: the session's connect timeout.
func (r *Receiver) brokerStateEndTimeout() time.Duration {
	if r.session != nil && r.session.opts.ConnectTimeout > 0 {
		return r.session.opts.ConnectTimeout
	}
	return DefaultSessionOptions().ConnectTimeout
}

var _ ports.BrokerStateEnder = (*Receiver)(nil)
