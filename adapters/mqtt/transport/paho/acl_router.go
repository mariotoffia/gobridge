package paho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	// packet advertises the corresponding whole-packet limit, while this local
	// guard rejects a non-compliant broker's oversize body before cloning it.
	maxPayloadBytes uint32
	// ingressPoison latches a packet that violates the accepted payload/metadata
	// representation contract into the owning Session's existing terminal
	// teardown lifecycle. Immutable after construction.
	ingressPoison func(error)
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
	sessionID        string
	routeCount       atomic.Int64 // total messages received by dispatch
	dropCount        atomic.Int64 // messages dropped (pending-buffer overflow)
	bufferedCount    atomic.Int64 // messages held for a not-yet-registered handler
	unmatchedDropped atomic.Int64 // orphan messages acked-and-dropped past grace
	coveredDropped   atomic.Int64 // covered-topic QoS 0 dropped past grace when the buffer could not hold it
	coveredRetained  atomic.Int64 // covered-topic messages RETAINED un-acked past grace (HIGH-1; NOT lost)
	overflowDropped  atomic.Int64 // QoS 1/2 acked-and-dropped because a broker exceeded receive_maximum (protocol violation; unreachable with a compliant broker)
	stalePurged      atomic.Int64 // pre-registration entries discarded on reconnect (A-1); QoS 1/2 redelivered, QoS 0 best-effort loss
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

// withCovered installs the predicate the router uses to tell a REAL
// live-route loss (a still-desired subscription whose handler registered
// late) from benign orphan cleanup when it acks-and-drops a publish past
// the grace window (M-3). Nil treats every post-grace drop as an orphan
// (the legacy/test behaviour).
func withCovered(fn func(topic string) bool) routerOption {
	return func(r *router) { r.covered = fn }
}

// withSessionTag records the session's client_id so router loss/drop metrics
// carry a session_id tag (A-11). Empty values are ignored (the tag is omitted).
func withSessionTag(id string) routerOption {
	return func(r *router) { r.sessionID = id }
}

func withDispatchCapacity(capacity int) routerOption {
	return func(r *router) {
		if capacity > 0 {
			r.dispatchSize = capacity
		}
	}
}

func withMaxPayloadBytes(maxPayloadBytes uint32) routerOption {
	return func(r *router) { r.maxPayloadBytes = maxPayloadBytes }
}

func withIngressPoison(fn func(error)) routerOption {
	return func(r *router) { r.ingressPoison = fn }
}

func newRouter(logger *slog.Logger, metrics ports.MetricsExporter, opts ...routerOption) *router {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	r := &router{
		handlers:          make(map[string]routerHandler),
		pendingChanged:    make(chan struct{}),
		queueChanged:      make(chan struct{}),
		queueReservations: make(map[*pahov5.Publish]struct{}),
		pendingLimit:      defaultPendingLimit,
		pendingBytesLimit: defaultPendingBytesLimit,
		dispatchSize:      defaultDispatchSize,
		clk:               clock.System,
		graceWindow:       DefaultUnmatchedGrace,
		unsubscribed:      make(map[string]struct{}),
		coveredWarned:     make(map[string]struct{}),
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

func (r *router) ingressMemoryStats() (depth, capacity int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.dispatchCh != nil {
		depth = len(r.dispatchCh)
	}
	return depth, r.dispatchSize
}

func (r *router) reserveQueueSlot(pub *pahov5.Publish, qos byte) bool {
	for {
		r.mu.Lock()
		if r.closing {
			r.mu.Unlock()
			return false
		}
		if r.queueReserved < r.dispatchSize {
			r.queueReserved++
			r.queueReservations[pub] = struct{}{}
			r.mu.Unlock()
			return true
		}
		changed := r.queueChanged
		r.mu.Unlock()
		if qos == 0 {
			return false
		}
		select {
		case <-changed:
		case <-r.stop:
			return false
		}
	}
}

func (r *router) releaseQueueReservation(pub *pahov5.Publish) {
	r.mu.Lock()
	r.releaseQueueReservationLocked(pub)
	r.mu.Unlock()
}

func (r *router) releaseQueueReservationLocked(pub *pahov5.Publish) {
	if _, reserved := r.queueReservations[pub]; !reserved {
		return
	}
	delete(r.queueReservations, pub)
	if r.queueReserved > 0 {
		r.queueReserved--
	}
	close(r.queueChanged)
	r.queueChanged = make(chan struct{})
}

// sessionTag returns the session_id metric tag for this router's drops, or
// nil when the router was built without a session (legacy/test). Spread it
// into a metrics Counter call: r.metrics.Counter(name, n, r.sessionTag()...).
func (r *router) sessionTag() []shared.Tag {
	if r.sessionID == "" {
		return nil
	}
	return []shared.Tag{{Key: shared.TagKeySessionID, Value: r.sessionID}}
}

type unsettledHealth struct {
	Count                    int
	OldestAge                time.Duration
	ReceiveWindowUtilization float64
}

// trackUnsettledPacket registers one QoS 1/2 packet in the current connection
// epoch and returns an idempotent successful-Ack callback.
func (r *router) trackUnsettledPacket() func() {
	r.mu.Lock()
	if r.unsettled == nil {
		r.unsettled = make(map[uint64]time.Time)
	}
	r.unsettledSeq++
	id := r.unsettledSeq
	r.unsettled[id] = r.clk.Now()
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.unsettled, id)
			r.mu.Unlock()
		})
	}
}

// trackAcknowledgement registers a packet and removes it only after the
// protocol acknowledgement succeeds. Failed ACKs remain visible and unsettled.
func (r *router) trackAcknowledgement(ack func() error) func() error {
	settled := r.trackUnsettledPacket()
	return func() error {
		if err := ack(); err != nil {
			return err
		}
		settled()
		return nil
	}
}

func (r *router) clearUnsettledLocked() {
	clear(r.unsettled)
}

func (r *router) unsettledSnapshot(receiveMaximum uint16) unsettledHealth {
	r.mu.RLock()
	count := len(r.unsettled)
	var oldest time.Time
	for _, receivedAt := range r.unsettled {
		if oldest.IsZero() || receivedAt.Before(oldest) {
			oldest = receivedAt
		}
	}
	r.mu.RUnlock()

	var age time.Duration
	if !oldest.IsZero() {
		age = r.clk.Since(oldest)
		if age < 0 {
			age = 0
		}
	}
	var utilization float64
	if receiveMaximum > 0 {
		utilization = float64(count) / float64(receiveMaximum)
	}
	return unsettledHealth{
		Count:                    count,
		OldestAge:                age,
		ReceiveWindowUtilization: utilization,
	}
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
	// Advance the connection generation. Entries buffered from here on are
	// stamped with this epoch (bufferLocked); on the RECONNECT path below,
	// entries stamped with a prior epoch are purged (A-1).
	r.connEpoch++
	r.clearUnsettledLocked()
	// Replacement ingress may be buffered while global quiescence remains.
	r.discarding = false
	if r.graceStarted {
		// Reconnect. A clean_start=false broker replays every un-acked QoS 1/2
		// from the prior connection with FRESH packet IDs, so any entry still
		// buffered under a previous epoch is a stale twin whose ack died with
		// the old connection. Purge them: keeping them lets a redelivered copy
		// accumulate beside its ghost until the count cap (== receive_maximum)
		// ack-drops a LIVE message as a bogus overflow, breaking at-least-once.
		r.purgeStalePendingLocked()
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
			r.dispatchAtEpoch(item.pub, item.ack, item.epoch, true)
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
			r.sweepIfExpired()
		case topic := <-unsubCh:
			if r.unsubscribe != nil {
				r.unsubscribe(topic)
			}
		}
	}
}

// sweepIfExpired runs the grace-end settle, but ONLY when the grace window has
// truly elapsed. beginGrace and rearmGrace re-arm the shared grace timer with
// Timer.Reset from another goroutine; if that timer had ALREADY fired, a stale
// tick is left sitting in the timer channel and the worker would act on it
// immediately after the re-arm — sweeping BEFORE the new deadline, ack-dropping
// orphans and firing retention metrics ahead of the configured grace (A-12).
// graceDeadline is advanced under r.mu on every arm, so comparing it against
// now distinguishes a genuine expiry from a stale tick: a premature tick re-arms
// the timer for the remaining window and skips the sweep. Runs on the grace
// worker; the check and the re-arm are done under r.mu so they never interleave
// with a concurrent beginGrace/rearmGrace Reset.
func (r *router) sweepIfExpired() {
	r.mu.Lock()
	remaining := r.graceDeadline.Sub(r.clk.Now())
	if remaining > 0 {
		if r.graceTimer != nil {
			r.graceTimer.Reset(remaining)
		}
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	r.sweepUnmatched()
}

// sweepUnmatched runs when the startup grace window ends: it settles every
// publish still buffered at that instant. A pending entry whose topic is still
// COVERED by a subscription the session wants (a receiver whose handler has not
// registered yet) is RETAINED in place — un-acked — so at-least-once holds
// until the handler registers (RegisterFiltered flushes it) or the broker
// redelivers on reconnect (HIGH-1); ack-dropping it would be acknowledged
// live-route loss. Only a true ORPHAN (a topic no subscription still wants) is
// acked, dropped, and (deduped) unsubscribed. Runs on the grace worker. The
// covered entries it retains ARE counted on MetricMQTTRouterCoveredRetained
// (WARN deduped per topic) so a slow/absent receiver is visible (blocking-#4).
func (r *router) sweepUnmatched() {
	r.settlePending(true)
}

// reclassifyPending re-evaluates every buffered publish against the CURRENT
// coverage after a successful reconcile changed the session's subscriptions
// (blocking-#1). A publish RETAINED-as-covered past the grace window becomes a
// TRUE ORPHAN the instant its covering subscription is removed, but nothing
// else re-sweeps it — it would stay un-acked forever, pinning the broker
// Receive-Maximum window and wedging ingress. Running the same settle pass
// reclassifies any now-uncovered entry as an orphan (ack, drop, unsubscribe)
// while leaving still-covered entries in place. Still-covered retentions are
// NOT re-counted on MetricMQTTRouterCoveredRetained — they were counted at
// grace end (sweepUnmatched) or are still within the grace window — so a
// reconcile does not inflate the retention metric. Called from Reconcile with
// NO session mutex held; a no-op when the pending buffer is empty.
func (r *router) reclassifyPending() {
	r.settlePending(false)
}

// settlePending settles every publish currently buffered: it snapshots the
// pending topics, classifies each as still-COVERED (retain in place) or ORPHAN
// (ack, drop, unsubscribe), then removes only the orphans under r.mu. covered()
// (which takes the SESSION mutex) is consulted with r.mu RELEASED — the
// lock-order rule forbids r.mu → s.mu — and the removal re-locks and acts on
// the CURRENT buffer so a concurrent RegisterFiltered flush or fresh dispatch
// that changed r.pending in the meantime is respected. countRetained gates
// whether still-covered retentions bump MetricMQTTRouterCoveredRetained: the
// grace-end sweep counts them (blocking-#4), a reconcile-triggered
// reclassification does not (avoids double-counting an already-counted or
// still-in-grace retention).
func (r *router) settlePending(countRetained bool) {
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	snapshot := make([]*pahov5.Publish, len(r.pending))
	for i := range r.pending {
		snapshot[i] = r.pending[i].pub
	}
	managedGateAll := r.quiesced
	managedFilters := append([]string(nil), r.managedCleanupFilters...)
	r.mu.Unlock()

	orphan := make(map[*pahov5.Publish]struct{}, len(snapshot))
	var coveredSnap map[*pahov5.Publish]struct{}
	if countRetained {
		coveredSnap = make(map[*pahov5.Publish]struct{}, len(snapshot))
	}
	for _, pub := range snapshot {
		if managedGateAll || (len(managedFilters) > 0 && matchesAnyFilter(managedFilters, pub.Topic)) {
			if countRetained {
				coveredSnap[pub] = struct{}{}
			}
			continue
		}
		if r.covered != nil && r.covered(pub.Topic) {
			if countRetained {
				coveredSnap[pub] = struct{}{}
			}
			continue // covered: retain in place
		}
		orphan[pub] = struct{}{}
	}

	// Re-lock and remove ONLY the orphan entries still present, keeping every
	// covered entry IN PLACE (not take-all-then-rebuffer, which would strand a
	// covered publish behind a concurrent RegisterFiltered whose
	// takePendingLocked already ran). Subtract only the dropped bytes. Count a
	// retention only for a covered entry STILL pending after the re-lock, and
	// only ONCE per entry (retainCounted latches it) so a post-grace
	// retainCovered dispatch or a second grace window does not double-count.
	r.mu.Lock()
	kept := r.pending[:0]
	var dropped []pendingPublish
	retainedByTopic := make(map[string]int)
	for i := range r.pending {
		p := r.pending[i]
		if _, isOrphan := orphan[p.pub]; isOrphan {
			dropped = append(dropped, p)
			r.pendingBytes -= pubBytes(p.pub)
			continue
		}
		if countRetained && !p.retainCounted {
			if _, wasCovered := coveredSnap[p.pub]; wasCovered {
				retainedByTopic[p.pub.Topic]++
				p.retainCounted = true // latch so a later sweep never re-counts it
			}
		}
		kept = append(kept, p)
	}
	r.pending = kept
	r.mu.Unlock()

	for i := range dropped {
		r.dropOrphan(dropped[i].pub, dropped[i].ack)
	}
	for i := range dropped {
		r.enqueueUnsub(dropped[i].pub.Topic)
	}
	// Count/log the covered entries retained past grace (grace sweep only).
	// noteCoveredRetained takes r.mu, so it runs with r.mu released here.
	for topic, n := range retainedByTopic {
		r.noteCoveredRetained(topic, n)
	}
}

// enqueueUnsub records topic as a seen orphan and queues a single
// best-effort UNSUBSCRIBE for it (deduped, one warn per topic — a natural
// rate limit). The queue send is non-blocking; on overflow, or before the
// grace worker exists (nil channel), the topic is left unseen so a later
// orphan publish retries. Ack-and-drop of the publish itself is handled
// separately by dropOrphan (only true orphans reach here — covered topics
// are retained, not unsubscribed).
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

// dropOrphan acks (freeing the broker in-flight slot) and drops one publish
// whose topic no subscription still wants — an ORPHAN broker subscription that
// survived on a resumed clean_start=false session (a route removed from
// config). The ack is exactly the PUBACK/PUBCOMP the buffered orphan would
// otherwise never send; QoS 0 carries no ack.
//
// COVERED topics are NEVER routed here: a still-desired subscription whose
// receiver handler registered late is RETAINED un-acked instead
// (settleUnmatched → retainCovered, HIGH-1), because ack-dropping it would
// convert startup slowness into acknowledged live-route loss and break
// at-least-once. Orphan classification is done by the caller with r.mu
// released (covered() takes the session mutex).
func (r *router) dropOrphan(pub *pahov5.Publish, ack func() error) {
	r.releaseQueueReservation(pub)
	r.unmatchedDropped.Add(1)
	r.metrics.Counter(MetricMQTTRouterUnmatchedDropped, 1, r.sessionTag()...)
	if ack != nil {
		if err := ack(); err != nil {
			logging.Debug(r.logger, "mqtt: ack of dropped orphan publish failed",
				"topic", pub.Topic,
				"error", err,
			)
		}
	}
}

// settleUnmatched handles a publish that matched NO registered handler AFTER
// the startup grace window. It NEVER ack-drops a still-covered topic — doing so
// would convert startup slowness into acknowledged live-route loss and break
// at-least-once (HIGH-1). A covered publish is RETAINED un-acked in the pending
// buffer (bounded by receive_maximum) so a late RegisterFiltered flushes it, or
// the broker redelivers it on reconnect. Only a true orphan (a topic no
// subscription still wants) is acked, dropped, and unsubscribed. covered() MUST
// be consulted with r.mu released (it takes the session mutex); a nil covered
// predicate treats every unmatched publish as an orphan (legacy/test behaviour).
func (r *router) settleUnmatched(pub *pahov5.Publish, ack func() error) {
	if r.covered != nil && r.covered(pub.Topic) {
		r.retainCovered(pub, ack)
		return
	}
	r.dropOrphan(pub, ack)
	r.enqueueUnsub(pub.Topic)
}

// retainCovered keeps a still-covered publish that matched no handler past the
// grace window UN-ACKED in the pending buffer, so at-least-once holds until the
// receiver handler registers (RegisterFiltered flushes it) or the broker
// redelivers it on reconnect (HIGH-1). It first re-scans handlers under r.mu to
// close the TOCTOU where a handler registered between the dispatch miss and
// here (dispatching to it directly). On a buffer refusal the fallback preserves
// the QoS contract: QoS 1/2 (reachable only when a broker exceeds the granted
// receive_maximum) is ack-dropped on the protocol-violation metric; QoS 0 (no
// redelivery contract) is a best-effort covered drop. Caller must hold NEITHER
// r.mu (this re-locks) NOR have consulted covered() under r.mu.
func (r *router) retainCovered(pub *pahov5.Publish, ack func() error) {
	r.mu.Lock()
	// TOCTOU: a handler may have registered since the dispatch miss. If one now
	// covers the topic, dispatch to it instead of buffering (mark in-flight
	// under r.mu so it pairs with Unregister's delete-then-Wait).
	matching := make([]routerHandler, 0, 1)
	for _, h := range r.handlers {
		if matchesAnyFilter(h.filters, pub.Topic) {
			h.inflight.Add(1)
			matching = append(matching, h)
		}
	}
	if len(matching) > 0 {
		r.addCallbacksLocked(len(matching))
		r.mu.Unlock()
		r.releaseQueueReservation(pub)
		r.fanout(pub, ack, matching)
		return
	}
	buffered := r.bufferLocked(pub, ack)
	if buffered {
		// This post-grace retention is counted immediately below. Mark the
		// just-buffered entry (bufferLocked appends it last) as counted so a
		// grace-end sweep racing this dispatch does not count it again
		// (blocking-#4 dedup).
		if n := len(r.pending); n > 0 {
			r.pending[n-1].retainCounted = true
		}
	}
	r.mu.Unlock()
	if buffered {
		r.noteCoveredRetained(pub.Topic, 1)
		return
	}
	// Buffer refused.
	if pub.QoS > 0 {
		// Covered QoS 1/2 the buffer cannot hold: only a receive_maximum-
		// exceeding broker (protocol violation) reaches here. Ack-drop the
		// victim to keep paho's contiguous-prefix ack stream draining.
		r.overflowAckDrop(pub, ack)
		return
	}
	// Covered QoS 0 the buffer cannot hold: best-effort drop (no redelivery
	// contract). Counted on the covered-drop metric for visibility.
	r.coveredDropped.Add(1)
	r.metrics.Counter(MetricMQTTRouterCoveredDropped, 1, r.sessionTag()...)
	if ack != nil {
		if err := ack(); err != nil {
			logging.Debug(r.logger, "mqtt: ack of covered QoS 0 overflow-dropped publish failed",
				"topic", pub.Topic, "error", err)
		}
	}
	if r.logger != nil {
		r.logger.Warn("mqtt: DROPPED covered QoS 0 publish past startup grace — a still-desired "+
			"subscription's handler registered late and the pending buffer is full; QoS 0 has no "+
			"redelivery contract so this is a best-effort loss",
			"topic", pub.Topic, "qos", pub.QoS)
	}
}

// noteCoveredRetained records n covered publishes on topic retained un-acked
// past the grace window (HIGH-1): it always bumps the counter and metric, and
// WARN-logs ONCE per topic (deduped via coveredWarned) so a high-throughput
// covered backlog does not spam the log while every retention is still counted.
func (r *router) noteCoveredRetained(topic string, n int) {
	if n <= 0 {
		return
	}
	r.coveredRetained.Add(int64(n))
	r.metrics.Counter(MetricMQTTRouterCoveredRetained, int64(n), r.sessionTag()...)
	r.mu.Lock()
	_, warned := r.coveredWarned[topic]
	if !warned {
		r.coveredWarned[topic] = struct{}{}
	}
	r.mu.Unlock()
	if !warned && r.logger != nil {
		r.logger.Warn("mqtt: RETAINED covered publish past startup grace — a still-desired "+
			"subscription's receiver handler has not registered yet; keeping it UN-ACKED "+
			"(bounded by receive_maximum) so at-least-once holds until the handler registers "+
			"or the broker redelivers on reconnect",
			"topic", topic,
		)
	}
}

// overflowAckDrop ack-drops a QoS 1/2 publish the pending buffer refused. This
// is reachable ONLY when a broker exceeds the receive_maximum it was granted
// (protocol violation): the buffer's count cap == receive_maximum, so a
// compliant broker's flow control never delivers a QoS 1/2 past a full buffer.
// Acking the victim keeps paho's contiguous-prefix ack stream draining (an
// un-acked victim would head-of-line-block every later ack and, once
// receive_maximum slots fill, wedge ingress); the buffered prefix still settles
// on handler registration. Bumps the aggregate drop counter and the dedicated
// protocol-violation metric. Caller must hold NEITHER r.mu.
func (r *router) overflowAckDrop(pub *pahov5.Publish, ack func() error) {
	r.releaseQueueReservation(pub)
	r.dropCount.Add(1)
	r.overflowDropped.Add(1)
	r.metrics.Counter(MetricMQTTRouterOverflowDropped, 1, r.sessionTag()...)
	if ack != nil {
		if err := ack(); err != nil {
			logging.Debug(r.logger, "mqtt: ack of overflow-dropped QoS 1/2 publish failed",
				"topic", pub.Topic,
				"error", err,
			)
		}
	}
	if r.logger != nil {
		r.logger.Warn("mqtt: DROPPED QoS 1/2 publish — broker exceeded the granted "+
			"receive_maximum (protocol violation) and the startup pending buffer holds no "+
			"evictable QoS 0; acked-and-dropped to keep paho's in-order ack stream draining. "+
			"MESSAGE LOST — investigate the broker.",
			"topic", pub.Topic,
			"qos", pub.QoS,
		)
	}
}

// dropQoS0Overflow drops a QoS 0 publish the pending buffer refused during the
// startup grace window. QoS 0 carries no delivery contract and no ack, so the
// drop is best-effort and counted only on the generic drop metric. Caller must
// hold NEITHER r.mu.
func (r *router) dropQoS0Overflow(pub *pahov5.Publish) {
	r.releaseQueueReservation(pub)
	r.dropCount.Add(1)
	r.metrics.Counter(MetricMQTTRouterDropped, 1, r.sessionTag()...)
	if r.logger != nil {
		r.logger.Warn("mqtt: dropped QoS 0 publish (pending buffer full during startup grace)",
			"topic", pub.Topic,
			"qos", pub.QoS,
		)
	}
}

// shutdown terminates the grace and dispatch worker goroutines and marks
// the router closing so RegisterFiltered's pending-flush stops producing
// into r.wg. Idempotent; called by Session.Close BEFORE awaitDispatchLoop
// + Wait.
func (r *router) shutdown() {
	r.mu.Lock()
	r.closing = true
	clear(r.queueReservations)
	r.queueReserved = 0
	close(r.queueChanged)
	r.queueChanged = make(chan struct{})
	r.mu.Unlock()
	r.stopOnce.Do(func() { close(r.stop) })
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
	if r.rejectIngressPacket(received) {
		return true, nil
	}
	pub := publishWithIdentity(received)
	client := pr.Client
	var ack func() error
	if received != nil && received.QoS > 0 && client != nil {
		ack = r.trackAcknowledgement(func() error {
			if err := client.Ack(received); err != nil {
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
		})
	}
	r.enqueueDispatch(pub, ack)
	return true, nil
}

func (r *router) rejectIngressPacket(pub *pahov5.Publish) bool {
	if pub == nil {
		return false
	}
	userProperties := 0
	if pub.Properties != nil {
		userProperties = len(pub.Properties.User)
	}
	var violation error
	switch {
	case r.maxPayloadBytes > 0 && uint64(len(pub.Payload)) > uint64(r.maxPayloadBytes):
		violation = shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"mqtt: inbound payload %d exceeds max_payload_bytes %d",
			len(pub.Payload), r.maxPayloadBytes,
		))
	case userProperties > maxIngressUserProperties:
		violation = shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"mqtt: inbound User Properties count %d exceeds retained-memory cap %d",
			userProperties, maxIngressUserProperties,
		))
	case ingressMetadataBytes(pub) > maxIngressMetadataBytes:
		violation = shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"mqtt: inbound metadata exceeds retained-memory cap %d bytes",
			maxIngressMetadataBytes,
		))
	}
	if violation == nil {
		return false
	}
	r.dropCount.Add(1)
	r.metrics.Counter(MetricMQTTRouterDropped, 1, r.sessionTag()...)
	if r.logger != nil {
		r.logger.Error("mqtt: rejected inbound packet before adapter enqueue; terminating session",
			"topic", pub.Topic,
			"qos", pub.QoS,
			"payload_bytes", len(pub.Payload),
			"max_payload_bytes", r.maxPayloadBytes,
			"user_properties", userProperties,
			"error", violation,
		)
	}
	if r.ingressPoison != nil {
		r.ingressPoison(violation)
	}
	return true
}

func ingressMetadataBytes(pub *pahov5.Publish) uint64 {
	if pub == nil {
		return 0
	}
	// Fixed header + worst-case Remaining Length + topic length prefix +
	// packet identifier + worst-case property-length VBI.
	total := uint64(1 + 4 + 2 + len(pub.Topic) + 2 + 4)
	if pub.Properties == nil {
		return total
	}
	props := pub.Properties
	if props.PayloadFormat != nil {
		total += 2
	}
	if props.MessageExpiry != nil {
		total += 5
	}
	if props.ContentType != "" {
		total += uint64(3 + len(props.ContentType))
	}
	if props.ResponseTopic != "" {
		total += uint64(3 + len(props.ResponseTopic))
	}
	if props.CorrelationData != nil {
		total += uint64(3 + len(props.CorrelationData))
	}
	if props.SubscriptionIdentifier != nil {
		total += 5
	}
	if props.TopicAlias != nil {
		total += 3
	}
	for _, property := range props.User {
		total += uint64(5 + len(property.Key) + len(property.Value))
	}
	return total
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
		r.releaseQueueReservation(pub)
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
	if r.rejectIngressPacket(pub) {
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
		r.releaseQueueReservation(pub)
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

// setManagedCleanupFilters atomically replaces the exact history-minus-desired
// gate. No handler match can race past a newly installed stale-filter gate.
func (r *router) setManagedCleanupFilters(filters []string) {
	if r == nil {
		return
	}
	copyFilters := append([]string(nil), filters...)
	sort.Strings(copyFilters)
	r.mu.Lock()
	r.managedCleanupFilters = copyFilters
	r.mu.Unlock()
}

// quiesceForRecycle waits for active handler dispatch, purges old-epoch pending
// deliveries without ACK, and makes subsequent old-socket ingress discard-only.
func (r *router) quiesceForRecycle(ctx context.Context, waitSettlement func(context.Context) error) error {
	if r == nil {
		return nil
	}
	// The state transition is under the same mutex dispatchCore uses before
	// handler matching. It therefore stops acceptance immediately without
	// waiting behind a stuck callback.
	r.mu.Lock()
	r.quiesced = true
	r.discarding = true
	r.connEpoch++
	r.clearUnsettledLocked()
	r.purgeStalePendingLocked()
	callbacks := r.callbacksInFlight
	idle := r.callbacksIdle
	r.mu.Unlock()

	if callbacks > 0 {
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Callback return only means the RouteRunner accepted a Delivery. Its
	// authoritative in-flight counter remains non-zero through processing and
	// settlement, so wait on the runtime-installed barrier as the second phase.
	if waitSettlement != nil {
		if err := waitSettlement(ctx); err != nil {
			return err
		}
	}
	return nil
}

// addCallbacksLocked records callbacks before dispatch releases mu, closing the
// match-to-quiesce race where a callback had been selected but not yet added to
// a WaitGroup. Caller must hold r.mu.
func (r *router) addCallbacksLocked(n int) {
	if n <= 0 {
		return
	}
	if r.callbacksInFlight == 0 {
		r.callbacksIdle = make(chan struct{})
	}
	r.callbacksInFlight += n
}

func (r *router) callbackDone() {
	r.mu.Lock()
	// fanout is an internal legacy test seam that can be invoked directly,
	// outside dispatch selection. Such calls are still covered by r.wg for Close
	// but were never accepted into the recycle counter.
	if r.callbacksInFlight > 0 {
		r.callbacksInFlight--
		if r.callbacksInFlight == 0 && r.callbacksIdle != nil {
			close(r.callbacksIdle)
		}
	}
	r.mu.Unlock()
}

// resumeManagedDispatch releases the recycle gate only after replacement
// convergence and routes buffered replacement deliveries before live traffic.
func (r *router) resumeManagedDispatch(ctx context.Context) error {
	if r == nil {
		return nil
	}
	// Keep quiesced true while draining entries that have a handler NOW. An
	// unmatched current-epoch entry stays pending for RegisterFiltered instead of
	// being extracted, rebuffered by dispatchCore, and selected again in a busy
	// loop. Process one entry at a time so cancellation is checked between
	// callbacks without losing an extracted batch.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			return err
		}
		r.discarding = false
		idx := -1
		for i := range r.pending {
			pending := r.pending[i]
			if pending.epoch != r.connEpoch ||
				(len(r.managedCleanupFilters) > 0 && matchesAnyFilter(r.managedCleanupFilters, pending.pub.Topic)) {
				continue
			}
			for _, handler := range r.handlers {
				if matchesAnyFilter(handler.filters, pending.pub.Topic) {
					idx = i
					break
				}
			}
			if idx >= 0 {
				break
			}
		}
		if idx < 0 {
			r.quiesced = false
			r.mu.Unlock()
			return nil
		}
		pending := r.pending[idx]
		copy(r.pending[idx:], r.pending[idx+1:])
		r.pending = r.pending[:len(r.pending)-1]
		r.pendingBytes -= pubBytes(pending.pub)
		epoch := r.connEpoch
		r.mu.Unlock()
		r.dispatchCore(pending.pub, pending.ack, epoch, false, true)
	}
}

// bufferLocked appends the publish to the pre-registration pending buffer,
// enforcing the buffer's two independent bounds ASYMMETRICALLY by QoS because
// dropping a QoS 1/2 publish is never safe:
//
//   - QoS 0 (no delivery contract): admitted only while under BOTH the entry
//     count cap (pendingLimit, sized to Receive Maximum) AND the payload-byte
//     ceiling (pendingBytesLimit). Over either cap it is refused (return
//     false) — a best-effort drop, always safe for QoS 0.
//
//   - QoS 1/2 (at-least-once): ALWAYS buffered. It is NEVER dropped for the
//     BYTE ceiling — that ceiling governs QoS 0 memory only. A QoS 1/2 drop is
//     never safe: ack+drop loses the message; un-ack+drop head-of-line-blocks
//     paho's CONTIGUOUS-PREFIX manual-ack stream (acksTracker.flush sends the
//     acknowledged prefix and stops at the first un-acked entry), stranding
//     acks for messages that WERE delivered and, once receive_maximum un-acked
//     slots accumulate, wedging ingress on a stable connection. QoS 1/2 memory
//     needs no byte cap: the broker's Receive-Maximum flow control never
//     delivers message R+1 while R un-acked QoS 1/2 sit un-acked here, so at
//     most pendingLimit (== receive_maximum) QoS 1/2 entries are ever pending —
//     worst case receive_maximum × max_payload, exactly the memory model
//     config.go documents. Over the byte ceiling a QoS 1/2 publish still
//     buffers, best-effort reclaiming memory by evicting the oldest QoS 0 first.
//
// The ONLY path that refuses a QoS 1/2 publish (return false) is the COUNT cap
// being hit with NO evictable QoS 0 — UNREACHABLE under a spec-compliant broker,
// retained only as a hard safety valve against a broker that exceeds the Receive
// Maximum it was granted. The caller handles that protocol-violation case.
func (r *router) bufferLocked(pub *pahov5.Publish, ack func() error) bool {
	size := pubBytes(pub)
	overCount := len(r.pending) >= r.pendingLimit
	overBytes := r.pendingBytesLimit > 0 && r.pendingBytes+size > r.pendingBytesLimit

	if pub.QoS == 0 {
		if overCount || overBytes {
			// Best-effort drop: refusing a QoS 0 publish is always safe (no
			// redelivery contract, no ack to strand).
			return false
		}
		r.pending = append(r.pending, pendingPublish{pub: pub, ack: ack, epoch: r.connEpoch})
		r.pendingBytes += size
		r.signalPendingChangedLocked()
		return true
	}

	// QoS 1/2: never refuse for the byte ceiling — reclaim memory best-effort
	// by evicting the oldest QoS 0, then buffer regardless (memory is bounded
	// by the count cap == receive_maximum).
	if overBytes {
		r.evictOldestQoS0Locked()
	}
	// Enforce the count cap AFTER any byte-driven eviction freed a slot.
	if len(r.pending) >= r.pendingLimit && !r.evictOldestQoS0Locked() {
		// Count cap hit with no QoS 0 to reclaim: only reachable if the broker
		// exceeded its granted Receive Maximum (protocol violation).
		return false
	}
	r.pending = append(r.pending, pendingPublish{pub: pub, ack: ack, epoch: r.connEpoch})
	r.pendingBytes += size
	r.signalPendingChangedLocked()
	return true
}

func (r *router) signalPendingChangedLocked() {
	if r.pendingChanged != nil {
		close(r.pendingChanged)
	}
	r.pendingChanged = make(chan struct{})
}

// purgeStalePendingLocked drops every pending entry stamped with an epoch
// older than the current connEpoch — i.e. buffered under a PREVIOUS broker
// connection (A-1). Their protocol acks are dead (paho returns
// ErrPacketNotFound for a packet ID from a closed connection), so they are
// deliberately NOT invoked; a clean_start=false broker redelivers the QoS 1/2
// fresh on the new connection. QoS 0 stale entries are a best-effort loss (no
// redelivery across a disconnect, by protocol). Every purged entry is metered
// on MetricMQTTRouterStalePurged. Caller holds r.mu.
func (r *router) purgeStalePendingLocked() {
	if len(r.pending) == 0 {
		return
	}
	kept := r.pending[:0]
	var purged int64
	for i := range r.pending {
		if r.pending[i].epoch < r.connEpoch {
			r.pendingBytes -= pubBytes(r.pending[i].pub)
			r.releaseQueueReservationLocked(r.pending[i].pub)
			purged++
			continue
		}
		kept = append(kept, r.pending[i])
	}
	r.pending = kept
	if purged > 0 {
		r.stalePurged.Add(purged)
		r.metrics.Counter(MetricMQTTRouterStalePurged, purged, r.sessionTag()...)
	}
}

// evictOldestQoS0Locked removes the OLDEST QoS 0 entry from the pending buffer
// to reclaim a slot and bytes for a QoS 1/2 publish that must be buffered,
// counting the evicted QoS 0 as a best-effort drop (it carries no delivery
// contract). Returns true when an entry was evicted. Caller holds r.mu.
func (r *router) evictOldestQoS0Locked() bool {
	for i := range r.pending {
		if r.pending[i].pub.QoS == 0 {
			r.pendingBytes -= pubBytes(r.pending[i].pub)
			r.releaseQueueReservationLocked(r.pending[i].pub)
			r.pending = append(r.pending[:i], r.pending[i+1:]...)
			r.dropCount.Add(1)
			r.metrics.Counter(MetricMQTTRouterDropped, 1, r.sessionTag()...)
			return true
		}
	}
	return false
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
		r.addCallbacksLocked(1)
	}
	r.mu.Unlock()

	if doFlush {
		go func() {
			defer r.wg.Done()
			defer r.callbackDone()
			defer entry.inflight.Done()
			defer entry.emitMu.Unlock()
			r.mu.Lock()
			ready := flush
			if r.discarding {
				for _, pending := range flush {
					r.releaseQueueReservationLocked(pending.pub)
				}
				ready = nil // old socket ACKs die with the recycled generation
			} else if r.quiesced || len(r.managedCleanupFilters) > 0 {
				ready = make([]pendingPublish, 0, len(flush))
				blocked := make([]pendingPublish, 0, len(flush))
				for _, pending := range flush {
					if r.quiesced || matchesAnyFilter(r.managedCleanupFilters, pending.pub.Topic) {
						blocked = append(blocked, pending)
						r.pendingBytes += pubBytes(pending.pub)
					} else {
						ready = append(ready, pending)
					}
				}
				if len(blocked) > 0 {
					r.pending = append(blocked, r.pending...)
				}
			}
			r.mu.Unlock()
			for _, pending := range ready {
				r.releaseQueueReservation(pending.pub)
				r.emitOne(entry, pending.pub, pending.ack)
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

// emitOne invokes handler.fn with an immutable shallow Publish copy, recovering
// panics into the handler-panic metric. Payload/properties backing is shared and
// must remain read-only. It does NOT touch emitMu,
// inflight or the shared WaitGroup — the caller owns that bookkeeping.
// Used by the pending-flush path, where a single handler consumes the
// publish so its Properties pointer may be shared safely.
func (r *router) emitOne(handler routerHandler, pub *pahov5.Publish, ack func() error) {
	p := *pub
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
	if len(r.pending) == 0 || r.quiesced || r.discarding {
		return nil
	}
	var flush []pendingPublish
	keep := r.pending[:0]
	for _, p := range r.pending {
		if matchesAnyFilter(filters, p.pub.Topic) && (len(r.managedCleanupFilters) == 0 || !matchesAnyFilter(r.managedCleanupFilters, p.pub.Topic)) {
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

// pendingMatching reports how many buffered publishes match exact managed
// filters. It never settles or removes entries; migration uses it after a
// cleanup recycle to detect broker-pinned QoS1 deliveries and fail closed.
func (r *router) pendingMatching(filters []string) int {
	if r == nil || len(filters) == 0 {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, pending := range r.pending {
		if matchesAnyFilter(filters, pending.pub.Topic) {
			count++
		}
	}
	return count
}

// awaitManagedReplay waits through the current connection startup-grace window
// for a broker-pinned replay matching filters. The managed gate remains active,
// so any match stays buffered and unacknowledged. Routers without a live grace
// generation (unit fakes/direct dispatch) return their immediate snapshot.
func (r *router) awaitManagedReplay(ctx context.Context, filters []string) (bool, error) {
	if r == nil || len(filters) == 0 {
		return false, nil
	}
	r.mu.RLock()
	hook := r.awaitManagedReplayHook
	r.mu.RUnlock()
	if hook != nil {
		hook()
	}
	for {
		r.mu.RLock()
		for _, pending := range r.pending {
			if matchesAnyFilter(filters, pending.pub.Topic) {
				r.mu.RUnlock()
				return true, nil
			}
		}
		if !r.graceStarted {
			r.mu.RUnlock()
			return false, nil
		}
		deadline := r.graceDeadline
		changed := r.pendingChanged
		remaining := deadline.Sub(r.clk.Now())
		r.mu.RUnlock()
		if remaining <= 0 {
			return false, nil
		}
		timer := r.clk.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C():
			return false, nil
		}
	}
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
