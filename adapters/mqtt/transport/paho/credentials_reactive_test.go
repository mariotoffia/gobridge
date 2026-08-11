package paho

import (
	"errors"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// newReactiveTestSession builds a minimal Session sufficient to exercise the
// reactive-recovery chokepoint (handleConnectError) without a broker.
func newReactiveTestSession(t *testing.T) *Session {
	t.Helper()
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tls://broker.example:8883"},
		ClientID:   "reactive-test",
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = s.Close(t.Context()) })
	return s
}

// TestSession_SetAuthFailureCallback_ConnackDenied_ForcesReactiveReResolve
// verifies the wiring: a CONNECT rejected by the broker with a CONNACK
// not-authorized reason (0x87) — the shape a hard credential rotation produces —
// invokes the URI-bound callback injected by the CredentialRefresher with
// shared.ErrNotAuthorized, forcing an immediate re-resolve.
//
// Mutation (drop `s.reportAuthFailure(mapped)` in handleConnectError, or revert
// mapConnectError to the generic MapError): the callback never fires (MapError
// misclassifies the ConnackError as ErrUnavailable) and RequireReceive times out.
func TestSession_SetAuthFailureCallback_ConnackDenied_ForcesReactiveReResolve(t *testing.T) {
	s := newReactiveTestSession(t)

	reported := make(chan error, 1)
	s.SetAuthFailureCallback(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})

	// autopaho surfaces a server CONNACK deny as *autopaho.ConnackError.
	s.handleConnectError(&autopaho.ConnackError{
		ReasonCode: 0x87, // Not authorized
		Reason:     "credentials revoked",
		Err:        errors.New("failed to connect to server: not authorized"),
	})

	got := wait.RequireReceive(t, reported, 2*time.Second)
	require.ErrorIs(t, got, shared.ErrNotAuthorized,
		"a CONNACK not-authorized must invoke the reactive-recovery callback")
}

// TestSession_SetAuthFailureCallback_ConnackNonAuth_DoesNotReport verifies the
// callback is NOT invoked for a non-auth CONNACK reason (0x88 server
// unavailable): only NOT_AUTHORIZED forces a reactive re-resolve.
func TestSession_SetAuthFailureCallback_ConnackNonAuth_DoesNotReport(t *testing.T) {
	s := newReactiveTestSession(t)

	reported := make(chan error, 1)
	s.SetAuthFailureCallback(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})

	s.handleConnectError(&autopaho.ConnackError{
		ReasonCode: 0x88, // Server unavailable
		Err:        errors.New("failed to connect to server: server unavailable"),
	})

	wait.Silent(t, reported, 100*time.Millisecond)
}
