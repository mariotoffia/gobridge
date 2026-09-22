package paho

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	runtimesession "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestQoSDowngrade_RuntimeSessionManagerNeverTerminates drives the runtime
// session manager over a session whose broker grants every subscription below
// the requested QoS. The manager must keep running with the session serving at
// best effort. The old reconcile returned ErrQoSNotSupported, which ended Run at
// once and — confirmed three times — became ErrSessionUnrecoverable, a process
// restart.
func TestQoSDowngrade_RuntimeSessionManagerNeverTerminates(t *testing.T) {
	clk := testClock()
	fake := &fakeReconcileConn{reasons: []byte{0x00}}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "downgrade-runtime",
		Clock:      clk,
	}, connectivity.SessionPersistent, nil)
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return fake, func() {}, nil
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	plan := withReceiver(s, planAtQoS("sensors/x", 1))
	mgr := runtimesession.NewFromConfig(runtimesession.Config{
		SessionID: "downgrade-runtime",
		Plan:      plan,
	}, s, nil, "owner-downgrade-runtime", nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()

	wait.Until(t, 5*time.Second, "the manager's reconcile recorded the downgrade", func() bool {
		_, recorded := downgradeState(s, "sensors/x")
		return recorded
	})
	// The record is visible after the SUBACK walk arms the first probe, but
	// the reconcile re-arms once more at its end. Advancing between the two
	// would fire a timer that is already replaced, so wait for the reconcile
	// to release the serialization gate first.
	require.NoError(t, s.acquireReload(ctx))
	s.releaseReload()
	confirmDowngrade(t, s, clk, fake, "sensors/x")

	h := s.Health(ctx)
	require.Equal(t, ports.ServiceLevelFull, h.ServiceLevel)
	require.Equal(t, []string{"sensors/x"}, h.BestEffortTopics)
	select {
	case err := <-runErr:
		t.Fatalf("manager Run returned while the session serves at best effort: %v", err)
	default:
	}

	cancel()
	err := wait.RequireReceive(t, runErr, 5*time.Second)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errors.Is(err, runtimesession.ErrSessionUnrecoverable))
}
