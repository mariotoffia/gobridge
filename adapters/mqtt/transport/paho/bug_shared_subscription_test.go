package paho

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// errorCountContaining counts captured ERROR records whose message contains
// substr. It complements warnCountContaining (bug_covered_dropped_test.go) so
// the tests can distinguish the escalated Error from a benign Warn.
func (h *recordingLogHandler) errorCountContaining(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == slog.LevelError && strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}

const sharedSubAdvisorySubstr = "shared subscriptions ($share)"

// TestBug_Takeover_SharedSubscription_EscalatesOnFirstOccurrence proves:
// a session takeover (0x8E) while shared subscriptions ($share) are active on a
// NON-Exclusive session is the smoking gun of the client_id-collision self-DOS
// (replicas that must be unique are sharing an identity and kicking each other
// out of one broker session), so the adapter escalates to Error on the FIRST
// takeover — not after the streak-of-3 storm heuristic. Without the fix the
// first takeover is a benign Warn, so the Error assertion fails.
func TestBug_Takeover_SharedSubscription_EscalatesOnFirstOccurrence(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	logs := &recordingLogHandler{}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "shared-collision",
		Clock:      clk,
	}, connectivity.SessionPersistent, slog.New(logs))

	// A reconciled plan carrying a shared subscription = scale-out intent that
	// REQUIRES a unique per-instance client_id.
	sess.mu.Lock()
	sess.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "$share/workers/orders/#", QoS: 1},
		},
	}
	sess.connUpAt = clk.Now().UnixNano()
	sess.mu.Unlock()

	// FIRST takeover.
	clk.Advance(time.Second)
	sess.handleServerDisconnect(disconnectSessionTakenOver)

	require.Equal(t, 1, logs.errorCountContaining(sharedSubAdvisorySubstr),
		"a takeover while $share is active must escalate to Error on the FIRST occurrence")
	require.Equal(t, 0, logs.warnCountContaining("taken over by another connection"),
		"the generic first-takeover Warn must be replaced by the shared-sub Error")
}

// TestBug_Takeover_SharedSubscription_ExclusiveIsLegitFailover pins that the
// escalation is mode-aware: in Exclusive mode a stable client_id is shared
// behind a lease (a single active owner), so a takeover is a LEGITIMATE lease
// handoff, not a collision. The first takeover must stay a benign Warn.
func TestBug_Takeover_SharedSubscription_ExclusiveIsLegitFailover(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	logs := &recordingLogHandler{}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "shared-exclusive",
		Clock:      clk,
	}, connectivity.SessionExclusive, slog.New(logs))

	sess.mu.Lock()
	sess.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "$share/workers/orders/#", QoS: 1},
		},
	}
	sess.connUpAt = clk.Now().UnixNano()
	sess.mu.Unlock()

	clk.Advance(time.Second)
	sess.handleServerDisconnect(disconnectSessionTakenOver)

	require.Equal(t, 0, logs.errorCountContaining(sharedSubAdvisorySubstr),
		"Exclusive shares a client_id behind a lease: a single takeover is legit failover, not a $share collision")
	require.Equal(t, 1, logs.warnCountContaining("taken over by another connection"),
		"Exclusive first takeover must stay a benign Warn")
}

// TestBug_Reconcile_SharedSubscription_WarnsOncePerSession proves the proactive
// half: when a plan with shared subscriptions is reconciled onto a
// stable-client_id (non-Ephemeral) session, the adapter warns ONCE about the
// unique-client_id requirement and deduplicates thereafter. cm is nil, so
// Reconcile records the plan, emits the advisory, then returns — enough to
// exercise the one-time warn without a live broker.
func TestBug_Reconcile_SharedSubscription_WarnsOncePerSession(t *testing.T) {
	logs := &recordingLogHandler{}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "shared-reconcile",
		Clock:      testClock(),
	}, connectivity.SessionPersistent, slog.New(logs))

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "$share/workers/orders/#", QoS: 1},
		},
	}

	_ = sess.Reconcile(context.Background(), plan)
	_ = sess.Reconcile(context.Background(), plan)

	require.Equal(t, 1, logs.warnCountContaining(sharedSubAdvisorySubstr),
		"the shared-sub client_id advisory must fire exactly once per session (deduped)")
}

// TestBug_Reconcile_SharedSubscription_EphemeralNotWarned pins that Ephemeral
// sessions — which already get a unique client_id + CleanStart, the correct
// scale-out shape — are NOT warned, keeping the advisory free of false noise.
func TestBug_Reconcile_SharedSubscription_EphemeralNotWarned(t *testing.T) {
	logs := &recordingLogHandler{}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "shared-ephemeral",
		Clock:      testClock(),
	}, connectivity.SessionEphemeral, slog.New(logs))

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "$share/workers/orders/#", QoS: 1},
		},
	}
	_ = sess.Reconcile(context.Background(), plan)

	require.Equal(t, 0, logs.warnCountContaining(sharedSubAdvisorySubstr),
		"Ephemeral sessions are the correct scale-out shape (unique client_id + CleanStart); no advisory")
}
