// Validates that the Sender's failure-handling path correctly identifies
// WHICH connection a failed link belonged to and does not destroy a
// freshly-reconnected connection when a stale link error surfaces.
//
// notifyDisconnect already guards against stale connection pointers, but
// the sender must pass the link's connection (captured at link creation
// time) — not the session's CURRENT connection — for that guard to work.
package amqp10

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestSender_StaleLinkFailure_DoesNotKillReconnectedConn validates that
// when a Send fails on a link that belonged to an old connection, the
// sender does NOT trigger a disconnect on the session's NEW connection
// (post-reconnect).
//
// Scenario:
// ───────────────────────────────────────────────
//
//	t0: link L1 created on conn1 (s.linkConn = conn1)
//	t1: conn1 dies; session reconnects to conn2 (independent of sender)
//	t2: Sender.Send on L1 returns "broken" error (still references conn1)
//	t3: handleSendFailure must NOT close conn2
//
// ───────────────────────────────────────────────
//
// Without the fix, handleSendFailure looks up s.session.Conn() = conn2
// and passes it to notifyDisconnect, which then matches and destroys
// the brand-new working connection.
func TestSender_StaleLinkFailure_DoesNotKillReconnectedConn(t *testing.T) {
	sess := newTestSession()

	conn1 := &mockConn{}
	conn2 := &mockConn{}

	s, err := NewSender(SenderConfig{Address: "queue/x", Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	// Inject a failing mock link that simulates a link belonging to conn1
	// returning an error AFTER the session has moved on to conn2.
	failing := newMockSenderLink()
	failing.sendErr = errors.New("amqp:link:detach-forced")
	close(failing.sendBlock)
	failing.sendBlock = nil

	s.mu.Lock()
	s.link = failing
	s.linkConn = conn1
	s.mu.Unlock()

	// Simulate: session has already reconnected to conn2.
	sess.mu.Lock()
	sess.conn = conn2
	sess.connected = true
	sess.mu.Unlock()

	if err := s.Send(context.Background(), &messaging.Envelope{ID: "x"}); err == nil {
		t.Fatal("expected Send to return the link error")
	}

	deadline := time.After(time.Second)
	for !failing.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("failing link was never detached/closed")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// OTHER: settle delay for async closeLinkAsync goroutine + session bookkeeping.
	time.Sleep(50 * time.Millisecond)

	conn2.mu.Lock()
	conn2Closed := conn2.closed
	conn2.mu.Unlock()
	if conn2Closed {
		t.Fatal("new connection (conn2) was closed because handleSendFailure " +
			"used the session's CURRENT connection instead of the failed link's connection")
	}

	sess.mu.Lock()
	stillConn2 := sess.conn == conn2 && sess.connected
	sess.mu.Unlock()
	if !stillConn2 {
		t.Fatal("session's conn2 state was torn down by stale link failure handling")
	}
}

// TestSender_LinkFailure_TriggersDisconnectWhenLinkConnStillCurrent
// validates the happy path of the conn-tracking fix: when the link's
// conn IS still the session's current conn (no intervening reconnect),
// notifyDisconnect fires and tears the conn down so the session monitor
// reconnects.
func TestSender_LinkFailure_TriggersDisconnectWhenLinkConnStillCurrent(t *testing.T) {
	sess := newTestSession()
	conn1 := &mockConn{}

	sess.mu.Lock()
	sess.conn = conn1
	sess.connected = true
	sess.mu.Unlock()

	s, err := NewSender(SenderConfig{Address: "queue/x", Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	failing := newMockSenderLink()
	failing.sendErr = errors.New("amqp:link:detach-forced")
	close(failing.sendBlock)
	failing.sendBlock = nil

	s.mu.Lock()
	s.link = failing
	s.linkConn = conn1
	s.mu.Unlock()

	if err := s.Send(context.Background(), &messaging.Envelope{ID: "x"}); err == nil {
		t.Fatal("expected Send to return the link error")
	}

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
			t.Fatal("conn1 was not closed by notifyDisconnect after link failure on its own conn")
		case <-time.After(5 * time.Millisecond):
		}
	}

	sess.mu.Lock()
	connNow := sess.conn
	connected := sess.connected
	sess.mu.Unlock()
	if connNow != nil {
		t.Fatalf("session conn should be cleared after notifyDisconnect, got %v", connNow)
	}
	if connected {
		t.Fatal("session connected flag should be false after notifyDisconnect")
	}
}
