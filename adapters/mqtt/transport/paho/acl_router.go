package paho

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

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
// Startup buffering vs orphan ack-and-drop: a publish that matches NO
// registered handler is handled by the STARTUP GRACE WINDOW. During the
// grace window (restarted on every (re)connect via beginGrace) the
// publish is the queued backlog a resumed clean_start=false session
// receives on CONNACK before Receiver.Run has registered its filters, so
// it is held — un-acked, in arrival order — in a bounded pending buffer
// and flushed to the first handler whose filters cover it (a crash while
// buffered leads to broker redelivery, not loss). AFTER the grace window
// a still-unmatched publish is treated as an ORPHAN broker subscription
// (a route removed from config whose subscription survives on the
// persistent session): it is ACKED and DROPPED, so its un-acked slot no
// longer pins the broker's Receive-Maximum in-flight window nor
// head-of-line-blocks in-order acking for every later message on the
// shared session, and its exact topic is UNSUBSCRIBED (deduped,
// best-effort) to converge broker state. Publishes buffered near the end
// of the window that never gain a handler are swept by the grace timer
// with the same ack-drop-unsubscribe treatment.
//
// On overflow of the pending buffer (within grace) the oldest QoS 0
// entry is evicted (QoS 0 has no redelivery contract); the buffer
// capacity is sized to the session's Receive Maximum, which bounds how
// many un-acked QoS 1/2 publishes the broker may have in flight.
type router struct {
	mu       sync.RWMutex
	wg       sync.WaitGroup
	handlers map[string]routerHandler

	// pending buffers publishes that matched no registered handler,
	// bounded by pendingLimit (entries) AND pendingBytesLimit (payload
	// bytes). Guarded by mu.
	pending      []pendingPublish
	pendingLimit int
	// pendingBytes is the running sum of buffered payload sizes; capped
	// by pendingBytesLimit so a flood of large publishes cannot buffer
	// multiple gigabytes during a grace window. Guarded by mu.
	pendingBytes      int64
	pendingBytesLimit int64

	// dispatchCh decouples the paho publish-callback goroutine from the
	// (synchronous, possibly slow) dispatch path: onPublishReceived
	// enqueues here and a single dispatchLoop worker drains it in arrival
	// order. Bounded so a QoS 0 flood cannot grow unbounded — on a full
	// queue QoS 0 is dropped (no delivery contract) to keep paho's read
	// loop moving (PINGRESP/PUBACK), while QoS 1/2 blocks (bounded anyway
	// by the broker's Receive-Maximum window). Created lazily by beginGrace;
	// nil until then (unit tests driving dispatch/Route directly bypass it
	// and run inline). Guarded by mu.
	dispatchCh   chan dispatchItem
	dispatchSize int
	// dispatchDone is closed by dispatchLoop when it exits, so Close can
	// JOIN the worker (awaitDispatchLoop) before r.wg.Wait(). Without the
	// join, a dispatchLoop draining a buffered item after r.stop is closed
	// would call r.wg.Add via fanout CONCURRENTLY with the in-flight Wait
	// (WaitGroup Add-during-Wait → panic). Created with dispatchCh in
	// beginGrace; nil until then. Guarded by mu.
	dispatchDone chan struct{}
	// closing is set true by shutdown (under mu) so RegisterFiltered's
	// pending-flush path — the other r.wg producer — skips its r.wg.Add
	// once Close has begun. The mutex orders that skip against Close's
	// subsequent Wait, so no flush Add can race the Wait. Guarded by mu.
	closing bool

	// clk sources time for the startup grace window. Defaults to
	// clock.System; the Session injects its (possibly fake) clock.
	clk clock.Clock
	// graceWindow is how long unmatched publishes are buffered after a
	// (re)connect before being treated as orphans. Immutable after
	// construction.
	graceWindow time.Duration
	// graceDeadline is the instant after which an unmatched publish is
	// acked-and-dropped rather than buffered. Guarded by mu; reset on
	// every beginGrace.
	graceDeadline time.Time
	// graceTimer fires graceWindow after each beginGrace so buffered
	// orphans are swept even when the broker stops delivering (its
	// in-flight window is full of un-acked orphans). Guarded by mu.
	graceTimer   clock.Timer
	graceStarted bool // grace worker goroutine started; guarded by mu
	// unsubscribe issues a best-effort UNSUBSCRIBE for an orphan topic.
	// Set by NewSession; nil in tests / the legacy Route path.
	unsubscribe func(topic string)
	// unsubscribed dedups the orphan warn log + unsubscribe per topic.
	// Guarded by mu.
	unsubscribed map[string]struct{}
	// unsubCh feeds orphan topics to the single grace worker goroutine so
	// the (network-blocking) UNSUBSCRIBE never runs on the paho publish
	// callback. Created by beginGrace; nil until then. Guarded by mu.
	unsubCh chan string
	// stop terminates the grace worker goroutine; closed once by shutdown.
	stop     chan struct{}
	stopOnce sync.Once

	logger           *slog.Logger
	metrics          ports.MetricsExporter
	routeCount       atomic.Int64 // total messages received by dispatch
	dropCount        atomic.Int64 // messages dropped (pending-buffer overflow)
	bufferedCount    atomic.Int64 // messages held for a not-yet-registered handler
	unmatchedDropped atomic.Int64 // orphan messages acked-and-dropped past grace
}

// routerHandler pairs a dispatch function with the topic filters that
// select it. Empty filters match every topic.
//
// emitMu serializes every emit to this handler — the live dispatch path
// and the pending-flush path both acquire it, so a handler never sees
// concurrent or out-of-order emit (ports.Receiver requires sequential,
// in-order emit). inflight tracks currently-dispatching invocations so
// Unregister can await them and guarantee emit is never called after
// Run has returned.
type routerHandler struct {
	fn       func(pub *pahov5.Publish, ack func() error)
	filters  []string
	emitMu   *sync.Mutex
	inflight *sync.WaitGroup
}

// dispatchItem is one publish queued for the serialized dispatch worker.
type dispatchItem struct {
	pub *pahov5.Publish
	ack func() error
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

// defaultPendingBytesLimit caps the pre-registration pending buffer in
// payload bytes (independent of entry count). Without a byte cap the
// default 65535-entry buffer could hold multiple gigabytes of large
// publishes during a grace window; 64 MiB is a generous ceiling that
// still bounds memory under a large-message flood.
const defaultPendingBytesLimit int64 = 64 << 20

// defaultDispatchSize bounds the serialized dispatch queue that decouples
// the paho publish-callback goroutine from the synchronous dispatch path.
// It absorbs a burst without unbounded growth; on overflow QoS 0 is
// dropped and QoS 1/2 applies backpressure (see dispatchCh).
const defaultDispatchSize = 1024

// routerOption customises a router at construction (functional options
// so new knobs do not churn the newRouter signature).
type routerOption func(*router)

// withRouterClock injects the clock sourcing the startup grace window.
// A nil clock keeps the clock.System default.
func withRouterClock(clk clock.Clock) routerOption {
	return func(r *router) {
		if clk != nil {
			r.clk = clk
		}
	}
}

// withUnmatchedGrace sets the startup grace window. Values <= 0 keep the
// DefaultUnmatchedGrace default.
func withUnmatchedGrace(d time.Duration) routerOption {
	return func(r *router) {
		if d > 0 {
			r.graceWindow = d
		}
	}
}

// withUnsubscribe installs the callback the router uses to unsubscribe an
// orphan topic identified past the grace window. Nil disables the
// unsubscribe-on-resume hygiene (ack-and-drop still happens).
func withUnsubscribe(fn func(topic string)) routerOption {
	return func(r *router) { r.unsubscribe = fn }
}

func newRouter(logger *slog.Logger, metrics ports.MetricsExporter, opts ...routerOption) *router {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	r := &router{
		handlers:          make(map[string]routerHandler),
		pendingLimit:      defaultPendingLimit,
		pendingBytesLimit: defaultPendingBytesLimit,
		dispatchSize:      defaultDispatchSize,
		clk:               clock.System,
		graceWindow:       DefaultUnmatchedGrace,
		unsubscribed:      make(map[string]struct{}),
		stop:              make(chan struct{}),
		logger:            logger,
		metrics:           metrics,
	}
	for _, opt := range opts {
		opt(r)
	}
	// Seed the grace deadline so an unmatched publish arriving before the
	// first beginGrace (no connection yet) is buffered, not dropped.
	r.graceDeadline = r.clk.Now().Add(r.graceWindow)
	return r
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

// beginGrace (re)starts the unmatched-publish grace window for the
// current broker connection. It is called on every OnConnectionUp so a
// resumed session gets a fresh window covering re-subscription and
// receiver re-registration; without the restart, the second connection's
// backlog would be judged against a long-expired deadline and dropped as
// orphan traffic. The single grace worker goroutine is started lazily on
// the first call and re-armed (Timer.Reset) on subsequent calls, so a
// router that never connects (unit tests constructing it directly) spawns
// no goroutine.
func (r *router) beginGrace() {
	r.mu.Lock()
	r.graceDeadline = r.clk.Now().Add(r.graceWindow)
	if r.graceStarted {
		if r.graceTimer != nil {
			r.graceTimer.Reset(r.graceWindow)
		}
		r.mu.Unlock()
		return
	}
	r.graceStarted = true
	r.graceTimer = r.clk.NewTimer(r.graceWindow)
	r.unsubCh = make(chan string, orphanUnsubBuffer)
	// Start the serialized dispatch worker alongside the grace worker so
	// the production ingress path (onPublishReceived → dispatchCh) is
	// decoupled from paho's callback goroutine. Lazy start keeps
	// direct-dispatch unit tests goroutine-free. dispatchDone lets Close
	// join this worker before r.wg.Wait() (see awaitDispatchLoop).
	r.dispatchCh = make(chan dispatchItem, r.dispatchSize)
	r.dispatchDone = make(chan struct{})
	timerC := r.graceTimer.C()
	unsubCh := r.unsubCh
	dispatchCh := r.dispatchCh
	dispatchDone := r.dispatchDone
	r.mu.Unlock()

	go r.graceLoop(timerC, unsubCh)
	go r.dispatchLoop(dispatchCh, dispatchDone)
}

// rearmGrace restarts the unmatched-publish grace window WITHOUT starting
// the grace subsystem if it never ran. It is called on Unregister so a
// publish arriving in the unregister→re-register gap (a supervisor-
// restarted receiver) is BUFFERED for the replacement handler instead of
// acked-and-dropped past a long-expired grace deadline — the covered-topic
// QoS 1/2 loss window. When no connection has ever come up (graceStarted
// == false, e.g. a unit test) it is a no-op so no worker goroutine leaks.
func (r *router) rearmGrace() {
	r.mu.Lock()
	if r.graceStarted {
		r.graceDeadline = r.clk.Now().Add(r.graceWindow)
		if r.graceTimer != nil {
			r.graceTimer.Reset(r.graceWindow)
		}
	}
	r.mu.Unlock()
}

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
			return
		case item := <-ch:
			r.dispatch(item.pub, item.ack)
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

// orphanUnsubBuffer bounds the in-memory queue of orphan topics awaiting
// an UNSUBSCRIBE. Orphan topics are few (one per removed route) and the
// send is non-blocking, so this only needs to absorb a burst; on overflow
// the topic is not marked seen, so a later orphan publish retries it.
const orphanUnsubBuffer = 256

// graceLoop is the SINGLE background goroutine of the grace subsystem. It
// sweeps buffered orphans when the grace timer fires and drains the
// orphan-unsubscribe queue, until shutdown. Both channels are captured by
// value so the select never races beginGrace (the timer is only Reset,
// never reassigned; unsubCh is set once). Owning the network-blocking
// UNSUBSCRIBE here keeps it off the paho publish-callback goroutine and
// avoids the WaitGroup Add/Wait hazard of per-topic goroutines.
func (r *router) graceLoop(timerC <-chan time.Time, unsubCh <-chan string) {
	for {
		select {
		case <-r.stop:
			return
		case <-timerC:
			r.sweepUnmatched()
		case topic := <-unsubCh:
			if r.unsubscribe != nil {
				r.unsubscribe(topic)
			}
		}
	}
}

// sweepUnmatched acks-and-drops every publish still buffered when the
// grace window ends. After grace, a pending entry matches no registered
// handler (a matching registration would have flushed it), so it is an
// orphan: acking frees the broker's Receive-Maximum slot and unblocks
// in-order acking for the rest of the session even when no further
// traffic arrives to drive the dispatch path. Each orphan's exact topic
// is queued for a single (deduped) UNSUBSCRIBE. Runs on the grace worker.
func (r *router) sweepUnmatched() {
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	orphans := r.pending
	r.pending = nil
	r.pendingBytes = 0
	r.mu.Unlock()

	for i := range orphans {
		r.dropUnmatched(orphans[i].pub, orphans[i].ack)
	}
	for i := range orphans {
		r.enqueueUnsub(orphans[i].pub.Topic)
	}
}

// enqueueUnsub records topic as a seen orphan and queues a single
// best-effort UNSUBSCRIBE for it (deduped, one warn per topic — a natural
// rate limit). The queue send is non-blocking; on overflow, or before the
// grace worker exists (nil channel), the topic is left unseen so a later
// orphan publish retries. Ack-and-drop of the publish itself is
// unconditional and handled separately by dropUnmatched.
func (r *router) enqueueUnsub(topic string) {
	r.mu.Lock()
	if _, seen := r.unsubscribed[topic]; seen {
		r.mu.Unlock()
		return
	}
	// A nil channel makes this select fall through to default (send on a
	// nil channel blocks forever) — the no-worker / legacy path.
	select {
	case r.unsubCh <- topic:
		r.unsubscribed[topic] = struct{}{}
		r.mu.Unlock()
		if r.logger != nil {
			r.logger.Warn("mqtt: unmatched publish past startup grace — orphan broker subscription; "+
				"acking, dropping, and unsubscribing to converge broker state",
				"topic", topic,
			)
		}
	default:
		r.mu.Unlock()
	}
}

// dropUnmatched acks (freeing the broker in-flight slot) and drops one
// publish that matched no registered handler past the grace window. The
// ack is exactly the PUBACK/PUBCOMP the buffered orphan would otherwise
// never send; QoS 0 carries no ack. Metric + counter make the otherwise
// silent drop observable.
func (r *router) dropUnmatched(pub *pahov5.Publish, ack func() error) {
	r.unmatchedDropped.Add(1)
	r.metrics.Counter(MetricMQTTRouterUnmatchedDropped, 1)
	if ack != nil {
		if err := ack(); err != nil {
			logging.Debug(r.logger, "mqtt: ack of dropped unmatched (orphan) publish failed",
				"topic", pub.Topic,
				"error", err,
			)
		}
	}
}

// shutdown terminates the grace and dispatch worker goroutines and marks
// the router closing so RegisterFiltered's pending-flush stops producing
// into r.wg. Idempotent; called by Session.Close BEFORE awaitDispatchLoop
// + Wait.
func (r *router) shutdown() {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.stopOnce.Do(func() { close(r.stop) })
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
	r.enqueueDispatch(pub, ack)
	return true, nil
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
	r.mu.Lock()
	ch := r.dispatchCh
	r.mu.Unlock()
	if ch == nil {
		r.dispatch(pub, ack)
		return
	}
	item := dispatchItem{pub: pub, ack: ack}
	select {
	case ch <- item:
		return
	default:
	}
	if pub.QoS == 0 {
		r.dropCount.Add(1)
		r.metrics.Counter(MetricMQTTRouterDropped, 1)
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
	}
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
// handler matches, the publish is buffered (bounded) within the startup
// grace window, or acked-and-dropped (and its topic unsubscribed) as an
// orphan broker subscription once the window has elapsed.
func (r *router) dispatch(pub *pahov5.Publish, ack func() error) {
	if pub == nil {
		return
	}
	r.routeCount.Add(1)

	r.mu.Lock()
	matching := make([]routerHandler, 0, len(r.handlers))
	for _, h := range r.handlers {
		if matchesAnyFilter(h.filters, pub.Topic) {
			// Mark in-flight UNDER r.mu so it pairs with Unregister's
			// delete-then-Wait: once Unregister has removed a handler, no
			// new dispatch can add to its inflight WaitGroup, so Wait
			// drains only the already-started emits.
			h.inflight.Add(1)
			matching = append(matching, h)
		}
	}
	if len(matching) == 0 {
		// Within the startup grace window an unmatched publish is the
		// legitimate CONNACK backlog racing receiver registration: buffer
		// it (un-acked) for a handler that is about to register. Past the
		// window it is an orphan broker subscription: ack + drop it so its
		// un-acked slot stops pinning the broker's in-flight window and
		// head-of-line-blocking in-order acks, then unsubscribe its topic.
		if r.clk.Now().Before(r.graceDeadline) {
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
		r.dropUnmatched(pub, ack)
		r.enqueueUnsub(pub.Topic)
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
// QoS 0 and there is no room). The buffer is bounded by BOTH an entry
// count (pendingLimit, sized to Receive Maximum) AND a payload-byte
// ceiling (pendingBytesLimit) so a flood of large publishes cannot
// buffer gigabytes. QoS 1/2 entries are never evicted: they are
// un-acked, so dropping them silently would either lose them (if acked)
// or head-of-line-block the ack stream (if not).
func (r *router) bufferLocked(pub *pahov5.Publish, ack func() error) bool {
	size := pubBytes(pub)
	overLimit := len(r.pending) >= r.pendingLimit ||
		(r.pendingBytesLimit > 0 && r.pendingBytes+size > r.pendingBytesLimit)
	if overLimit {
		if pub.QoS == 0 {
			return false
		}
		// Evict the oldest QoS 0 entry to make room for a QoS 1/2 publish
		// (which carries a delivery contract and must not be dropped).
		evicted := false
		for i := range r.pending {
			if r.pending[i].pub.QoS == 0 {
				r.pendingBytes -= pubBytes(r.pending[i].pub)
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
	r.pendingBytes += size
	return true
}

// pubBytes estimates the retained memory of a buffered publish: topic +
// payload bytes. It is intentionally cheap (ignores property overhead);
// it only needs to bound the buffer, not account exactly.
func pubBytes(pub *pahov5.Publish) int64 {
	if pub == nil {
		return 0
	}
	return int64(len(pub.Topic) + len(pub.Payload))
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
	entry := routerHandler{
		fn:       h,
		filters:  filters,
		emitMu:   &sync.Mutex{},
		inflight: &sync.WaitGroup{},
	}
	r.mu.Lock()
	r.handlers[id] = entry
	flush := r.takePendingLocked(filters)
	// Skip the flush once Close has begun (r.closing set under r.mu by
	// shutdown): the flush goroutine's r.wg.Add would otherwise race
	// Close's r.wg.Wait. The mutex orders this decision against shutdown,
	// so either we Add before shutdown-before-Wait (safe) or we observe
	// closing and skip entirely. Skipped pending stays un-acked → the
	// broker redelivers to the next owner (at-least-once preserved).
	doFlush := len(flush) > 0 && !r.closing
	if doFlush {
		// Lock emitMu WHILE still holding r.mu (uncontended — brand-new
		// handler) so any live dispatch that observes this handler after
		// r.mu is released blocks on emitMu until the flush of the (older)
		// buffered publishes completes. That funnels flush and live
		// dispatch through ONE serialized, in-order path (ports.Receiver
		// sequential-emit contract). inflight/wg track the flush so
		// Unregister and Session.Close both await it.
		entry.emitMu.Lock()
		entry.inflight.Add(1)
		r.wg.Add(1)
	}
	r.mu.Unlock()

	if doFlush {
		go func() {
			defer r.wg.Done()
			defer entry.inflight.Done()
			defer entry.emitMu.Unlock()
			for _, p := range flush {
				r.emitOne(entry, p.pub, p.ack)
			}
		}()
	}

	if logging.DebugEnabled(r.logger) {
		flushed := 0
		if doFlush {
			flushed = len(flush)
		}
		r.logger.Log(context.Background(), logging.LevelDebug, "mqtt: handler registered",
			"handler_id", id,
			"filter_count", len(filters),
			"flushed_pending", flushed,
			"total_handlers", r.HandlerCount(),
		)
	}
}

// emitOne invokes handler.fn with an independent copy of pub (payload
// copied so the handler cannot race the shared backing array), recovering
// panics into the handler-panic metric. It does NOT touch emitMu,
// inflight or the shared WaitGroup — the caller owns that bookkeeping.
// Used by the pending-flush path, where a single handler consumes the
// publish so its Properties pointer may be shared safely.
func (r *router) emitOne(handler routerHandler, pub *pahov5.Publish, ack func() error) {
	p := *pub
	if pub.Payload != nil {
		p.Payload = make([]byte, len(pub.Payload))
		copy(p.Payload, pub.Payload)
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
		}
	}()
	handler.fn(&p, ack)
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
			r.pendingBytes -= pubBytes(p.pub)
		} else {
			keep = append(keep, p)
		}
	}
	r.pending = keep
	return flush
}

func (r *router) Unregister(id string) {
	r.mu.Lock()
	entry, ok := r.handlers[id]
	delete(r.handlers, id)
	r.mu.Unlock()

	if ok && entry.inflight != nil {
		// Await any in-flight dispatch to this handler so emit is never
		// invoked after Unregister returns — and therefore never after
		// the owning Receiver.Run has returned (ports.Receiver emit
		// lifetime). The delete above (under r.mu) guarantees no new
		// dispatch can add to inflight, so this Wait drains only the
		// already-started emits.
		entry.inflight.Wait()
	}

	// Re-arm the unmatched grace window: a receiver being unregistered is
	// often a restart (its Run exited and the supervisor will re-register),
	// so publishes arriving in the unregister→re-register gap must be
	// BUFFERED for the replacement handler, not acked-and-dropped past a
	// long-expired grace deadline (covered-topic QoS 1/2 loss window).
	r.rearmGrace()

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

// UnmatchedDroppedCount returns the number of orphan publishes acked and
// dropped after the startup grace window elapsed.
func (r *router) UnmatchedDroppedCount() int64 {
	return r.unmatchedDropped.Load()
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
