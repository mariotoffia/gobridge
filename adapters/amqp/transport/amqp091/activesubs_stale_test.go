// ═══════════════════════════════════════════════
// Adversarial-review remediation tests: a timed-out topology declare must not
// leave stale activeSubs, and the reconcile commit is generation-guarded
// Run under -race.
//
// The pre-fix reconcile reset activeSubs to empty up front and declareSubscription
// wrote activeSubs[queue]=true from the abandonable declare goroutine. So a
// plan-A declare that timed out could later unwind and write its stale queues
// over a newer plan-B's activeSubs (health reports subscriptions for a stale
// plan/connection). The fix: declaration returns queue names LOCALLY, and
// reconcile commits activeSubs only AFTER a successful declare, under a
// connection-generation guard — never from the abandoned goroutine.
//
// A fully-hermetic "abandoned goroutine writes activeSubs after plan B" race is
// not reproducible here because amqpChannel is concrete and a successful
// QueueDeclare needs a live broker; these tests instead pin the two mechanisms
// the fix relies on (last-known-good preservation on timeout, generation-guarded
// commit), each of which independently catches the corresponding mutation.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// TestReconcile_TimedOutDeclare_PreservesLastKnownGoodActiveSubs asserts a
// reconcile whose topology declare wedges past the deadline leaves activeSubs at
// its last-known-good value rather than clobbering it. Mutation (reinstate the
// up-front `s.activeSubs = make(...)` reset at the top of reconcile): the timed
// out reconcile empties activeSubs and the assertion fails.
func TestReconcile_TimedOutDeclare_PreservesLastKnownGoodActiveSubs(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newDeclareTestSession(rec)

	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var once sync.Once
	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) {
		once.Do(func() { close(started) })
		<-release // model a ctx-less declare wedged on a half-dead broker
		return nil, errors.New("channel refused after release")
	}

	// Install a live connection and a last-known-good activeSubs, as a session
	// that already reconciled once would look.
	s.mu.Lock()
	s.conn = mc
	s.activeSubs = map[string]bool{"lkg": true}
	s.mu.Unlock()

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "newq"}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel() // cancel once the declare is genuinely wedged
	}()

	err := s.reconcile(ctx, mc, plan)
	require.Error(t, err, "a wedged declare must fail reconcile")

	s.mu.Lock()
	subs := make(map[string]bool, len(s.activeSubs))
	for k, v := range s.activeSubs {
		subs[k] = v
	}
	s.mu.Unlock()
	require.Equal(t, map[string]bool{"lkg": true}, subs,
		"a timed-out reconcile must preserve the last-known-good activeSubs, not reset it")
}

// TestReconcile_GenerationGuard_SkipsCommitForStaleConnection asserts reconcile
// commits activeSubs only when its connection is still the installed one. A
// reconcile driven for connection B while the session already advanced to
// connection A must NOT overwrite A's activeSubs. Mutation (drop the
// `if s.conn == conn` guard around the commit): the stale reconcile clears the
// live activeSubs and the assertion fails.
func TestReconcile_GenerationGuard_SkipsCommitForStaleConnection(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newDeclareTestSession(rec)

	mcA := newMockConnection()
	mcB := newMockConnection()

	// The session is currently on connection A with a live subscription view.
	s.mu.Lock()
	s.conn = mcA
	s.activeSubs = map[string]bool{"keep": true}
	s.mu.Unlock()

	// Reconcile is driven for the STALE connection B (empty plan → declare
	// succeeds immediately with no queues), reaching the commit.
	err := s.reconcile(context.Background(), mcB, connectivity.SessionPlan{})
	require.NoError(t, err, "an empty-plan reconcile succeeds")

	s.mu.Lock()
	subs := make(map[string]bool, len(s.activeSubs))
	for k, v := range s.activeSubs {
		subs[k] = v
	}
	s.mu.Unlock()
	require.Equal(t, map[string]bool{"keep": true}, subs,
		"a reconcile for a stale connection must not commit activeSubs over the live connection's view")
}
