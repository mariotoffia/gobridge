package paho

import (
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
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
// Startup buffering vs post-grace settlement: a publish that matches NO
// registered handler is handled by the STARTUP GRACE WINDOW. During the
// grace window (restarted on every (re)connect via beginGrace) the
// publish is the queued backlog a resumed clean_start=false session
// receives on CONNACK before Receiver.Run has registered its filters, so
// it is held — un-acked, in arrival order — in a bounded pending buffer
// and flushed to the first handler whose filters cover it (a crash while
// buffered leads to broker redelivery, not loss). AFTER the grace window
// a still-unmatched publish is classified by whether its topic is still
// COVERED by a subscription the session wants (covered()):
//   - ORPHAN (no subscription still wants it — a route removed from config
//     whose subscription survives on the persistent session): it is ACKED
//     and DROPPED, so its un-acked slot no longer pins the broker's
//     Receive-Maximum in-flight window nor head-of-line-blocks in-order
//     acking for every later message on the shared session, and its exact
//     topic is UNSUBSCRIBED (deduped, best-effort) to converge broker state.
//   - COVERED (a still-desired subscription whose receiver handler
//     registered later than the grace window): it is RETAINED un-acked in
//     the pending buffer (HIGH-1). Ack-dropping a still-covered live-route
//     publish would convert startup slowness into acknowledged loss and
//     break at-least-once; instead it is held (bounded by receive_maximum)
//     until the handler registers and flushes it, or the broker redelivers
//     it on reconnect. QoS 0 covered publishes the buffer cannot hold are a
//     best-effort loss (no redelivery contract). Publishes buffered near the
//     end of the window are swept by the grace timer with the same
//     covered-retain / orphan-drop classification.
//
// On overflow of the pending buffer (within grace) the oldest QoS 0
// entry is evicted (QoS 0 has no redelivery contract); the buffer
// capacity is sized to the session's Receive Maximum, which bounds how
// many un-acked QoS 1/2 publishes the broker may have in flight.
type router struct {
	mu       sync.RWMutex
	wg       sync.WaitGroup
	handlers map[string]routerHandler
	// callbacksInFlight counts callbacks accepted under mu but not yet returned.
	// callbacksIdle closes on the transition to zero, allowing recycle quiescence
	// to wait with a context instead of an unbounded WaitGroup.Wait goroutine.
	callbacksInFlight int
	callbacksIdle     chan struct{}

	// pending buffers publishes that matched no registered handler,
	// bounded by pendingLimit (entries) AND pendingBytesLimit (payload
	// bytes). Guarded by mu.
	pending      []pendingPublish
	pendingLimit int
	// pendingChanged is rotated whenever a publish is buffered so managed
	// migration verification can wait event-wise for broker replay. Guarded by mu.
	pendingChanged chan struct{}
	// awaitManagedReplayHook is a deterministic test seam invoked when replay
	// verification starts. Production leaves it nil. Guarded by mu.
	awaitManagedReplayHook func()
	// connEpoch identifies the CURRENT broker connection generation. It is
	// bumped on every beginGrace (i.e. every OnConnectionUp) and stamped onto
	// each entry buffered under it (bufferLocked). On a RECONNECT, beginGrace
	// purges every entry whose epoch predates connEpoch: those carry dead acks
	// and the broker redelivers their QoS 1/2 fresh, so keeping them would
	// double-count against the receive_maximum count cap and ack-drop a live
	// message as a bogus overflow (A-1). Guarded by mu.
	connEpoch uint64
	// unsettled records every QoS 1/2 packet accepted in connEpoch until its
	// protocol Ack succeeds. The map is cleared on every epoch transition because
	// old packet handles die with their connection and the broker redelivers them.
	unsettledSeq uint64
	unsettled    map[uint64]time.Time
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
	// queueReserved is the single ownership budget shared by dispatchCh and
	// pending. queueChanged is rotated on release so blocked QoS 1/2 callbacks
	// can wait without polling. Reservations are keyed by immutable Publish
	// identity and released idempotently exactly once.
	queueReserved     int
	queueChanged      chan struct{}
	queueReservations map[*pahov5.Publish]struct{}
	// maxPayloadBytes is the effective application-body ceiling. The CONNECT
	// packet advertises the whole-packet limit (max_payload_bytes + metadata
	// allowance); this local guard enforces the finer body/metadata split the
	// broker cannot see. A violation is acked-and-dropped, never terminal
	// (MQTT-L1) — see ingressCapViolation.
	maxPayloadBytes uint32
	// poisonLogged dedups the poison Error log per violation class so a
	// poison flood cannot flood the log while the metric still counts every
	// drop. Keyed by the bounded class name (payload / user_properties /
	// metadata), never by attacker-controlled topic. Guarded by mu.
	poisonLogged map[string]struct{}
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
	// quiesced prevents all handler matching while a managed-subscription
	// recycle is in progress. discarding drops old-connection ingress without
	// invoking its ACK while the socket is being disconnected.
	quiesced   bool
	discarding bool
	// managedCleanupFilters are exact durable filters in history - desired.
	// They are matched before handlers so overlapping desired handlers cannot
	// ACK traffic delivered through a stale wildcard/shared subscription.
	managedCleanupFilters []string

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
	// covered reports whether a concrete publish topic is still covered by
	// a subscription the session wants. It lets settleUnmatched split a
	// post-grace unmatched publish into a still-desired live route (covered:
	// a subscription whose handler registered late — RETAINED un-acked so
	// at-least-once holds, HIGH-1) versus a benign orphan (a route removed
	// from config — acked, dropped, unsubscribed). Set by NewSession (wired to
	// Session.topicCovered); nil in tests / the legacy Route path, where
	// every unmatched publish is treated as an orphan (previous behaviour).
	// MUST be invoked without r.mu held (it takes the session mutex).
	covered func(topic string) bool
	// unsubscribed dedups the orphan warn log + unsubscribe per topic.
	// Guarded by mu.
	unsubscribed map[string]struct{}
	// coveredWarned dedups the covered-retention WARN per topic so a
	// high-throughput covered topic whose handler registered late (HIGH-1)
	// logs once, not once per retained publish. The metric still counts every
	// retention; only the log is deduped. Guarded by mu.
	coveredWarned map[string]struct{}
	// unsubCh feeds orphan topics to the single grace worker goroutine so
	// the (network-blocking) UNSUBSCRIBE never runs on the paho publish
	// callback. Created by beginGrace; nil until then. Guarded by mu.
	unsubCh chan string
	// stop terminates the grace worker goroutine; closed once by shutdown.
	stop     chan struct{}
	stopOnce sync.Once

	logger  *slog.Logger
	metrics ports.MetricsExporter
	// sessionID tags every router loss/drop metric so a multi-session
	// deployment can attribute an orphan/overflow/stale-purge drop to the
	// session that produced it (A-11). Empty when the router is built without
	// a session (legacy/test construction); the tag is then omitted.
	sessionID         string
	routeCount        atomic.Int64 // total messages received by dispatch
	dropCount         atomic.Int64 // messages dropped (pending-buffer overflow)
	bufferedCount     atomic.Int64 // messages held for a not-yet-registered handler
	unmatchedDropped  atomic.Int64 // orphan messages acked-and-dropped past grace
	coveredDropped    atomic.Int64 // covered-topic QoS 0 dropped past grace when the buffer could not hold it
	coveredRetained   atomic.Int64 // covered-topic messages RETAINED un-acked past grace (HIGH-1; NOT lost)
	overflowDropped   atomic.Int64 // QoS 1/2 acked-and-dropped because a broker exceeded receive_maximum (protocol violation; unreachable with a compliant broker)
	stalePurged       atomic.Int64 // old-connection entries discarded on reconnect purge or recycle-window discard (A-1 / MQTT-L4); QoS 1/2 redelivered, QoS 0 best-effort loss
	poisonDropped     atomic.Int64 // ingress-cap violations acked-and-dropped instead of latching terminal (MQTT-L1)
	ackAfterReconnect atomic.Int64 // settlements mapped to success because the connection cycled (ErrPacketNotFound); broker redelivers (MQTT-L5)
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
	pub   *pahov5.Publish
	ack   func() error
	epoch uint64
}

// pendingPublish is one buffered pre-registration publish together
// with its protocol-ack callback.
type pendingPublish struct {
	pub *pahov5.Publish
	ack func() error
	// epoch is the router.connEpoch under which this entry was buffered. A
	// reconnect (beginGrace) purges every entry whose epoch predates the new
	// connEpoch: its ack is dead and the broker redelivers the QoS 1/2 fresh
	// (A-1). Set under r.mu at buffer time.
	epoch uint64
	// retainCounted latches once this entry has been counted on
	// MetricMQTTRouterCoveredRetained (blocking-#4 dedup). Both the grace-end
	// sweep (settlePending) and the post-grace dispatch retention
	// (retainCovered) count covered retentions; without this marker an entry
	// buffered by retainCovered just before a racing grace-end sweep — or an
	// entry still retained across a second grace window on reconnect — would be
	// counted twice. Set under r.mu.
	retainCounted bool
}

// defaultPendingLimit bounds the pre-registration pending buffer when
// the session does not configure receive_maximum. It matches the MQTT
// v5 protocol default for Receive Maximum, which is what bounds the
// broker's un-acked QoS 1/2 in-flight window under manual
// acknowledgement.
const defaultPendingLimit = 65535

// defaultPendingBytesLimit caps the pre-registration pending buffer in
// payload bytes for QoS 0 ONLY (QoS 1/2 are never dropped for this ceiling —
// see bufferLocked). Without it a QoS 0 flood during a grace window could
// buffer gigabytes; 64 MiB is a generous ceiling that still bounds QoS 0
// memory. QoS 1/2 pending memory is bounded instead by the entry count cap
// (== receive_maximum), since dropping a QoS 1/2 is incompatible with
// at-least-once.
const defaultPendingBytesLimit int64 = 64 << 20

// defaultDispatchSize is used only by routers constructed without a Session.
// Session construction overrides it with the effective Receive Maximum.
const defaultDispatchSize = int(DefaultReceiveMaximum)

// routerOption customises a router at construction (functional options
// so new knobs do not churn the newRouter signature).
type routerOption func(*router)

// HandlerIDs returns a sorted snapshot of the registered handler IDs.
func (r *router) HandlerIDs() []string {
	r.mu.RLock()
	ids := make([]string, 0, len(r.handlers))
	for id := range r.handlers {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	sort.Strings(ids)
	return ids
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

// CoveredDroppedCount returns the number of COVERED-topic QoS 0 publishes
// dropped after the startup grace window because the pending buffer could not
// hold them (a best-effort loss — QoS 0 has no redelivery contract). Covered
// QoS 1/2 is never dropped: it is RETAINED un-acked (CoveredRetainedCount).
func (r *router) CoveredDroppedCount() int64 {
	return r.coveredDropped.Load()
}

// CoveredRetainedCount returns the number of publishes on STILL-COVERED topics
// RETAINED un-acked past the startup grace window (HIGH-1) — held for a late
// receiver handler (flushed on RegisterFiltered) or broker redelivery, NOT
// lost. Distinct from CoveredDroppedCount (covered QoS 0 the buffer could not
// hold) and from the orphan cleanup counted by UnmatchedDroppedCount.
func (r *router) CoveredRetainedCount() int64 {
	return r.coveredRetained.Load()
}

// OverflowDroppedCount returns the number of QoS 1/2 publishes acked-and-dropped
// because the startup pending buffer's COUNT cap (== receive_maximum) was hit
// with no evictable QoS 0 to reclaim — UNREACHABLE under a spec-compliant broker
// (Receive-Maximum flow control bounds in-flight QoS 1/2 at the count cap), so a
// non-zero value means a broker delivered more un-acked QoS 1/2 than the Receive
// Maximum it was granted. The byte ceiling NEVER drops QoS 1/2 (it governs QoS 0
// only), so this is distinct from best-effort QoS 0 overflow drops (folded into
// Stats' `dropped` aggregate) and from the covered/orphan past-grace drops
// (CoveredDroppedCount / UnmatchedDroppedCount).
func (r *router) OverflowDroppedCount() int64 {
	return r.overflowDropped.Load()
}

// StalePurgedCount returns the number of pre-registration pending publishes
// DISCARDED across reconnects because they were buffered under a prior broker
// connection (A-1). QoS 1/2 entries counted here are redelivered fresh by a
// clean_start=false broker (not lost); QoS 0 entries are a best-effort loss.
// A steadily rising value indicates frequent reconnects while receivers
// register slowly, not data loss for QoS 1/2.
func (r *router) StalePurgedCount() int64 {
	return r.stalePurged.Load()
}

// IngressPoisonDroppedCount returns the number of inbound publishes
// acked-and-dropped because they violated a local representational ingress
// cap the broker cannot enforce (MQTT-L1). Each is an acknowledged,
// deliberate loss — the alternative was a publisher-triggerable permanent
// terminal loop. Any non-zero value warrants finding the offending
// publisher (docs/runbooks/mqtt-ingress-poison.md).
func (r *router) IngressPoisonDroppedCount() int64 {
	return r.poisonDropped.Load()
}

// AckAfterReconnectCount returns the number of delivery settlements mapped
// to success because the underlying connection cycled between receive and
// settle (paho ErrPacketNotFound; MQTT-L5). Each one is a guaranteed broker
// redelivery — a burst after a reconnect storm predicts a duplicate flood
// on routes without downstream dedup.
func (r *router) AckAfterReconnectCount() int64 {
	return r.ackAfterReconnect.Load()
}

// RegisterEnvelope adapts a domain-shaped handler so port-side files
// (Receiver) can subscribe to incoming messages without importing the
// vendor SDK. The translation from *paho.Publish to *messaging.Envelope
// happens here, inside the ACL. The ack callback passed to h defers the
// MQTT protocol acknowledgement until delivery settlement; it is nil
// for publishes that need no protocol ack (QoS 0 / legacy Route path).
func (r *router) RegisterEnvelope(id string, clk clock.Clock, filters []string, h func(*messaging.Envelope, func() error)) {
	r.RegisterFiltered(id, filters, func(pub *pahov5.Publish, ack func() error) {
		h(EnvelopeFromPublish(pub, clk, r.metrics), ack)
	})
}

// paho.Router interface stubs — registration is done via Register/Unregister.
func (r *router) RegisterHandler(_ string, _ pahov5.MessageHandler) {}
func (r *router) UnregisterHandler(_ string)                        {}
func (r *router) SetDebugLogger(_ log.Logger)                       {}
