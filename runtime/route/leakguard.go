package route

import (
	"errors"
	"fmt"
	"sync"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// --- terminal / wedge supervision signals (HIGH-2, HIGH-3, HIGH-4) ----------

// ErrRouteTerminal marks a PERMANENT route condition that the supervisor MUST
// escalate (RouteDead / pod restart) rather than restart in place: there is no
// in-process recovery reachable from the runner. Both the single-use-receiver
// re-entry guard (HIGH-2) and the hung sender/processor wedge (HIGH-3/HIGH-4)
// wrap it, so superviseRoute needs exactly ONE predicate —
// errors.Is(err, route.ErrRouteTerminal) — to decide escalation. This is the one
// coherent "rebuild-or-escalate" supervision decision the findings share; since
// AddRoute stores built receiver/sender/session INSTANCES (not factories/
// blueprints — runtime/bridge_routes.go), rebuild is not reachable here, so the
// lazy-correct choice for all three is escalate-to-terminal.
var ErrRouteTerminal = errors.New("route: terminal route condition; supervisor must escalate")

// ErrRouteReceiverClosed is returned when RouteRunner.Run is re-entered against a
// receiver a prior Run already closed (HIGH-2). RouteRunner.Run always closes its
// (single-use) receiver on exit; re-running the now-closed instance would flap at
// the backoff cap forever behind green liveness. Escalating instead gives an
// orchestrator an actionable signal.
var ErrRouteReceiverClosed = fmt.Errorf(
	"route: receiver already closed on a prior run; single-use transport cannot be rebuilt in-process: %w",
	ErrRouteTerminal)

// errSenderWedged and errProcessorWedged are the wedge causes wrapped by
// RouteRunner.wedge so the terminal error carries the reason.
var (
	errSenderWedged    = errors.New("route: bounded send ceiling fired; a sender goroutine leaked and the binding stopped accepting")
	errProcessorWedged = errors.New("route: too many processor timeouts leaked abandoned goroutines; route paused")
)

// wedgeBox lets an error be stored in an atomic.Value (which requires a single
// concrete type for all Store calls).
type wedgeBox struct{ err error }

// wedge latches a terminal route condition (a hung sender whose bounded goroutine
// leaked — HIGH-3; too many abandoned processor goroutines — HIGH-4) and returns
// the wrapped error. Once wedged, the Run receive callback refuses new deliveries
// and Run returns the wedge error, which superviseRoute escalates via
// ErrRouteTerminal. Idempotent: the FIRST cause wins so a burst of hung sends
// yields one coherent terminal reason, not a race of overwrites.
func (r *RouteRunner) wedge(cause error) error {
	r.wedgeOnce.Do(func() {
		werr := fmt.Errorf("%w: %w", ErrRouteTerminal, cause)
		r.wedgeErr.Store(wedgeBox{err: werr})
		r.wedged.Store(true)
		if r.logger != nil {
			r.logger.Error("route wedged; refusing new deliveries and escalating to supervisor",
				"route", r.routeID, "cause", cause)
		}
	})
	return r.wedgeError()
}

// isWedged reports whether the route has latched a terminal wedge.
func (r *RouteRunner) isWedged() bool { return r.wedged.Load() }

// wedgeError returns the latched wedge error, or a bare ErrRouteTerminal if the
// latch is set but the cause has not yet been stored (a transient window that
// cannot happen in practice because wedge stores the box before flipping the
// flag).
func (r *RouteRunner) wedgeError() error {
	if v, ok := r.wedgeErr.Load().(wedgeBox); ok {
		return v.err
	}
	return ErrRouteTerminal
}

// --- per-binding hung-send latch (HIGH-3) -----------------------------------

// sendHung reports whether binding already has a parked (leaked) send goroutine
// from a prior timed-out send. A second CONSECUTIVE send to the same binding is
// refused before spawning so at most ONE leaked goroutine exists per binding
// (Go cannot forcibly kill the parked goroutine, so the cap + stop-accepting is
// the only available bound).
func (r *RouteRunner) sendHung(binding string) bool {
	r.hungMu.Lock()
	defer r.hungMu.Unlock()
	return r.hungBind[binding]
}

// markSendHung latches binding as having a parked send goroutine.
func (r *RouteRunner) markSendHung(binding string) {
	r.hungMu.Lock()
	if r.hungBind == nil {
		r.hungBind = make(map[string]bool)
	}
	r.hungBind[binding] = true
	r.hungMu.Unlock()
}

// --- route-level abandoned-processor circuit breaker (HIGH-4) ---------------

// maxAbandonedProcessors is the number of processor-timeout abandons WITHOUT an
// intervening terminal settle that trips the route circuit breaker. It is set
// well above the default MaxReplayAttempts (5) so a SINGLE timeout-poison message
// — which abandons one goroutine per retry then reaches the cap and is DLQ'd
// (terminal ack resets the counter) — never trips it; only sustained
// amplification with NO terminal resolution does. That is exactly the residual
// HIGH-4 hazard when HIGH-1's ledger is defeated (an adapter minting an unstable
// per-message identity so the cap never fires) or when MaxReplayAttempts is
// configured to 0 (cap disabled): abandons then accumulate without a settle and
// the breaker paves the route to a supervised restart that clears the leaked
// goroutines.
//
// N4 — honest guarantee (this breaker is a SECONDARY defense, not the primary
// one). Because resetAbandonedProcessors fires on EVERY terminal settle, the
// counter measures abandons in a run of deliveries that all abandoned; interleaved
// UNRELATED successful traffic resets it. So the breaker reliably bounds only:
// (a) a single poison message's per-retry abandons, and (b) a back-to-back run of
// abandoning deliveries with no successful settle between them. It does NOT bound
// abandoned goroutines that accumulate WHILE unrelated messages keep succeeding
// (each success zeroes the counter) — in that interleaved case the leaked
// goroutines are still memory-safe (each writes only its own per-frame clone,
// never merged) and the PRIMARY bound is HIGH-1's replay ledger, which caps the
// count-less poison stream that produces the abandons in the first place; the pod
// restart on a genuine breaker trip is the last-resort backstop. Upgrade path to a
// breaker immune to interleaved success: track OUTSTANDING abandoned goroutines
// (increment on abandon, decrement when the abandoned goroutine finally returns —
// a chain hook that waits on the processor's done channel) and trip on the
// concurrent count rather than the consecutive-since-settle count. That is a
// larger change to the chain's abandon lifecycle (a new goroutine per abandon and
// a paired inc/dec) and is deferred as the documented follow-up.
const maxAbandonedProcessors = 64

// onProcessorAbandoned is the WithChainOnProcessorTimeout callback. It counts a
// genuine processor-timeout abandon and wedges the route once the count of
// abandons-since-the-last-terminal-settle crosses the ceiling. See
// maxAbandonedProcessors for the honest scope of the guarantee (N4).
func (r *RouteRunner) onProcessorAbandoned() {
	if r.abandonedProc.Add(1) >= int64(maxAbandonedProcessors) {
		_ = r.wedge(fmt.Errorf("%w (%d abandons without a terminal settle)", errProcessorWedged, maxAbandonedProcessors))
	}
}

// resetAbandonedProcessors clears the consecutive-abandon counter on a terminal
// settle (ackDelivery), so the breaker measures abandons WITHOUT an intervening
// terminal resolution rather than a route's whole-lifetime total.
func (r *RouteRunner) resetAbandonedProcessors() {
	r.abandonedProc.Store(0)
}

// --- bridge-owned replay ledger for count-less sources (HIGH-1) -------------

// replayLedgerMaxKeys caps the bridge-owned retry ledger's in-memory footprint.
// The ledger only ever holds keys for COUNT-LESS-source messages that have
// FAILED at least once (a message that succeeds on first delivery is never
// recorded), so under healthy load it stays near-empty. The cap is a hard
// ceiling for a pathological stream of unique-identity poison messages: at the
// ceiling the oldest key is evicted (FIFO), and an evicted message simply gets
// up to MaxReplayAttempts more tries before being re-capped — bounded, not
// unbounded.
//
// ponytail: the named ceiling is 100k CONCURRENTLY-LIVE unique-identity FAILING
// count-less messages. Below it the ledger is exact; at or above it FIFO eviction
// can drop a still-retrying counter and that message restarts at attempt 0 (N2) —
// so the worst case degrades to "extra MaxReplayAttempts tries per evicted
// message", never to "infinite retry". Upgrade path when a single instance's cap
// is too small, when the 100k-concurrent-failing regime is realistic, or when the
// count must survive restarts / span instances: back the ledger with the existing
// durable dedup/outbox store instead of this in-memory FIFO map (a ports change
// this workstream does not own — reported as cross-cutting), which makes the cap
// fail-closed-as-capped rather than fail-open-as-evicted.
const replayLedgerMaxKeys = 100_000

// replayLedger is a bounded, FIFO-evicting attempt counter keyed by a stable
// per-message identity. It gives count-less sources (MQTT, AMQP 0-9-1, HTTP —
// anything without a native receive-count header) a bridge-owned retry count so
// the route policy's MaxReplayAttempts cap actually applies instead of letting a
// deterministically-failing message loop forever (HIGH-1, HIGH-4).
type replayLedger struct {
	mu       sync.Mutex
	max      int
	attempts map[string]int
	order    []string // insertion order of live keys, for FIFO eviction
}

func newReplayLedger(max int) *replayLedger {
	if max <= 0 {
		max = replayLedgerMaxKeys
	}
	return &replayLedger{max: max, attempts: make(map[string]int)}
}

// record increments and returns the attempt count for key, evicting the oldest
// key first when the ledger is full so memory stays bounded.
func (l *replayLedger) record(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.attempts[key]; !ok {
		l.evictIfFullLocked()
		if len(l.order) >= 2*l.max {
			l.compactLocked()
		}
		l.order = append(l.order, key)
	}
	l.attempts[key]++
	return l.attempts[key]
}

// attemptsFor returns the recorded attempt count for key (0 if unseen).
func (l *replayLedger) attemptsFor(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attempts[key]
}

// remove drops key on a terminal outcome (poison/drop/success) so the ledger
// does not retain a settled message. The FIFO cap already bounds memory; this
// keeps it tight for the common resolve-after-a-few-retries case.
func (l *replayLedger) remove(key string) {
	l.mu.Lock()
	delete(l.attempts, key)
	l.mu.Unlock()
}

// size reports the number of live keys (test/observability helper).
func (l *replayLedger) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.attempts)
}

func (l *replayLedger) evictIfFullLocked() {
	for len(l.attempts) >= l.max && len(l.order) > 0 {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.attempts, oldest)
	}
}

// compactLocked rebuilds order to only live keys, bounding the slack the order
// slice accumulates from remove() (which cannot cheaply delete from the slice).
func (l *replayLedger) compactLocked() {
	live := make([]string, 0, len(l.attempts))
	seen := make(map[string]struct{}, len(l.attempts))
	for _, k := range l.order {
		if _, ok := l.attempts[k]; !ok {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		live = append(live, k)
	}
	l.order = live
}

// replayKey derives a STABLE per-message identity for the replay ledger,
// preferring the bridge-to-bridge dedup id, then the idempotency key, then the
// envelope id. The prefix keeps the three key spaces disjoint so a dedup id can
// never alias an idempotency key or an envelope id.
//
// Effectiveness caveat (cross-cutting): the ledger caps redelivery only as well
// as the chosen key is STABLE across a source transport's redeliveries. A source
// adapter that mints a fresh envelope id (and stamps no dedup/idempotency header)
// on every redelivery defeats it — the runtime cannot fix that here; it is
// reported as an adapter-side dependency.
func replayKey(env *messaging.Envelope) string {
	if env == nil {
		return ""
	}
	h := env.Headers()
	if v, ok := messaging.GetHeaderString(h, messaging.HeaderDeduplicationID); ok && v != "" {
		return "dedup:" + v
	}
	if v, ok := messaging.GetHeaderString(h, messaging.HeaderIdempotencyKey); ok && v != "" {
		return "idem:" + v
	}
	return "id:" + env.ID()
}

// effectiveAttempt returns the delivery-attempt count used by the replay-cap
// gate: the source transport's NATIVE receive count when present, else the
// bridge-owned ledger count for count-less sources. It is deliberately 0-BASED
// for count-less sources — a FIRST delivery reads 0, exactly as the historical
// receiveCount(env)==0 did — so a genuine first-delivery transient error still
// retries under a MaxReplayAttempts=1 policy instead of poisoning before a single
// real attempt (the F3 forged-count contract; chunk10_test.go pins that a
// count-less MQTT source reads 0 on first delivery so it "still gets its
// retries"). The ledger then climbs one per redelivery (recordReplayAttempt in
// retryOrFallback), so the cap fires after MaxReplayAttempts redeliveries —
// bounded, where a count-less source previously looped forever.
//
// N1 — deliberate off-by-one vs native counts: a NATIVE receive count is 1-based
// (the source stamps 1 on first receive), so a count-bearing source is DLQ'd
// after exactly MaxReplayAttempts deliveries; a count-less source is 0-based here
// and is DLQ'd after MaxReplayAttempts+1 deliveries (one extra try). The +1 is
// KEPT on purpose: making it 1-based (attemptsFor(key)+1 >= MaxReplayAttempts)
// would poison a first-delivery transient with ZERO real retries under a
// MaxReplayAttempts=1 policy, breaking F3's "retry at least once". One extra
// delivery for count-less sources is the accepted cost of preserving that
// first-attempt guarantee.
//
// It is a pure read; the ledger is incremented separately so the gate and the
// increment stay decoupled.
func (r *RouteRunner) effectiveAttempt(env *messaging.Envelope) int {
	if rc := receiveCount(env); rc > 0 {
		return rc
	}
	if r.replay == nil {
		return 0 // ledger disabled -> preserve the historical fail-open behaviour
	}
	return r.replay.attemptsFor(replayKey(env))
}

// replayCapReached is the SINGLE gate every transient-failure poison decision
// routes through (HIGH-1). It returns the effective attempt count and whether the
// route policy's MaxReplayAttempts cap has been reached, so a count-less source
// is capped by the bridge-owned ledger exactly as a count-bearing source is
// capped by its native header.
func (r *RouteRunner) replayCapReached(env *messaging.Envelope) (int, bool) {
	rc := r.effectiveAttempt(env)
	return rc, r.policy.MaxReplayAttempts > 0 && rc >= r.policy.MaxReplayAttempts
}

// recordReplayAttempt records one more attempt for a COUNT-LESS source so the cap
// climbs on each redelivery. A count-bearing source self-caps via its native
// header and is never recorded, keeping the ledger empty for the common case.
func (r *RouteRunner) recordReplayAttempt(env *messaging.Envelope) {
	if r.replay == nil {
		return
	}
	if receiveCount(env) > 0 {
		return
	}
	r.replay.record(replayKey(env))
}

// forgetReplayAttempts evicts a message from the ledger on a terminal outcome
// (called from the single ack convergence point, ackDelivery). A retried
// delivery uses del.Retry (not ackDelivery) and is therefore NOT forgotten.
func (r *RouteRunner) forgetReplayAttempts(env *messaging.Envelope) {
	if r.replay == nil || env == nil {
		return
	}
	if receiveCount(env) > 0 {
		return
	}
	r.replay.remove(replayKey(env))
}
