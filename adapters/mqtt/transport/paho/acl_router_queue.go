package paho

import (
	"sync"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
)

// reserveQueueSlot claims one unit of the shared dispatch budget for pub.
//
// QoS 0 is refused immediately on a full budget: it carries no delivery
// contract, and parking Paho's single publish-callback goroutine for it would
// stop PINGRESP being read.
//
// QoS 1/2 must not be refused (at-least-once), but it must not park behind
// QoS 0 either. A pending QoS 0 holds a reservation while sitting OUTSIDE the
// broker's Receive-Maximum window, so a budget saturated by grace-buffered
// QoS 0 could only be released by handler progress that a not-yet-registered
// receiver cannot make — the callback parks, keepalive kills the connection,
// and because the callback never returns autopaho observes neither client
// shutdown nor a connection-down edge. So a QoS 1/2 first RECLAIMS the oldest
// reserved pending QoS 0 (a best-effort drop, always safe) and waits only when
// no reclaimable pending QoS 0 remains — everything holding the budget is then
// either being dispatched or inside the broker's in-flight window, so the
// broker's own Receive-Maximum flow control bounds the wait.
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
		if qos == 0 {
			r.mu.Unlock()
			return false
		}
		if r.evictOldestQoS0Locked(true) {
			r.mu.Unlock()
			continue
		}
		changed := r.queueChanged
		r.mu.Unlock()
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

// unsettledCount returns the number of QoS 1/2 packets received and not yet
// acknowledged in the current connection epoch.
func (r *router) unsettledCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.unsettled)
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

// bufferLocked appends the publish to the pre-registration pending buffer,
// which holds at most pendingLimit entries (== receive_maximum). The cap is
// enforced asymmetrically by QoS because dropping a QoS 1/2 publish is never
// safe:
//
//   - QoS 0 over the cap is refused (return false) — a best-effort drop, always
//     safe: there is no delivery contract and no ack to strand.
//
//   - QoS 1/2 over the cap evicts the oldest QoS 0 to make room. A QoS 1/2 drop
//     is never safe: ack+drop loses the message; un-ack+drop
//     head-of-line-blocks paho's CONTIGUOUS-PREFIX manual-ack stream
//     (acksTracker.flush sends the acknowledged prefix and stops at the first
//     un-acked entry), stranding acks for messages that WERE delivered and,
//     once receive_maximum un-acked slots accumulate, wedging ingress on a
//     stable connection. The broker's Receive-Maximum flow control never
//     delivers message R+1 while R QoS 1/2 sit un-acked here, so at most
//     pendingLimit QoS 1/2 entries are ever pending.
//
// A QoS 1/2 publish is refused (return false) only when the cap is hit with NO
// QoS 0 left to evict — reachable only when a broker exceeds the Receive
// Maximum it was granted. The caller handles that protocol-violation case.
func (r *router) bufferLocked(pub *pahov5.Publish, ack func() error) bool {
	if len(r.pending) >= r.pendingLimit {
		if pub.QoS == 0 {
			return false
		}
		if !r.evictOldestQoS0Locked(false) {
			return false
		}
	}
	r.pending = append(r.pending, pendingPublish{pub: pub, ack: ack, epoch: r.connEpoch})
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
// connection. Their protocol acks are dead (paho returns
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
// to reclaim a slot for a QoS 1/2 publish that must be buffered,
// counting the evicted QoS 0 as a best-effort drop (it carries no delivery
// contract). Returns true when an entry was evicted. Caller holds r.mu.
//
// reserved restricts the scan to entries that still hold a dispatch
// reservation: the budget gate needs the RESERVATION back, and an entry
// buffered through the legacy Route path holds none, so evicting it would drop
// a message without freeing what the caller is waiting for.
func (r *router) evictOldestQoS0Locked(reserved bool) bool {
	for i := range r.pending {
		if r.pending[i].pub.QoS != 0 {
			continue
		}
		if _, held := r.queueReservations[r.pending[i].pub]; reserved && !held {
			continue
		}
		// A retained COVERED entry is a live route's message held for a receiver
		// that registered late. Reclaiming it is still the right trade, but it
		// keeps the covered-drop attribution: that metric is the operator's
		// signal for slow receiver startup, and folding it into generic
		// backpressure would silence exactly that alert.
		covered := r.pending[i].retainCounted
		r.releaseQueueReservationLocked(r.pending[i].pub)
		r.pending = append(r.pending[:i], r.pending[i+1:]...)
		if covered {
			r.coveredDropped.Add(1)
			r.metrics.Counter(MetricMQTTRouterCoveredDropped, 1, r.sessionTag()...)
			return true
		}
		r.dropCount.Add(1)
		r.metrics.Counter(MetricMQTTRouterDropped, 1, r.sessionTag()...)
		return true
	}
	return false
}
