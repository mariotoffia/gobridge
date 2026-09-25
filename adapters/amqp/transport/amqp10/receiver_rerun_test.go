// Pins the ports.Receiver rule that Close ends one Run, not the receiver:
// the route runner calls Close when a Run ends and, on a supervised
// restart, calls Run again on the SAME receiver, which must attach a fresh
// link instead of failing or reusing the detached one.
package amqp10

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Azure/go-amqp"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestReceiver_RunAfterCloseAttachesFreshLink runs a receiver, closes it
// and runs it again. Run #1 consumes from link A. Close detaches A: a
// non-durable receiver closes the link, a durable one drops the connection
// so the subscription survives. Run #2 must go back through the attach
// path and emit a delivery from a new link B.
//
// go-amqp's *amqp.Session has no seam, so createLink cannot succeed in a
// unit test. Run #2 fails its attach against the sessionless test Session
// and parks in its link re-creation backoff; the test then completes the
// attach the way createLink does (r.link, r.linkConn) and fires the backoff
// on the fake clock so the loop consumes from B.
//
// Mutation check: latch a closed flag in Close and return it from Run —
// Run #2 then exits before it re-attaches and the test fails.
func TestReceiver_RunAfterCloseAttachesFreshLink(t *testing.T) {
	cases := []struct {
		name           string
		durabilityMode uint32
	}{
		{name: "non_durable_closes_link", durabilityMode: 0},
		{name: "durable_drops_connection", durabilityMode: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			durable := tc.durabilityMode > 0
			sess := newTestSession()
			connA := &mockConn{}
			installConn(sess, connA)

			fake := clocktest.NewAt(time.Unix(1_700_000_000, 0))
			r, err := NewReceiver(ReceiverConfig{
				Address:        "queue/rerun",
				DurabilityMode: tc.durabilityMode,
				Clock:          fake,
			}, sess)
			require.NoError(t, err)

			linkA := &fakeLink{deliveries: []*Delivery{rerunDelivery("a")}}
			r.mu.Lock()
			r.link = linkA
			r.linkConn = connA
			r.mu.Unlock()

			runFirst(t, r, "a")
			require.NoError(t, r.Close(context.Background()))

			if durable {
				require.Zero(t, linkA.closeCalls,
					"durable Close must not link-close A: a closing detach unsubscribes")
				require.True(t, connClosed(connA), "durable Close must detach A by dropping its connection")
			} else {
				require.Equal(t, 1, linkA.closeCalls, "Close must detach link A")
			}
			r.mu.Lock()
			held := r.link
			r.mu.Unlock()
			require.Nil(t, held, "Close must drop link A so the next Run attaches a fresh link")

			got, done, cancel := startRun(t, r)
			connB := connA
			if durable {
				// Run #2 waits for the session the durable Close dropped;
				// reconnect it the way the monitor loop does.
				awaitWhileRunning(t, done, "Run #2 to wait for the session", func() bool {
					return sess.subscriberCount() == 1
				})
				connB = &mockConn{}
				installConn(sess, connB)
				sess.pushEvent(ports.SessionConnected, nil)
			}

			linkB := &fakeLink{deliveries: []*Delivery{rerunDelivery("b")}}
			awaitWhileRunning(t, done, "Run #2 to back off after its attach attempt", func() bool {
				return fake.TimerCount() >= 1
			})
			r.mu.Lock()
			r.link = linkB
			r.linkConn = connB
			r.mu.Unlock()
			fake.Advance(2 * time.Second) // first link backoff: 1s ± 25%

			del := wait.RequireReceive(t, got, 2*time.Second)
			require.Equal(t, "b", del.Envelope().ID(), "Run #2 must emit from the fresh link B")
			cancel()
			require.ErrorIs(t, wait.RequireReceive(t, done, 2*time.Second), context.Canceled,
				"Run #2 must end on its own ctx, not on the prior Close")

			require.NoError(t, del.Ack(context.Background()))
			require.NoError(t, r.Close(context.Background()))
			if durable {
				require.Zero(t, linkB.closeCalls, "durable Close must not link-close B")
				require.True(t, connClosed(connB), "durable Close must detach B by dropping its connection")
			} else {
				require.Equal(t, 1, linkB.closeCalls, "Close after Run #2 must detach link B")
				require.Equal(t, 1, linkA.closeCalls, "link A must not be reused or closed again")
			}
		})
	}
}

// installConn marks sess connected on conn with no AMQP session, so every
// real attach fails with a recoverable "session not connected".
func installConn(sess *Session, conn *mockConn) {
	sess.mu.Lock()
	sess.conn = conn
	sess.connected = true
	sess.mu.Unlock()
}

func connClosed(conn *mockConn) bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.closed
}

func rerunDelivery(id string) *Delivery {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id})
	return NewDelivery(env, &amqp.Message{}, newMockSettler(), slog.Default(), nil, nil)
}

// runFirst drives Run #1 until it emits one delivery, stops it with a ctx
// cancel and settles the delivery, as the route runner does before Close.
func runFirst(t *testing.T, r *Receiver, wantID string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got ports.Delivery
	err := r.Run(ctx, func(_ context.Context, d ports.Delivery) error {
		got = d
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, got, "Run #1 must emit from link A")
	require.Equal(t, wantID, got.Envelope().ID())
	require.NoError(t, got.Ack(context.Background()))
}

// startRun starts Run #2 in the background and returns its emitted
// deliveries, its result and the cancel that stops it.
func startRun(t *testing.T, r *Receiver) (<-chan ports.Delivery, <-chan error, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	got := make(chan ports.Delivery, 1)
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			got <- d
			return nil
		})
	}()
	return got, done, cancel
}

// awaitWhileRunning waits for cond and fails at once if Run #2 returns
// first: an early return means the prior Close ended the receiver, not
// just its Run.
func awaitWhileRunning(t *testing.T, done <-chan error, desc string, cond func() bool) {
	t.Helper()
	wait.Until(t, 2*time.Second, desc, func() bool {
		select {
		case err := <-done:
			t.Fatalf("Run after Close returned %v before %s", err, desc)
		default:
		}
		return cond()
	})
}
