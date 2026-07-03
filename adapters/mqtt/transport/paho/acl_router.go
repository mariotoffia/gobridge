package paho

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// router dispatches incoming publishes to the registered handlers (one
// per Receiver on this Session) whose topic filters cover the publish
// topic. A WaitGroup tracks in-flight handler goroutines so
// Session.Close can await them.
//
// Manual acknowledgement: the Session runs the Paho client with
// EnableManualAcknowledgment and installs onPublishReceived as the
// client's publish callback. Each dispatch carries an ack callback
// bound to the originating publish; the Receiver threads it into the
// Delivery so the protocol PUBACK/PUBCOMP fires only when the runtime
// SETTLES the delivery (Delivery.Ack after outbox persist / pipeline
// completion) — never merely because dispatch returned. The Paho
// client serialises acknowledgements in receive order internally, so
// out-of-order settlement is safe (see delivery.go).
//
// Dispatch is SYNCHRONOUS: onPublishReceived blocks until every
// matching handler for a given publish has returned (for the runtime
// receiver a handler returns when emit returns, i.e. when the route
// runner has accepted the delivery into a concurrency slot). The Paho
// client calls the publish callbacks from a single routePublishPackets
// goroutine, so blocking here stops read-ahead when downstream is slow
// instead of spawning unbounded goroutines. Broker-level backpressure
// for QoS 1/2 additionally comes from the un-acked in-flight window
// (Receive Maximum) under manual acknowledgement.
//
// Per-receiver topic filtering: handlers register with the topic
// filters of their subscriptions (MQTT wildcards + and # supported,
// shared-subscription $share prefixes stripped). A publish is
// dispatched ONLY to handlers whose filters match, so two Receivers
// with disjoint subscriptions on a shared Session no longer process
// each other's messages. A handler registered WITHOUT filters matches
// everything (legacy behaviour).
//
// Startup buffering: a publish that matches NO registered handler
// (e.g. the queued backlog a persistent session receives on CONNACK
// before Receiver.Run has registered) is held in a bounded pending
// buffer instead of being dropped, and is flushed — in arrival order —
// to the first handler whose filters cover it. Buffered QoS 1/2
// publishes stay un-acked while buffered, so even a crash while
// buffered leads to broker redelivery, not loss. On overflow the
// oldest QoS 0 entry is evicted (QoS 0 has no redelivery contract);
// QoS 1/2 entries are never evicted-and-acked — the buffer capacity is
// sized to the session's Receive Maximum, which bounds how many
// un-acked QoS 1/2 publishes the broker may have in flight.
type router struct {
	mu       sync.RWMutex
	wg       sync.WaitGroup
	handlers map[string]routerHandler

	// pending buffers publishes that matched no registered handler,
	// bounded by pendingLimit. Guarded by mu.
	pending      []pendingPublish
	pendingLimit int

	logger        *slog.Logger
	metrics       ports.MetricsExporter
	routeCount    atomic.Int64 // total messages received by dispatch
	dropCount     atomic.Int64 // messages dropped (pending-buffer overflow)
	bufferedCount atomic.Int64 // messages held for a not-yet-registered handler
}

// routerHandler pairs a dispatch function with the topic filters that
// select it. Empty filters match every topic.
type routerHandler struct {
	fn      func(pub *pahov5.Publish, ack func() error)
	filters []string
}

// pendingPublish is one buffered pre-registration publish together
// with its protocol-ack callback.
type pendingPublish struct {
	pub *pahov5.Publish
	ack func() error
}

// defaultPendingLimit bounds the pre-registration pending buffer when
// the session does not configure receive_maximum. It matches the MQTT
// v5 protocol default for Receive Maximum, which is what bounds the
// broker's un-acked QoS 1/2 in-flight window under manual
// acknowledgement.
const defaultPendingLimit = 65535

func newRouter(logger *slog.Logger, metrics ports.MetricsExporter) *router {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	return &router{
		handlers:     make(map[string]routerHandler),
		pendingLimit: defaultPendingLimit,
		logger:       logger,
		metrics:      metrics,
	}
}

// setPendingLimit bounds the pending buffer; values < 1 keep the
// current limit. Called by NewSession with the session's effective
// Receive Maximum.
func (r *router) setPendingLimit(n int) {
	if n < 1 {
		return
	}
	r.mu.Lock()
	r.pendingLimit = n
	r.mu.Unlock()
}

// onPublishReceived is the Paho client's publish callback (installed
// via paho.ClientConfig.OnPublishReceived in Session.Start). It binds
// the manual-acknowledgment callback to the originating publish and
// dispatches. Returning true marks the publish handled; errors are
// never returned because failure handling is the runtime's job via the
// Delivery contract.
func (r *router) onPublishReceived(pr pahov5.PublishReceived) (bool, error) {
	pub := pr.Packet
	client := pr.Client
	var ack func() error
	if pub != nil && pub.QoS > 0 && client != nil {
		ack = func() error {
			if err := client.Ack(pub); err != nil {
				if errors.Is(err, pahov5.ErrPacketNotFound) {
					// The connection cycled between receive and settle:
					// the client's ack tracker was reset, the broker will
					// redeliver, and downstream dedup absorbs the
					// duplicate. Not a settlement failure.
					return nil
				}
				return MapError(err)
			}
			return nil
		}
	}
	r.dispatch(pub, ack)
	return true, nil
}

// Route implements paho.Router for compatibility with tests and the
// legacy router seam. Publishes entering through Route carry no
// protocol-ack callback (the Paho client auto-acks when it drives a
// Router); production traffic enters through onPublishReceived.
func (r *router) Route(pb *packets.Publish) {
	r.dispatch(pahov5.PublishFromPacketPublish(pb), nil)
}

// dispatch fans a publish out to every registered handler whose topic
// filters match, then BLOCKS until each has returned (synchronous
// dispatch). Each handler receives an independent copy of the Publish
// — struct, Payload and Properties are copied so handlers can safely
// inspect the data without racing on shared backing arrays. When no
// handler matches, the publish is buffered (bounded) until a matching
// handler registers.
func (r *router) dispatch(pub *pahov5.Publish, ack func() error) {
	if pub == nil {
		return
	}
	r.routeCount.Add(1)

	r.mu.Lock()
	matching := make([]routerHandler, 0, len(r.handlers))
	for _, h := range r.handlers {
		if matchesAnyFilter(h.filters, pub.Topic) {
			matching = append(matching, h)
		}
	}
	if len(matching) == 0 {
		buffered := r.bufferLocked(pub, ack)
		r.mu.Unlock()
		if buffered {
			r.bufferedCount.Add(1)
			r.metrics.Counter(MetricMQTTRouterBuffered, 1)
			logging.Debug(r.logger, "mqtt: buffered message (no matching handler registered yet)",
				"topic", pub.Topic,
				"qos", pub.QoS,
			)
		} else {
			r.dropCount.Add(1)
			r.metrics.Counter(MetricMQTTRouterDropped, 1)
			if r.logger != nil {
				r.logger.Warn("mqtt: dropped message (pending buffer full)",
					"topic", pub.Topic,
					"qos", pub.QoS,
				)
			}
		}
		return
	}
	r.mu.Unlock()

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelTrace,
			"mqtt: routing message",
			"topic", pub.Topic,
			"payload_len", len(pub.Payload),
			"handler_count", len(matching),
		)
	}

	r.fanout(pub, ack, matching)
}

// bufferLocked appends the publish to the pending buffer, evicting the
// oldest QoS 0 entry on overflow. Returns false when the publish had
// to be dropped (buffer full of QoS 1/2 entries, or the new publish is
// QoS 0 and there is no room). QoS 1/2 entries are never evicted: they
// are un-acked, so dropping them silently would either lose them (if
// acked) or head-of-line-block the ack stream (if not); capacity is
// sized to the broker's Receive Maximum window so QoS 1/2 overflow
// cannot occur in practice.
func (r *router) bufferLocked(pub *pahov5.Publish, ack func() error) bool {
	if len(r.pending) >= r.pendingLimit {
		if pub.QoS == 0 {
			return false
		}
		evicted := false
		for i := range r.pending {
			if r.pending[i].pub.QoS == 0 {
				r.pending = append(r.pending[:i], r.pending[i+1:]...)
				evicted = true
				break
			}
		}
		if !evicted {
			return false
		}
		r.dropCount.Add(1)
		r.metrics.Counter(MetricMQTTRouterDropped, 1)
	}
	r.pending = append(r.pending, pendingPublish{pub: pub, ack: ack})
	return true
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
	for i, h := range handlers {
		p := *pub
		if pub.Payload != nil {
			p.Payload = make([]byte, len(pub.Payload))
			copy(p.Payload, pub.Payload)
		}
		if len(handlers) > 1 && pub.Properties != nil && i > 0 {
			orig := pub.Properties
			cp := *orig
			if orig.User != nil {
				cp.User = make(pahov5.UserProperties, len(orig.User))
				copy(cp.User, orig.User)
			}
			if orig.CorrelationData != nil {
				cp.CorrelationData = make([]byte, len(orig.CorrelationData))
				copy(cp.CorrelationData, orig.CorrelationData)
			}
			if orig.SubscriptionIdentifier != nil {
				si := *orig.SubscriptionIdentifier
				cp.SubscriptionIdentifier = &si
			}
			if orig.PayloadFormat != nil {
				pf := *orig.PayloadFormat
				cp.PayloadFormat = &pf
			}
			if orig.MessageExpiry != nil {
				me := *orig.MessageExpiry
				cp.MessageExpiry = &me
			}
			if orig.TopicAlias != nil {
				ta := *orig.TopicAlias
				cp.TopicAlias = &ta
			}
			p.Properties = &cp
		}
		go func(handler routerHandler, ackPart func() error) {
			defer r.wg.Done()
			defer local.Done()
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
			handler.fn(&p, ackPart)
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

// Wait blocks until all in-flight handler goroutines have returned.
func (r *router) Wait() { r.wg.Wait() }

// Register adds a match-all handler with LEGACY auto-settle semantics:
// the protocol ack (when the publish carries one) fires as soon as the
// handler returns. Production receivers use RegisterEnvelope /
// RegisterFiltered so the ack can be deferred to delivery settlement;
// Register exists for tests and diagnostic taps.
func (r *router) Register(id string, h func(*pahov5.Publish)) {
	r.RegisterFiltered(id, nil, func(pub *pahov5.Publish, ack func() error) {
		h(pub)
		if ack != nil {
			_ = ack()
		}
	})
}

// RegisterFiltered adds a settlement-aware handler for the given ID,
// dispatched only for topics matching filters (empty filters = match
// all). Handlers receive an independent copy of the Publish struct,
// Payload, and Properties per invocation when multiple handlers match a
// publish. Handlers should treat Properties as read-only. Pending
// (pre-registration) publishes matching the filters are flushed to the
// new handler in arrival order.
func (r *router) RegisterFiltered(id string, filters []string, h func(*pahov5.Publish, func() error)) {
	entry := routerHandler{fn: h, filters: filters}
	r.mu.Lock()
	r.handlers[id] = entry
	flush := r.takePendingLocked(filters)
	if len(flush) > 0 {
		// Track the flush goroutine on the shared WaitGroup so
		// Session.Close awaits an in-progress flush.
		r.wg.Add(1)
	}
	r.mu.Unlock()

	if len(flush) > 0 {
		go func() {
			defer r.wg.Done()
			for _, p := range flush {
				r.fanout(p.pub, p.ack, []routerHandler{entry})
			}
		}()
	}

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelDebug, "mqtt: handler registered",
			"handler_id", id,
			"filter_count", len(filters),
			"flushed_pending", len(flush),
			"total_handlers", r.HandlerCount(),
		)
	}
}

// takePendingLocked removes and returns — preserving arrival order —
// every pending publish whose topic matches filters. Caller holds r.mu.
func (r *router) takePendingLocked(filters []string) []pendingPublish {
	if len(r.pending) == 0 {
		return nil
	}
	var flush []pendingPublish
	keep := r.pending[:0]
	for _, p := range r.pending {
		if matchesAnyFilter(filters, p.pub.Topic) {
			flush = append(flush, p)
		} else {
			keep = append(keep, p)
		}
	}
	r.pending = keep
	return flush
}

func (r *router) Unregister(id string) {
	r.mu.Lock()
	delete(r.handlers, id)
	r.mu.Unlock()
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelDebug, "mqtt: handler unregistered",
			"handler_id", id,
		)
	}
}

// HandlerCount returns the number of registered handlers.
func (r *router) HandlerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.handlers)
}

// PendingCount returns the number of publishes buffered while waiting
// for a matching handler to register.
func (r *router) PendingCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pending)
}

// Stats returns diagnostic counters for the message router.
func (r *router) Stats() (received, dropped int64) {
	return r.routeCount.Load(), r.dropCount.Load()
}

// RegisterEnvelope adapts a domain-shaped handler so port-side files
// (Receiver) can subscribe to incoming messages without importing the
// vendor SDK. The translation from *paho.Publish to *messaging.Envelope
// happens here, inside the ACL. The ack callback passed to h defers the
// MQTT protocol acknowledgement until delivery settlement; it is nil
// for publishes that need no protocol ack (QoS 0 / legacy Route path).
func (r *router) RegisterEnvelope(id string, clk clock.Clock, filters []string, h func(*messaging.Envelope, func() error)) {
	r.RegisterFiltered(id, filters, func(pub *pahov5.Publish, ack func() error) {
		h(EnvelopeFromPublish(pub, clk), ack)
	})
}

// paho.Router interface stubs — registration is done via Register/Unregister.
func (r *router) RegisterHandler(_ string, _ pahov5.MessageHandler) {}
func (r *router) UnregisterHandler(_ string)                        {}
func (r *router) SetDebugLogger(_ log.Logger)                       {}
