// Validates that the Receiver's link-error handling correctly identifies
// WHICH connection a failed link belonged to and does not destroy a
// freshly-reconnected connection when a stale link error surfaces.
//
// Same root cause as sender_disconnect_test.go: notifyDisconnect already
// guards against stale connection pointers, but the receiver must pass
// the link's connection (captured at link creation time) — not the
// session's CURRENT connection — for that guard to work.
package amqp10

import (
	"errors"
	"testing"
	"time"
)

// TestReceiver_StaleLinkError_DoesNotKillReconnectedConn validates that
// when a Receive fails on a link that belonged to an old connection, the
// receiver does NOT trigger a disconnect on the session's NEW connection
// (post-reconnect).
//
// Scenario:
// ───────────────────────────────────────────────
//
//	t0: receiver link created on conn1 (r.linkConn = conn1)
//	t1: conn1 dies; session reconnects to conn2 (independent of receiver)
//	t2: link.Receive on the old link returns "broken" error
//	t3: handleLinkError must NOT close conn2
//
// ───────────────────────────────────────────────
func TestReceiver_StaleLinkError_DoesNotKillReconnectedConn(t *testing.T) {
	sess := newTestSession()

	conn1 := &mockConn{}
	conn2 := &mockConn{}

	r, err := NewReceiver(ReceiverConfig{Address: "queue/x"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	// Pretend the receiver had a link on conn1; it has since been nil-ed
	// out (or never set since the *amqp.Receiver concrete type cannot be
	// faked here, but linkConn tracking is what handleLinkError uses).
	r.mu.Lock()
	r.link = nil
	r.linkConn = conn1
	r.mu.Unlock()

	// Simulate: session has already reconnected to conn2.
	sess.mu.Lock()
	sess.conn = conn2
	sess.connected = true
	sess.mu.Unlock()

	// Trigger the failure path with an Unavailable-mapped error.
	r.handleLinkError(errors.New("amqp:link:detach-forced"))

	// OTHER: settle delay for notifyDisconnect async effects.
	time.Sleep(50 * time.Millisecond)

	conn2.mu.Lock()
	conn2Closed := conn2.closed
	conn2.mu.Unlock()
	if conn2Closed {
		t.Fatal("new connection (conn2) was closed because handleLinkError " +
			"used the session's CURRENT connection instead of the failed link's connection")
	}

	sess.mu.Lock()
	stillConn2 := sess.conn == conn2 && sess.connected
	sess.mu.Unlock()
	if !stillConn2 {
		t.Fatal("session's conn2 state was torn down by stale receiver link failure handling")
	}
}

// TestReceiver_LinkError_TriggersDisconnectWhenLinkConnStillCurrent
// validates the happy path of the conn-tracking fix for receivers: when
// the link's conn IS still the session's current conn, notifyDisconnect
// fires and the session monitor reconnects.
func TestReceiver_LinkError_TriggersDisconnectWhenLinkConnStillCurrent(t *testing.T) {
	sess := newTestSession()
	conn1 := &mockConn{}

	sess.mu.Lock()
	sess.conn = conn1
	sess.connected = true
	sess.mu.Unlock()

	r, err := NewReceiver(ReceiverConfig{Address: "queue/x"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	r.mu.Lock()
	r.link = nil
	r.linkConn = conn1
	r.mu.Unlock()

	r.handleLinkError(errors.New("amqp:link:detach-forced"))

	deadline := time.After(2 * time.Second)
	for {
		conn1.mu.Lock()
		closed := conn1.closed
		conn1.mu.Unlock()
		if closed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("conn1 was not closed after link failure on its own conn")
		case <-time.After(5 * time.Millisecond):
		}
	}

	sess.mu.Lock()
	connNow := sess.conn
	connected := sess.connected
	sess.mu.Unlock()
	if connNow != nil {
		t.Fatalf("session conn should be cleared, got %v", connNow)
	}
	if connected {
		t.Fatal("session connected flag should be false after notifyDisconnect")
	}
}
