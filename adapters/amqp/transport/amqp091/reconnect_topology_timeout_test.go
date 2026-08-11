// ═══════════════════════════════════════════════
// Production-readiness remediation tests: bounded topology declaration
// (Chunk-11).
//
// The amqp091-go declare calls (conn.Channel / ExchangeDeclare /
// QueueDeclare / QueueBind — see acl_session.go) are NOT context-aware: on a
// half-dead broker they block until it answers or the connection dies. The old
// reconcile ran them inline, so a single wedged declare hung Start, the public
// Reconcile, and the reconnect loop indefinitely — past the configured
// ConnectTimeout — leaving the session stuck with connected=false and no
// receivers.
//
// reconcile now runs the declarations on a goroutine raced against ctx
// (declareTopologyWithin), and Start / Reconcile bound that ctx by
// ConnectTimeout. On the deadline the wedged declare is abandoned (its
// goroutine unwinds once the driver drops the connection; done is buffered so
// it never leaks) and the caller drops the connection so the reconnect
// machinery can redial.
//
// These tests drive the wedge deterministically with a Channel() that blocks
// on a release channel (the ctx-less SDK declare) and force the deadline by
// cancelling ctx / setting a small ConnectTimeout — no real sleep.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSession_Reconcile_TopologyDeclareWedge_HonoursDeadline pins the
// reconcile-level contract: when a topology declaration wedges (the ctx-less
// SDK Channel/declare blocks on an unresponsive broker), reconcile must abandon
// it and return a TRANSIENT error the instant ctx fires — not block forever.
//
// Counterfactual (inline declare loops, pre-fix): reconcile calls conn.Channel()
// directly on its own goroutine and blocks on release; cancelling ctx does not
// unblock the ctx-ignoring call, so RequireReceive times out (the exact hang).
func TestSession_Reconcile_TopologyDeclareWedge_HonoursDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // unwind the abandoned declare goroutine after the test

	started := make(chan struct{})
	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) {
		close(started)
		<-release // ctx-less SDK declare that blocks on a half-dead broker
		return nil, errors.New("unreachable: released only at test cleanup")
	}
	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })

	plan := subscriptionPlanForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the declare is genuinely wedged so declareTopologyWithin
	// observes the deadline mid-declaration.
	go func() {
		<-started
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- s.reconcile(ctx, mc, plan) }()

	err := wait.RequireReceive(t, done, 2*time.Second)
	require.Error(t, err, "a wedged topology declaration must be abandoned when ctx fires, not block forever")
	var be *shared.BridgeError
	require.True(t, errors.As(err, &be), "reconcile must return a classified BridgeError, got %v", err)
	require.Equal(t, shared.ErrorTransient, be.Class,
		"a declaration abandoned on deadline must be transient so the reconnect loop retries")
}

// TestSession_Start_TopologyDeclareWedge_BoundedByConnectTimeout pins the
// Start-level contract: a caller that passes a DEADLINE-LESS ctx (the common
// case, context.Background()) must still not hang when the initial topology
// declaration wedges — Start bounds it by the configured ConnectTimeout and
// drops the connection so the caller is not left with a half-initialised
// session.
//
// Counterfactual (pre-fix, raw ctx into reconcile): Start blocks in the inline
// declare on release forever, so RequireReceive times out and the connection is
// never dropped.
func TestSession_Start_TopologyDeclareWedge_BoundedByConnectTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // unwind the abandoned declare goroutine after the test

	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) {
		<-release // ctx-less SDK declare that blocks until the connection dies
		return nil, errors.New("unreachable: released only at test cleanup")
	}
	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	s.opts.ConnectTimeout = 50 * time.Millisecond
	plan := subscriptionPlanForTest()
	s.plan = &plan

	done := make(chan error, 1)
	// Deadline-LESS ctx on purpose: the bound must come from ConnectTimeout,
	// not from the caller's ctx.
	go func() { done <- s.Start(context.Background()) }()

	err := wait.RequireReceive(t, done, 2*time.Second)
	require.Error(t, err, "Start must not hang on a wedged declare; ConnectTimeout must bound it")
	var be *shared.BridgeError
	require.True(t, errors.As(err, &be), "Start must return a classified BridgeError, got %v", err)
	require.Equal(t, shared.ErrorTransient, be.Class,
		"a Start aborted by the topology-declare deadline must be transient")

	// The wedged connection must be dropped so the caller is not pinned to a
	// half-initialised session and the reconnect machinery can redial.
	wait.Until(t, 2*time.Second, "wedged connection dropped on declare timeout", func() bool {
		return mc.closeCalls() >= 1
	})
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	require.Nil(t, conn, "no connection may remain installed after a declare-timeout Start")
	require.False(t, s.Health(context.Background()).Connected,
		"a session whose initial declare timed out must not report Connected")
}
