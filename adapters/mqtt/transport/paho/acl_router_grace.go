package paho

import (
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/logging"
)

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
	if r.generationOpenedByClient {
		// The replacement connection's first packet already opened this
		// generation (it reached the callback before autopaho got here).
		// Advancing again would purge the entries that packet just buffered and
		// erase its unsettled bookkeeping — the loss this ordering prevents.
		r.generationOpenedByClient = false
	} else {
		r.advanceGenerationLocked()
	}
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
// orphans and firing retention metrics ahead of the configured grace.
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
// redelivers on reconnect; ack-dropping it would be acknowledged
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
				"acking, dropping, and attempting to unsubscribe its exact topic",
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
// (settleUnmatched → retainCovered), because ack-dropping it would
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
// at-least-once. A covered publish is RETAINED un-acked in the pending
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
// redelivers it on reconnect. It first re-scans handlers under r.mu to
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
	// contract). Counted on the covered-drop metric for visibility. The
	// reservation MUST be returned here: this branch is reachable on every
	// byte-ceiling refusal, so holding it would retire one unit of the shared
	// dispatch budget per drop until nothing could be admitted at all.
	r.releaseQueueReservation(pub)
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
// past the grace window: it always bumps the counter and metric, and
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

// dropQoS0 drops a QoS 0 publish no admission path could hold. QoS 0 carries no
// delivery contract and no ack, so the drop is best-effort and counted only on
// the generic drop metric. reason names WHICH bound refused it, because the
// operator remedy differs: a full pending buffer means a receiver has not
// registered (or has stalled), while an exhausted dispatch budget means
// receive_maximum is too small for the offered load. Caller must hold NEITHER
// r.mu.
func (r *router) dropQoS0(pub *pahov5.Publish, reason string) {
	r.releaseQueueReservation(pub)
	r.dropCount.Add(1)
	r.metrics.Counter(MetricMQTTRouterDropped, 1, r.sessionTag()...)
	if r.logger != nil {
		r.logger.Warn("mqtt: dropped QoS 0 publish ("+reason+")",
			"topic", pub.Topic,
			"qos", pub.QoS,
		)
	}
}

// Reasons a QoS 0 publish is shed, as reported by dropQoS0.
const (
	dropReasonPendingFull     = "pending buffer full during startup grace"
	dropReasonBudgetExhausted = "dispatch budget exhausted"
	dropReasonSessionClosing  = "session is closing"
)
