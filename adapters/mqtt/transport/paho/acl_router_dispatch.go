package paho

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/logging"
)

// dispatchLoop is the SINGLE serialized dispatch worker. It drains
// dispatchCh in arrival order and runs the (synchronous) dispatch for
// each publish, off the paho publish-callback goroutine, until shutdown.
// It closes done on exit so Close can join it (awaitDispatchLoop) before
// r.wg.Wait() — guaranteeing its final fanout r.wg.Add has completed and
// therefore never races the Wait.
func (r *router) dispatchLoop(ch <-chan dispatchItem, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-r.stop:
			r.drainDispatchOnStop(ch)
			return
		case item := <-ch:
			r.dispatchAtEpoch(item.pub, item.ack, item.epoch, true)
		}
	}
}

// drainDispatchOnStop empties any publishes still queued in dispatchCh when the
// worker stops, so a QoS 0 entry left buffered at close is metered rather than
// vanishing silently (MQTT-OBS-1). Each entry's queue reservation is released;
// a QoS 1/2 entry is deliberately left UNACKED so the broker redelivers it on
// session resume (at-least-once), while a QoS 0 entry — which has no redelivery
// — is counted on MetricMQTTRouterDropped so the close-time loss is observable.
func (r *router) drainDispatchOnStop(ch <-chan dispatchItem) {
	for {
		select {
		case item := <-ch:
			if item.pub == nil {
				continue
			}
			r.releaseQueueReservation(item.pub)
			if item.pub.QoS == 0 {
				r.dropCount.Add(1)
				r.metrics.Counter(MetricMQTTRouterDropped, 1, r.sessionTag()...)
			}
		default:
			return
		}
	}
}

// awaitDispatchLoop blocks until the serialized dispatch worker has fully
// exited (its final r.wg.Add/Done pair complete), or returns immediately
// when the worker was never started (dispatchDone nil — a session that
// never began grace). Callers MUST invoke this before r.wg.Wait() so no
// dispatchLoop Add can race the Wait.
func (r *router) awaitDispatchLoop() {
	r.mu.Lock()
	done := r.dispatchDone
	r.mu.Unlock()
	if done != nil {
		<-done
	}
}

// clonePublish makes an isolated deep copy for the legacy Router and
// multi-handler compatibility boundaries, whose public callers historically
// may mutate payload/properties. Production OnPublishReceived instead retains
// immutable Paho callback backing: Paho allocates a Publish wrapper per packet
// and its acknowledgement tracker retains the underlying wire packet until
// settlement, so callback return does not invalidate payload/property strings.
func clonePublish(pub *pahov5.Publish) *pahov5.Publish {
	if pub == nil {
		return nil
	}
	cloned := *pub
	if pub.Payload != nil {
		cloned.Payload = make([]byte, len(pub.Payload))
		copy(cloned.Payload, pub.Payload)
	}
	if pub.Properties == nil {
		return &cloned
	}
	properties := *pub.Properties
	properties.User = append(pahov5.UserProperties(nil), pub.Properties.User...)
	if pub.Properties.CorrelationData != nil {
		properties.CorrelationData = make([]byte, len(pub.Properties.CorrelationData))
		copy(properties.CorrelationData, pub.Properties.CorrelationData)
	}
	if pub.Properties.SubscriptionIdentifier != nil {
		v := *pub.Properties.SubscriptionIdentifier
		properties.SubscriptionIdentifier = &v
	}
	if pub.Properties.PayloadFormat != nil {
		v := *pub.Properties.PayloadFormat
		properties.PayloadFormat = &v
	}
	if pub.Properties.MessageExpiry != nil {
		v := *pub.Properties.MessageExpiry
		properties.MessageExpiry = &v
	}
	if pub.Properties.TopicAlias != nil {
		v := *pub.Properties.TopicAlias
		properties.TopicAlias = &v
	}
	cloned.Properties = &properties
	return &cloned
}

// onPublishReceived is the Paho client's publish callback (installed
// via paho.ClientConfig.OnPublishReceived in Session.Start). It binds
// the manual-acknowledgment callback to the originating publish and
// dispatches. Returning true marks the publish handled; errors are
// never returned because failure handling is the runtime's job via the
// Delivery contract.
func (r *router) onPublishReceived(pr pahov5.PublishReceived) (bool, error) {
	received := pr.Packet
	if class, violation := r.ingressCapViolation(received); violation != nil {
		client := pr.Client
		var ack func() error
		if received.QoS > 0 && client != nil {
			ack = func() error { return client.Ack(received) }
		}
		r.dropPoisonIngress(received, class, violation, ack)
		return true, nil
	}
	pub := publishWithIdentity(received)
	client := pr.Client
	var ack func() error
	if received != nil && received.QoS > 0 && client != nil {
		ack = r.trackAcknowledgement(r.ackWithReconnectMapping(func() error {
			return client.Ack(received)
		}))
	}
	r.enqueueDispatch(pub, ack)
	return true, nil
}

// ackWithReconnectMapping wraps a protocol-ack callback with the
// connection-cycled mapping: paho ErrPacketNotFound means the connection was
// torn down and re-established between receive and settle — the client's ack
// tracker was reset, the broker will redeliver, and downstream dedup absorbs
// the duplicate, so the settlement reports SUCCESS. Each mapped success is a
// GUARANTEED broker redelivery, so it is counted on
// MetricMQTTAckAfterReconnect (MQTT-L5): a burst after a reconnect storm is
// the leading indicator of a duplicate flood on routes without downstream
// dedup. Every other ack error is classified via MapError and remains a
// settlement failure.
func (r *router) ackWithReconnectMapping(ack func() error) func() error {
	return func() error {
		if err := ack(); err != nil {
			if errors.Is(err, pahov5.ErrPacketNotFound) {
				r.ackAfterReconnect.Add(1)
				r.metrics.Counter(MetricMQTTAckAfterReconnect, 1, r.sessionTag()...)
				return nil
			}
			return MapError(err)
		}
		return nil
	}
}

// enqueueDispatch hands a publish to the serialized dispatch worker
// (dispatchCh) so the paho publish-callback goroutine returns promptly
// and keeps processing PINGRESP/PUBACK. On a full queue a QoS 0 publish
// is dropped (no delivery contract) so a QoS 0 flood cannot stall the
// connection into keepalive death; a QoS 1/2 publish blocks (bounded by
// the broker's Receive-Maximum window) so at-least-once is preserved.
// When the worker has not started (dispatchCh == nil — direct-dispatch
// unit tests) it dispatches inline so behaviour is unchanged there.
func (r *router) enqueueDispatch(pub *pahov5.Publish, ack func() error) {
	if pub == nil {
		return
	}
	if !r.reserveQueueSlot(pub, pub.QoS) {
		if pub.QoS == 0 {
			r.dropQoS0Overflow(pub)
		}
		return
	}
	r.mu.Lock()
	ch := r.dispatchCh
	epoch := r.connEpoch
	discarding := r.discarding
	r.mu.Unlock()
	if discarding {
		// Recycle-window discard: old-socket ingress released without ack while
		// the connection is being torn down. QoS 1/2 is redelivered by the
		// resumed session; QoS 0 is a best-effort loss. Counted on the same
		// stale-purge metric as the epoch-mismatch branch so this drop is
		// never silent (MQTT-L4).
		r.releaseQueueReservation(pub)
		r.stalePurged.Add(1)
		r.metrics.Counter(MetricMQTTRouterStalePurged, 1, r.sessionTag()...)
		return
	}
	if ch == nil {
		r.dispatchAtEpoch(pub, ack, epoch, true)
		return
	}
	item := dispatchItem{pub: pub, ack: ack, epoch: epoch}
	select {
	case ch <- item:
		return
	default:
	}
	if pub.QoS == 0 {
		r.releaseQueueReservation(pub)
		r.dropCount.Add(1)
		r.metrics.Counter(MetricMQTTRouterDropped, 1, r.sessionTag()...)
		if r.logger != nil {
			r.logger.Warn("mqtt: dropped QoS 0 publish (dispatch queue full under flood)",
				"topic", pub.Topic)
		}
		return
	}
	// QoS 1/2: block until the worker drains a slot or the router stops.
	// The un-acked publish is redelivered by the broker if we stop first.
	select {
	case ch <- item:
	case <-r.stop:
		r.releaseQueueReservation(pub)
	}
}

// Route implements paho.Router for compatibility with tests and the
// legacy router seam. Publishes entering through Route carry no
// protocol-ack callback (the Paho client auto-acks when it drives a
// Router); production traffic enters through onPublishReceived.
func (r *router) Route(pb *packets.Publish) {
	// PublishFromPacketPublish retains the packet payload backing array. Clone
	// at this compatibility boundary so fanout can transfer its router-owned
	// publish to one handler without allowing that handler to mutate pb.
	pub := pahov5.PublishFromPacketPublish(pb)
	if class, violation := r.ingressCapViolation(pub); violation != nil {
		// Legacy Route path: the Paho client auto-acks when it drives a
		// Router, so no manual ack is available (or needed) here.
		r.dropPoisonIngress(pub, class, violation, nil)
		return
	}
	r.dispatch(clonePublish(publishWithIdentity(pub)), nil)
}

// dispatch fans a publish out to every registered handler whose topic
// filters match, then BLOCKS until each has returned (synchronous
// dispatch). Each handler receives an independent copy of the Publish
// — struct, Payload and Properties are copied so handlers can safely
// inspect the data without racing on shared backing arrays. When no
// handler matches, the publish is buffered (bounded) within the startup
// grace window, or acked-and-dropped (and its topic unsubscribed) as an
// orphan broker subscription once the window has elapsed.
func (r *router) dispatch(pub *pahov5.Publish, ack func() error) {
	if pub == nil {
		return
	}
	r.mu.RLock()
	epoch := r.connEpoch
	r.mu.RUnlock()
	r.dispatchAtEpoch(pub, ack, epoch, true)
}

func (r *router) dispatchAtEpoch(pub *pahov5.Publish, ack func() error, epoch uint64, count bool) {
	r.dispatchCore(pub, ack, epoch, count, false)
}

// dispatchCore applies managed cleanup/quiesce gates before inspecting handlers.
// bypassQuiesce is reserved for resumeManagedDispatch while it drains buffered
// replacement-generation publishes before reopening live ingress.
func (r *router) dispatchCore(pub *pahov5.Publish, ack func() error, epoch uint64, count, bypassQuiesce bool) {
	if pub == nil {
		return
	}
	if count {
		r.routeCount.Add(1)
	}
	r.mu.Lock()
	if epoch != r.connEpoch {
		r.mu.Unlock()
		r.releaseQueueReservation(pub)
		r.stalePurged.Add(1)
		r.metrics.Counter(MetricMQTTRouterStalePurged, 1, r.sessionTag()...)
		return
	}
	if r.discarding {
		r.mu.Unlock()
		// Same recycle-window discard as enqueueDispatch's, for an item already
		// drained from dispatchCh when the discard began (MQTT-L4).
		r.releaseQueueReservation(pub)
		r.stalePurged.Add(1)
		r.metrics.Counter(MetricMQTTRouterStalePurged, 1, r.sessionTag()...)
		return
	}
	if (!bypassQuiesce && r.quiesced) || (len(r.managedCleanupFilters) > 0 && matchesAnyFilter(r.managedCleanupFilters, pub.Topic)) {
		buffered := r.bufferLocked(pub, ack)
		r.mu.Unlock()
		r.finishHeldPublish(pub, ack, buffered)
		return
	}

	matching := make([]routerHandler, 0, len(r.handlers))
	for _, h := range r.handlers {
		if matchesAnyFilter(h.filters, pub.Topic) {
			h.inflight.Add(1)
			matching = append(matching, h)
		}
	}
	if len(matching) > 0 {
		r.addCallbacksLocked(len(matching))
	}
	if len(matching) == 0 {
		if r.clk.Now().Before(r.graceDeadline) {
			buffered := r.bufferLocked(pub, ack)
			r.mu.Unlock()
			if buffered {
				r.bufferedCount.Add(1)
				r.metrics.Counter(MetricMQTTRouterBuffered, 1)
				logging.Debug(r.logger, "mqtt: buffered message (no matching handler registered yet)",
					"topic", pub.Topic, "qos", pub.QoS)
			} else if pub.QoS > 0 {
				r.overflowAckDrop(pub, ack)
			} else {
				r.dropQoS0Overflow(pub)
			}
			return
		}
		r.mu.Unlock()
		r.settleUnmatched(pub, ack)
		return
	}
	r.mu.Unlock()
	r.releaseQueueReservation(pub)

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelTrace,
			"mqtt: routing message", "topic", pub.Topic,
			"payload_len", len(pub.Payload), "handler_count", len(matching))
	}
	r.fanout(pub, ack, matching)
}

func (r *router) finishHeldPublish(pub *pahov5.Publish, ack func() error, buffered bool) {
	if buffered {
		r.bufferedCount.Add(1)
		r.metrics.Counter(MetricMQTTRouterBuffered, 1)
		logging.Debug(r.logger, "mqtt: retained publish while managed subscription cleanup is pending",
			"topic", pub.Topic, "qos", pub.QoS)
		return
	}
	if pub.QoS > 0 {
		r.overflowAckDrop(pub, ack)
		return
	}
	r.dropQoS0Overflow(pub)
}

// fanout dispatches one publish to the given handlers and blocks until
// all of them have returned. With more than one matching handler the
// protocol ack is split: it fires only after EVERY handler's delivery
// has been settled, preserving at-least-once for each receiver.
func (r *router) fanout(pub *pahov5.Publish, ack func() error, handlers []routerHandler) {
	acks := splitAck(len(handlers), ack)

	// local tracks just this publish's handlers so fanout can block
	// until they complete (synchronous dispatch / backpressure). r.wg is
	// the shared WaitGroup Session.Close awaits, so an in-flight handler
	// still blocks Close when dispatch is driven from its own goroutine.
	var local sync.WaitGroup
	r.wg.Add(len(handlers))
	local.Add(len(handlers))

	// The callback/Route boundary already gave the router ownership of pub.
	// Pre-clone all but one dispatch before starting any handler, then transfer
	// pub itself to the remaining handler. Pre-cloning preserves isolation even
	// when the transferred handler mutates immediately, while avoiding one full
	// payload allocation per received publish.
	dispatches := make([]*pahov5.Publish, len(handlers))
	for i := 1; i < len(dispatches); i++ {
		dispatches[i] = clonePublish(pub)
	}
	if len(dispatches) > 0 {
		dispatches[0] = pub
	}
	for i, h := range handlers {
		p := dispatches[i]
		go func(handler routerHandler, ackPart func() error) {
			defer r.wg.Done()
			defer r.callbackDone()
			defer local.Done()
			// inflight was incremented under r.mu in dispatch (or in
			// RegisterFiltered for the flush path); release it here so
			// Unregister's Wait unblocks once this emit returns —
			// guaranteeing emit is never in flight after Unregister
			// returns (ports.Receiver emit lifetime).
			if handler.inflight != nil {
				defer handler.inflight.Done()
			}
			defer func() {
				if rv := recover(); rv != nil {
					r.metrics.Counter(MetricMQTTHandlerPanics, 1)
					if r.logger != nil {
						r.logger.Error("mqtt: handler panicked",
							"recovered", rv,
							"topic", p.Topic,
						)
					}
					// Deliberately NOT acked: the un-acked publish is
					// redelivered on session resume (at-least-once).
				}
			}()
			// emitMu serializes this handler's live dispatch with its
			// pending-flush so a handler never sees concurrent or
			// out-of-order emit (ports.Receiver sequential-emit contract).
			if handler.emitMu != nil {
				handler.emitMu.Lock()
				defer handler.emitMu.Unlock()
			}
			handler.fn(p, ackPart)
		}(h, acks[i])
	}
	// Block until all handlers for THIS publish have returned. This is
	// the backpressure point described in the type doc; the protocol ack
	// fires later, at delivery settlement.
	local.Wait()
}

// splitAck fans one protocol-ack callback out to n consumers: each
// returned callback is idempotent, and the underlying ack fires only
// after ALL n callbacks have been invoked at least once. The first
// error from the underlying ack is returned to the caller that
// triggered it. A nil ack yields n no-op callbacks.
func splitAck(n int, ack func() error) []func() error {
	out := make([]func() error, n)
	if ack == nil {
		for i := range out {
			out[i] = nil
		}
		return out
	}
	if n == 1 {
		// Even the single-consumer path must be idempotent: Delivery
		// has its own sync.Once, but the router contract is that every
		// callback it hands out is safe to invoke more than once.
		var once sync.Once
		out[0] = func() error {
			var err error
			once.Do(func() { err = ack() })
			return err
		}
		return out
	}
	var remaining atomic.Int64
	remaining.Store(int64(n))
	for i := range out {
		var once sync.Once
		out[i] = func() error {
			var err error
			once.Do(func() {
				if remaining.Add(-1) == 0 {
					err = ack()
				}
			})
			return err
		}
	}
	return out
}
