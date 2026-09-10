package paho

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// TestBugA_Integration_ReconcileBeforeStart drives the full real-broker
// flow that programmatic wiring commonly performs: Reconcile (which
// stashes the plan while the ConnectionManager is still nil and returns
// "session not started") → Start → manager-driven Reconcile on
// SessionConnected.
//
// Per finding the runtime session manager is the SINGLE owner of
// reconnect reconciliation: handleConnectionUp only resets activeSubs and
// signals SessionConnected, and the manager reacts by driving Reconcile,
// which re-establishes the previously stashed plan and emits
// SessionReconciled. This test asserts the still-load-bearing invariant
// that a plan stashed BEFORE Start survives and is applied
// (activeSubs == 1) with the session reporting Connected.
func TestBugA_Integration_ReconcileBeforeStart(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess := NewSession(SessionOptions{
		BrokerURLs:       []string{url},
		ClientID:         mqttlocal.UniqueClientID("bug-a-integ"),
		KeepAlive:        10,
		ConnectTimeout:   5 * time.Second,
		ReconnectTimeout: 5 * time.Second,
		CleanStart:       true,
	}, connectivity.SessionEphemeral, nil)

	// Stash a plan BEFORE Start. This sets s.plan and returns
	// "session not started".
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "bug-a/integ/topic", QoS: 1},
		},
	}
	if err := sess.Reconcile(ctx, plan); err == nil {
		t.Fatal("expected Reconcile-before-Start to error")
	}

	// Start the session. This must not panic and Health must report
	// Connected once the initial connection is established.
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	// Emulate the runtime session manager (it is the single
	// owner of reconnect reconciliation). On SessionConnected the manager
	// drives Reconcile, which re-establishes the stashed plan and emits
	// SessionReconciled.
	deadline := time.After(10 * time.Second)
	gotReconciled := false
EVENTS:
	for !gotReconciled {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break EVENTS
			}
			switch ev.Type {
			case ports.SessionConnected:
				if err := sess.Reconcile(ctx, plan); err != nil {
					t.Fatalf("manager-driven Reconcile: %v", err)
				}
			case ports.SessionReconciled:
				gotReconciled = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for SessionReconciled event")
		}
	}

	h := sess.Health(ctx)
	if !h.Connected {
		t.Fatal("session should be connected after Start")
	}

	sess.mu.Lock()
	subs := len(sess.activeSubs)
	sess.mu.Unlock()
	assert.Equal(t, 1, subs,
		"pre-Start plan must be applied by the manager-driven Reconcile "+
			"after SessionConnected")
}
