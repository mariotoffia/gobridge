package paho

import (
	"errors"

	pahov5 "github.com/eclipse/paho.golang/paho"
)

// The router's connection generation identifies which broker connection a
// buffered publish, an un-acked packet, or a queued dispatch belongs to. It is
// the router's answer to "is this publish's acknowledgement still live?", and
// getting it wrong in either direction loses messages: purging a live publish
// strands its acknowledgement at the head of Paho's contiguous-prefix ack
// stream, while keeping a dead one lets a redelivered copy accumulate beside
// its ghost until the count cap ack-drops a live message as bogus overflow.
//
// Two events can be the first to observe a new connection, and they are NOT
// ordered against each other: Paho starts delivering publishes from inside
// Client.Connect, while autopaho calls OnConnectionUp only after
// establishServerConnection returns. So a queued backlog can reach the router
// before the callback that announces the connection carrying it.
//
// A client pointer alone cannot decide the question. Two Paho clients can be
// alive at once — a ConnectionManager whose Disconnect timed out keeps its
// client running while the replacement connects — so "a packet from a client I
// have not seen" is ambiguous between a replacement's backlog and a straggler
// from a superseded socket. The generation therefore advances only when the
// SESSION has reported the previous connection torn down (noteConnectionTornDown,
// wired to autopaho's connection-down edge and to the return of every explicit
// Disconnect). That report is the happens-after evidence; the client pointer
// then says which of the two events got there first.

// noteConnectionTornDown reports that the connection the router was serving is
// gone: autopaho's connection-down edge (raised only after the client's
// workers — including the goroutine running our publish callback — returned),
// or the return of an explicit ConnectionManager.Disconnect.
//
// It arms exactly one generation advance. Until it is called, a packet from an
// unfamiliar client is a straggler from a socket that is being torn down or has
// already been superseded, and must not open a generation: doing so would lift
// a recycle's discard window with the old socket still live, or thrash the
// generation between two overlapping clients and purge live traffic.
//
// It also voids an unclaimed connection-up marker, so a connection that
// delivered nothing cannot leave the NEXT connection's first packet consuming a
// marker that was never about it. Caller must not hold r.mu.
func (r *router) noteConnectionTornDown() {
	r.mu.Lock()
	r.replacementPending = true
	r.generationOpenedByClient = false
	r.mu.Unlock()
}

// advanceGenerationLocked closes the previous broker connection generation and
// opens the next one: entries buffered under the old generation are purged
// (their protocol acks died with their connection and a clean_start=false
// broker redelivers the QoS 1/2 fresh, so keeping them would let a redelivered
// copy accumulate beside its ghost until the count cap ack-drops a LIVE message
// as a bogus overflow), un-acked packet bookkeeping from the old connection is
// dropped, and recycle-window discarding ends because the socket it applied to
// is gone. Caller holds r.mu.
func (r *router) advanceGenerationLocked() {
	r.connEpoch++
	r.replacementPending = false
	r.clearUnsettledLocked()
	r.discarding = false
	if r.graceStarted {
		r.purgeStalePendingLocked()
	}
}

// noteLiveClient records the Paho client that delivered a packet, and opens the
// generation when that packet is the first evidence of the replacement
// connection the session has already reported it is waiting for. Opening it
// here is what keeps a CONNACK backlog that beats autopaho's connection-up
// callback dispatchable, its acknowledgement live, and its unsettled
// bookkeeping intact. Caller must not hold r.mu.
func (r *router) noteLiveClient(client *pahov5.Client) {
	if client == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.liveClient == client {
		return
	}
	if !r.replacementPending {
		// No teardown has been reported since this generation opened, so this
		// packet carries no evidence that the current generation ended.
		if r.liveClient == nil {
			// It is the first packet of the connection the connection-up
			// callback already opened: adopt it as this generation's client so
			// its own settlements are not read as superseded.
			r.liveClient = client
			return
		}
		// A straggler from an overlapping or superseded socket. It must NOT
		// displace the live client: that would make the live connection's
		// settlements look like they landed on a dead socket.
		return
	}
	r.liveClient = client
	r.advanceGenerationLocked()
	r.generationOpenedByClient = true
}

// liveConnectionSuperseded reports whether the connection that delivered client's
// packet is gone — the settlement-time question behind
// MetricMQTTAckAfterReconnect. It is deliberately NOT the connection epoch: the
// epoch also advances for a recycle on a still-live socket, which would report
// every settlement in a routine drain as a guaranteed broker redelivery and
// swallow a genuine acknowledgement failure on a connection that never cycled.
// Caller must not hold r.mu.
func (r *router) liveConnectionSuperseded(client *pahov5.Client) bool {
	if client == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	// replacementPending means the session reported this connection torn down
	// and no replacement has delivered yet, so client is dead even though it is
	// still the last one seen.
	return r.liveClient != client || r.replacementPending
}

// ackWithReconnectMapping wraps a protocol-ack callback with the
// connection-cycled mapping: when the broker connection was torn down and
// re-established between receive and settle, the client's ack tracker was
// reset, the broker WILL redeliver, and downstream dedup absorbs the
// duplicate — so the settlement reports SUCCESS. Each mapped success is a
// GUARANTEED broker redelivery and is counted on MetricMQTTAckAfterReconnect: a
// burst after a reconnect storm is the leading indicator of a duplicate flood
// on routes without downstream dedup.
//
// The cycle is detected from the CLIENT that received the packet, not from the
// SDK's error class. Paho's acknowledgement tracker marks an ack and flushes
// the acknowledged prefix asynchronously, so an ack marked just before the
// connection dropped returns NIL and is still redelivered — measuring by error
// alone silently misses exactly the settlements that produce duplicates. The
// connection EPOCH is equally wrong in the other direction: it also advances
// for a recycle on a still-live socket, which would report every settlement in
// a routine drain as a guaranteed redelivery. SDK errors stay reserved for
// classifying the operation: ErrPacketNotFound is still read as a cycle (it can
// only mean the tracker was reset), and every other error is classified via
// MapError and remains a settlement failure.
func (r *router) ackWithReconnectMapping(client *pahov5.Client, ack func() error) func() error {
	return func() error {
		err := ack()
		cycled := r.liveConnectionSuperseded(client)
		if err != nil && !errors.Is(err, pahov5.ErrPacketNotFound) {
			if !cycled {
				return MapError(err)
			}
			// The connection is gone: this ack could not have reached the broker
			// whatever the SDK reported. Redelivery covers it.
		} else if err == nil && !cycled {
			return nil
		}
		r.ackAfterReconnect.Add(1)
		r.metrics.Counter(MetricMQTTAckAfterReconnect, 1, r.sessionTag()...)
		return nil
	}
}
