package paho

import (
	"log/slog"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// A broker-retained subscription for a route removed from config is identified
// only by the concrete topic its post-grace publish arrived on — MQTT exposes
// no way to list subscriptions. UNSUBSCRIBE, however, matches the FILTER a
// subscription was created with, never a topic that filter covers. So an
// UNSUBSCRIBE built from the concrete topic converges an EXACT-filter orphan
// and can never converge a WILDCARD one: the broker answers 0x11 ("no
// subscription existed") and keeps delivering.
//
// Only a session with exact managed subscription history knows the filter, so
// the unmanaged path must REPORT the surviving orphan instead of logging a
// cleanup it did not perform. The publishes keep being acked-and-dropped
// (MQTTRouterUnmatchedDropped), so ingress never stalls — but a steadily rising
// count now has a log line naming why it will not stop on its own.

// TestOrphan_WildcardOrphan_UnmanagedSessionDoesNotClaimCleanup pins the honest
// reporting of an UNSUBACK that removed nothing.
//
// Counterfactual (the pre-fix claim): 0x11 was classified as "confirmed" and
// the session logged "unsubscribed orphan topic" at Debug — a convergence that
// never happened, with nothing at any level telling an operator the orphan
// survives.
func TestOrphan_WildcardOrphan_UnmanagedSessionDoesNotClaimCleanup(t *testing.T) {
	clk := testClock()
	logs := &recordingLogHandler{}
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "wildcard-orphan",
		UnmatchedGrace: testGrace,
		Clock:          clk,
	}, connectivity.SessionPersistent, slog.New(logs), &ports.RecordingExporter{})

	// 0x11 == "No subscription existed": the concrete topic is not a filter the
	// broker holds, so the orphan is a wildcard/shared subscription.
	fake := &recordingUnsubConn{reason: 0x11}
	sess.mu.Lock()
	sess.cm = fake
	sess.plan = &connectivity.SessionPlan{}
	sess.mu.Unlock()

	sess.handleConnectionUp()
	clk.Advance(testGrace + time.Second)
	sess.Router().dispatch(&pahov5.Publish{Topic: "removed/route/1", QoS: 1, Payload: []byte("x")},
		func() error { return nil })

	require.Eventually(t, func() bool {
		return len(fake.unsubscribed()) == 1
	}, time.Second, time.Millisecond, "the exact orphan topic is still attempted once")
	require.Equal(t, []string{"removed/route/1"}, fake.unsubscribed())

	require.Eventually(t, func() bool {
		return logs.warnCountContaining("orphan broker subscription survived") == 1
	}, time.Second, time.Millisecond,
		"an UNSUBACK that removed nothing must report the surviving wildcard orphan")
	require.Zero(t, logs.messageCountContaining(slog.LevelDebug, "unsubscribed orphan topic"),
		"nothing was removed, so no cleanup may be claimed")
}

// TestOrphan_ExactOrphanFilter_RemovalIsClaimed is the negative control: an
// orphan whose subscription really was created with this exact filter IS
// removed (UNSUBACK 0x00), so the convergence claim stands and no warning
// fires.
func TestOrphan_ExactOrphanFilter_RemovalIsClaimed(t *testing.T) {
	clk := testClock()
	logs := &recordingLogHandler{}
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "exact-orphan",
		UnmatchedGrace: testGrace,
		Clock:          clk,
	}, connectivity.SessionPersistent, slog.New(logs), &ports.RecordingExporter{})

	fake := &recordingUnsubConn{} // reason 0x00: the filter existed and is gone
	sess.mu.Lock()
	sess.cm = fake
	sess.plan = &connectivity.SessionPlan{}
	sess.mu.Unlock()

	sess.handleConnectionUp()
	clk.Advance(testGrace + time.Second)
	sess.Router().dispatch(&pahov5.Publish{Topic: "removed/route/2", QoS: 1, Payload: []byte("x")},
		func() error { return nil })

	require.Eventually(t, func() bool {
		return logs.messageCountContaining(slog.LevelDebug, "unsubscribed orphan topic") == 1
	}, time.Second, time.Millisecond, "a confirmed removal is the convergence this path exists for")
	require.Zero(t, logs.warnCountContaining("orphan broker subscription survived"))
}

// TestOrphan_ManagedSession_NeverGuessesFromAConcreteTopic pins the reason the
// managed path opts out entirely: its durable ledger holds the EXACT filters
// and reconcile unsubscribes those, so inferring a filter from a delivered
// topic could only remove the wrong thing.
func TestOrphan_ManagedSession_NeverGuessesFromAConcreteTopic(t *testing.T) {
	clk := testClock()
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "managed-orphan",
		UnmatchedGrace: testGrace,
		Clock:          clk,
	}, connectivity.SessionPersistent, nil, &ports.RecordingExporter{})

	fake := &recordingUnsubConn{}
	sess.mu.Lock()
	sess.cm = fake
	sess.plan = &connectivity.SessionPlan{}
	sess.managedRequired = true
	sess.mu.Unlock()

	sess.unsubscribeOrphan("removed/route/3")
	require.Empty(t, fake.unsubscribed(),
		"a managed session converges from its exact durable history, never from a concrete topic")
}
