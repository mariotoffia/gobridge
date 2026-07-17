package paho

import (
	"context"
	"log/slog"
	"testing"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// A-5 (MED): ServiceLevelFull must NOT be reported while the router still holds
// covered publishes in its pending buffer. The classic gap: a session with
// several receivers loses one — its subscription stays active and the OTHER
// receivers keep the session-total handler count above zero, so the naive
// `activeCount == wantedCount && handlerCount > 0` test reports Full even though
// the dead receiver's messages are pinned in the pending buffer (retained as
// still-covered past the grace window, or waiting for the handler to
// re-register). Health must degrade while that backlog is non-empty.
//
// In steady state (every subscription's handler live) an incoming publish
// dispatches immediately and never enters the buffer, so PendingCount() is 0
// and Full is reported normally — the "all subs active with handlers" case in
// TestHealth_ServiceLevel already pins that.
//
// Mutation killed:
//   - drop `&& pendingCount == 0` from the Full case  → the seeded backlog is
//     ignored and Health reports Full instead of Degraded; the first assertion
//     fails.
//
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_Health_PendingBacklog_DegradesFromFull(t *testing.T) {
	logger := slog.Default()
	s := NewSession(SessionOptions{}, connectivity.SessionEphemeral, logger)

	// Connected, one desired subscription that IS active, one live handler:
	// the exact shape that would otherwise report Full.
	s.mu.Lock()
	s.cm = &pahoConn{cm: &autopaho.ConnectionManager{}}
	s.connected = true
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "test/topic/a", QoS: 1}},
	}
	s.activeSubs["test/topic/a"] = 1
	s.subscriptionsSatisfied = true
	s.mu.Unlock()
	s.router.Register("handler-0", func(*pahov5.Publish) {})

	// Baseline: with an empty pending buffer this session is Full.
	assert.Equal(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel,
		"empty pending buffer should report Full")

	// Seed one covered publish still pinned in the pending buffer (a dead
	// receiver's retained message).
	s.router.mu.Lock()
	s.router.pending = append(s.router.pending, pendingPublish{
		pub:   &pahov5.Publish{Topic: "test/topic/a"},
		ack:   func() error { return nil },
		epoch: s.router.connEpoch,
	})
	s.router.mu.Unlock()

	// A non-empty backlog degrades readiness even though active==wanted and a
	// handler is registered.
	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(context.Background()).ServiceLevel,
		"non-empty pending buffer should degrade from Full")
}
