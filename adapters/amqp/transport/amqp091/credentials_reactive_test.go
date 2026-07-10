package amqp091

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSession_SetAuthFailureCallback_ReconnectAuthFailure_ForcesReactiveReResolve
// verifies the HIGH-3 reactive-recovery wiring: when a hard rotation revokes the
// old credentials, the reconnect redial fails with 403 access-refused
// (shared.ErrNotAuthorized) and the URI-bound callback injected by the
// CredentialRefresher is invoked, forcing an immediate re-resolve instead of
// looping on the revoked material until the next poll.
//
// Mutation (drop `s.reportAuthFailure(MapError(err))` in doReconnect): the redial
// still fails but the callback never fires and the RequireReceive times out.
func TestSession_SetAuthFailureCallback_ReconnectAuthFailure_ForcesReactiveReResolve(t *testing.T) {
	mc1 := newMockConnection()
	var dialN atomic.Int64
	authErr := &amqp.Error{Code: 403, Reason: "ACCESS_REFUSED - credentials revoked"}
	s := newResilienceSession(func(string) (amqpConnection, error) {
		if dialN.Add(1) == 1 {
			return mc1, nil
		}
		return nil, authErr // every redial after the rotation fails auth
	})
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	wait.Until(t, 2*time.Second, "connected on the first connection", func() bool {
		return s.connectionIfReady() == mc1
	})

	reported := make(chan error, 1)
	s.SetAuthFailureCallback(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})

	// Model ApplyCredentials's detach + wake: drop the stale conn and force a
	// reconnect. The redial fails 403.
	s.mu.Lock()
	s.connected = false
	s.conn = nil
	s.mu.Unlock()
	s.forceReconnect <- struct{}{}

	got := wait.RequireReceive(t, reported, 2*time.Second)
	require.ErrorIs(t, got, shared.ErrNotAuthorized,
		"a reconnect auth failure must invoke the reactive-recovery callback")
}

// TestSession_SetAuthFailureCallback_ReconnectNonAuthError_DoesNotReport
// verifies the callback is NOT invoked when a reconnect fails for a non-auth
// reason (connection refused): only NOT_AUTHORIZED forces a reactive re-resolve.
func TestSession_SetAuthFailureCallback_ReconnectNonAuthError_DoesNotReport(t *testing.T) {
	mc1 := newMockConnection()
	var dialN atomic.Int64
	s := newResilienceSession(func(string) (amqpConnection, error) {
		if dialN.Add(1) == 1 {
			return mc1, nil
		}
		return nil, errors.New("dial tcp: connection refused")
	})
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	wait.Until(t, 2*time.Second, "connected on the first connection", func() bool {
		return s.connectionIfReady() == mc1
	})

	reported := make(chan error, 1)
	s.SetAuthFailureCallback(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})

	s.mu.Lock()
	s.connected = false
	s.conn = nil
	s.mu.Unlock()
	s.forceReconnect <- struct{}{}

	// Prove the redial (and thus the report chokepoint) actually executed...
	wait.Until(t, 2*time.Second, "the forced reconnect attempted a redial", func() bool {
		return dialN.Load() >= 2
	})
	// ...yet a non-auth failure must not report.
	wait.Silent(t, reported, 100*time.Millisecond)
}
