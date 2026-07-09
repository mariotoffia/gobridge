// Validates c7-durable-close: closing a DURABLE receiver performs a REAL
// teardown of the live link (via connection drop) instead of merely
// nil-ing the reference, so the broker stops delivering into an abandoned
// link — while the durable subscription (terminus) is preserved because a
// connection drop is a NON-closing detach.
package amqp10

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReceiver_CloseLink_Durable_ForcesConnectionTeardown proves the
// c7-durable-close fix. A durable receiver's Close must force a real
// connection teardown (the only way go-amqp can detach the live link
// WITHOUT sending a closing detach = UNSUBSCRIBE) so no message is
// delivered into an abandoned live link. It must NOT full-close the link
// (that would destroy the durable terminus).
//
// Mutation killed: revert closeLink's durable branch to nil-only (the
// pre-fix behaviour that just clears r.link/r.linkConn). Then the
// connection is never torn down, conn.closed stays false and the
// require.True below FAILs.
func TestReceiver_CloseLink_Durable_ForcesConnectionTeardown(t *testing.T) {
	sess := newTestSession()

	conn := &mockConn{}
	sess.mu.Lock()
	sess.conn = conn
	sess.connected = true
	sess.mu.Unlock()

	r, err := NewReceiver(ReceiverConfig{Address: "queue/durable", DurabilityMode: 1}, sess)
	require.NoError(t, err)

	// recordingLink counts Close calls so we can assert the durable link
	// is NOT full-closed (a closing detach would UNSUBSCRIBE on Artemis).
	link := &recordingLink{}
	r.mu.Lock()
	r.link = link
	r.linkConn = conn
	r.mu.Unlock()

	r.closeLink()

	// The live link must be REALLY detached: closeLink forces a connection
	// teardown (notifyDisconnect closes the conn synchronously) so the
	// broker cannot keep delivering credit into the abandoned link.
	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	require.True(t, closed,
		"durable Close must force a REAL connection teardown; a nil-only closeLink "+
			"leaves the link attached and the broker keeps delivering into it (c7-durable-close)")

	// The durable terminus must survive: go-amqp can only send a closing
	// detach, which brokers read as UNSUBSCRIBE, so the link itself must
	// NEVER be link-closed on a durable Close.
	link.mu.Lock()
	closeCalls := link.closeCalls
	link.mu.Unlock()
	require.Zero(t, closeCalls,
		"durable subscription link must NOT be full-closed (closing detach = UNSUBSCRIBE); "+
			"teardown must go through a connection drop that preserves the terminus")

	// notifyDisconnect ran: session connection state is cleared and the
	// link is marked down for health.
	sess.mu.Lock()
	connNow := sess.conn
	connected := sess.connected
	sess.mu.Unlock()
	require.Nil(t, connNow, "session connection must be cleared after durable-close teardown")
	require.False(t, connected, "session must report not-connected after durable-close teardown")
}

// TestReceiver_CloseLink_NonDurable_ClosesLinkNotConnection is the
// counterfactual: a NON-durable receiver detaches its own link (a closing
// detach is safe for a non-durable link) and must NOT tear down the shared
// connection. This guards against the durable teardown path being applied
// too broadly.
func TestReceiver_CloseLink_NonDurable_ClosesLinkNotConnection(t *testing.T) {
	sess := newTestSession()

	conn := &mockConn{}
	sess.mu.Lock()
	sess.conn = conn
	sess.connected = true
	sess.mu.Unlock()

	r, err := NewReceiver(ReceiverConfig{Address: "queue/plain", DurabilityMode: 0}, sess)
	require.NoError(t, err)

	link := &recordingLink{}
	r.mu.Lock()
	r.link = link
	r.linkConn = conn
	r.mu.Unlock()

	r.closeLink()

	link.mu.Lock()
	closeCalls := link.closeCalls
	link.mu.Unlock()
	require.Equal(t, 1, closeCalls, "non-durable Close must detach its own link exactly once")

	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	require.False(t, closed, "non-durable Close must NOT tear down the shared connection")

	sess.mu.Lock()
	connected := sess.connected
	sess.mu.Unlock()
	require.True(t, connected, "session must stay connected after a non-durable link detach")
}

// TestReceiver_DurableClose_BlastRadius_MarksSiblingsDownAndSignalsReconnect
// pins the review-item-2 contract with a DETERMINISTIC unit test: because a
// durable Close forces a full connection teardown (the only way the pinned
// go-amqp can detach a durable link WITHOUT emitting a closing detach that
// Artemis reads as UNSUBSCRIBE), it takes down EVERY sibling link
// multiplexed on the same shared connection. That collateral is exactly why
// durable receivers MUST be isolated on their own dedicated session (see
// doc.go / README.md "dedicated-session contract").
//
// It also proves recovery is BOUNDED, not a permanent loss: the teardown
// clears the connection AND signals a reconnect (reconnectCh), which the
// monitor loop consumes to re-dial so siblings relatch — exercised
// end-to-end against a live broker in
// TestIntegration_DurableClose_SiblingRecoveryBounded.
//
// Mutation killed: revert closeLink's durable branch to nil-only (the
// pre-fix behaviour). Then notifyDisconnect never runs — the shared
// connection stays open, the sibling links stay marked UP, and no reconnect
// is signalled — so the require assertions below FAIL.
func TestReceiver_DurableClose_BlastRadius_MarksSiblingsDownAndSignalsReconnect(t *testing.T) {
	sess := newTestSession()

	conn := &mockConn{}
	sess.mu.Lock()
	sess.conn = conn
	sess.connected = true
	sess.mu.Unlock()

	// The durable receiver whose Close forces the teardown.
	durable, err := NewReceiver(ReceiverConfig{Address: "topic/durable", DurabilityMode: 2}, sess)
	require.NoError(t, err)
	sess.registerReceiver(durable)
	sess.markReceiverLink(durable, true)
	durable.mu.Lock()
	durable.link = &recordingLink{}
	durable.linkConn = conn
	durable.mu.Unlock()

	// A sibling NON-durable receiver on the SAME session/connection.
	sibling, err := NewReceiver(ReceiverConfig{Address: "queue/sibling"}, sess)
	require.NoError(t, err)
	sess.registerReceiver(sibling)
	sess.markReceiverLink(sibling, true)

	// A sibling sender on the SAME session/connection.
	siblingSender, err := NewSender(SenderConfig{Address: "queue/egress", Session: sess}, sess)
	require.NoError(t, err)
	sess.registerSender(siblingSender)

	// Sanity: every sibling link is UP before the durable close.
	sess.mu.Lock()
	require.True(t, sess.receivers[sibling], "sibling receiver must start link-up")
	require.True(t, sess.senders[siblingSender], "sibling sender must start link-up")
	sess.mu.Unlock()

	durable.closeLink()

	// Blast radius: the shared connection is really torn down, so EVERY
	// sibling link on it is collateral-damaged (marked down -> Health
	// degrades for otherwise-healthy unrelated traffic).
	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	require.True(t, closed, "durable close must tear down the SHARED connection (blast radius)")

	sess.mu.Lock()
	siblingRxUp := sess.receivers[sibling]
	siblingTxUp := sess.senders[siblingSender]
	connCleared := sess.conn == nil
	sess.mu.Unlock()
	require.False(t, siblingRxUp,
		"sibling receiver link must be marked DOWN by the durable-close teardown (blast radius)")
	require.False(t, siblingTxUp,
		"sibling sender link must be marked DOWN by the durable-close teardown (blast radius)")
	require.True(t, connCleared, "session connection must be cleared so the monitor re-dials")

	// Recovery is BOUNDED, not permanent: a reconnect is signalled so the
	// monitor loop re-dials and the siblings relatch on the fresh
	// connection (no permanent loss).
	select {
	case <-sess.reconnectCh:
	default:
		t.Fatal("durable close must SIGNAL a reconnect so siblings relatch (bounded recovery); reconnectCh was empty")
	}
}
