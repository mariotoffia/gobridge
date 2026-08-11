package paho

// Regression pins for the MQTT router poison-handling fixes that live in
// the router: (poison ack-drop instead of terminal),
// (recycle-window discards metered), (ack-after-reconnect counted).

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// every broker-forwardable representational cap violation is
// classified with a bounded class name, acked exactly once, dropped, and
// counted — never latched terminal. The ack is what frees the broker's
// in-flight slot and stops the redelivery loop.
func TestRouter_IngressPoisonAckDrop_ClassifiesAcksAndCounts(t *testing.T) {
	overMetadata := &pahov5.Publish{Topic: strings.Repeat("t", 65_535), QoS: 1, Properties: &pahov5.PublishProperties{
		User: pahov5.UserProperties{{Key: "k", Value: strings.Repeat("v", int(maxIngressMetadataBytes))}},
	}}
	tooManyProps := make(pahov5.UserProperties, maxIngressUserProperties+1)
	for i := range tooManyProps {
		tooManyProps[i] = pahov5.UserProperty{Key: "k", Value: "v"}
	}

	tests := []struct {
		name      string
		pub       *pahov5.Publish
		wantClass string
	}{
		{
			name:      "payload over cap",
			pub:       &pahov5.Publish{Topic: "poison/payload", QoS: 1, Payload: []byte("12345")},
			wantClass: "payload",
		},
		{
			name: "user property count over cap",
			pub: &pahov5.Publish{Topic: "poison/props", QoS: 1, Payload: []byte("ok"),
				Properties: &pahov5.PublishProperties{User: tooManyProps}},
			wantClass: "user_properties",
		},
		{
			name:      "metadata bytes over cap",
			pub:       overMetadata,
			wantClass: "metadata",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := &ports.RecordingExporter{}
			r := newRouter(nil, rec, withMaxPayloadBytes(4), withSessionTag("poison-session"))

			class, violation := r.ingressCapViolation(test.pub)
			require.Error(t, violation)
			assert.ErrorIs(t, violation, shared.ErrInvalidPayload)
			assert.Equal(t, test.wantClass, class)

			acks := 0
			r.dropPoisonIngress(test.pub, class, violation, func() error {
				acks++
				return nil
			})
			assert.Equal(t, 1, acks, "the poison escape MUST ack to free the broker slot")
			assert.Equal(t, int64(1), r.IngressPoisonDroppedCount())
			require.Len(t, rec.FindEntries(MetricMQTTIngressPoisonDropped), 1)
		})
	}
}

// a within-cap publish is not a violation.
func TestRouter_IngressCapViolation_WithinCapsPasses(t *testing.T) {
	r := newRouter(nil, nil, withMaxPayloadBytes(16))
	class, violation := r.ingressCapViolation(&pahov5.Publish{
		Topic: "clean/topic", QoS: 1, Payload: []byte("ok"),
	})
	assert.NoError(t, violation)
	assert.Empty(t, class)
}

// a failed ack (connection cycled under the drop) must not panic or
// count differently — the redelivered copy is dropped and counted again.
func TestRouter_IngressPoisonAckDrop_AckFailureStillDropsAndCounts(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withMaxPayloadBytes(4))
	pub := &pahov5.Publish{Topic: "poison/ack-fail", QoS: 1, Payload: []byte("12345")}
	class, violation := r.ingressCapViolation(pub)
	require.Error(t, violation)

	r.dropPoisonIngress(pub, class, violation, func() error { return pahov5.ErrPacketNotFound })
	assert.Equal(t, int64(1), r.IngressPoisonDroppedCount())
	require.Len(t, rec.FindEntries(MetricMQTTIngressPoisonDropped), 1)
}

// the recycle-window discard branches — previously the router's
// only fully silent drops — count on MetricMQTTRouterStalePurged.
func TestRouter_RecycleWindowDiscardsAreMetered(t *testing.T) {
	t.Run("enqueueDispatch discard", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		r := newRouter(nil, rec)
		r.beginGrace()
		t.Cleanup(r.shutdown)
		require.NoError(t, r.quiesceForRecycle(t.Context(), nil))

		pub := &pahov5.Publish{Topic: "recycle/enqueue", QoS: 0, Payload: []byte("x")}
		r.enqueueDispatch(pub, nil)

		assert.Equal(t, int64(1), r.StalePurgedCount(),
			"a publish arriving while discarding must be counted, not silently released")
		require.Len(t, rec.FindEntries(MetricMQTTRouterStalePurged), 1)
	})

	t.Run("dispatchCore discard of an already-queued item", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		r := newRouter(nil, rec)
		r.beginGrace()
		t.Cleanup(r.shutdown)
		require.NoError(t, r.quiesceForRecycle(t.Context(), nil))

		r.mu.Lock()
		epoch := r.connEpoch
		r.mu.Unlock()
		r.dispatchAtEpoch(&pahov5.Publish{Topic: "recycle/queued", QoS: 1}, nil, epoch, true)

		assert.Equal(t, int64(1), r.StalePurgedCount())
		require.Len(t, rec.FindEntries(MetricMQTTRouterStalePurged), 1)
	})
}

// an ack whose connection cycled (paho ErrPacketNotFound) maps to
// SUCCESS — the broker redelivers, downstream dedup absorbs — but the mapped
// success is counted so the duplicate window is observable. Any other ack
// error stays a settlement failure and is not counted here.
func TestRouter_AckAfterReconnectMappedSuccessIsCounted(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withSessionTag("l5-session"))

	settle := r.ackWithReconnectMapping(func() error { return pahov5.ErrPacketNotFound })
	require.NoError(t, settle(), "ErrPacketNotFound maps to success (broker redelivers)")
	assert.Equal(t, int64(1), r.AckAfterReconnectCount())
	require.Len(t, rec.FindEntries(MetricMQTTAckAfterReconnect), 1)

	failing := r.ackWithReconnectMapping(func() error { return errors.New("broker rejected") })
	require.Error(t, failing(), "a non-cycled ack error remains a settlement failure")
	assert.Equal(t, int64(1), r.AckAfterReconnectCount(), "only the cycled mapping is counted")

	ok := r.ackWithReconnectMapping(func() error { return nil })
	require.NoError(t, ok())
	assert.Equal(t, int64(1), r.AckAfterReconnectCount())
}

// before the first Reconcile of a process lifetime (nil plan) a
// post-grace unmatched publish is RETAINED as covered — never orphan-dropped,
// never unsubscribed. Once a Reconcile stashes a plan that does not cover it,
// reclassifyPending converges it as a genuine orphan.
func TestRouter_PreFirstReconcileBacklogRetainedThenReclassified(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "l2-pre-reconcile",
		UnmatchedGrace: testGrace,
		Clock:          clk,
	}, connectivity.SessionPersistent, nil, rec)
	fake := &recordingUnsubConn{}
	sess.mu.Lock()
	sess.cm = fake
	sess.mu.Unlock()

	// Connect edge; the broker replays the persistent backlog while NO plan
	// has ever been stashed (Manager.Run calls Start before Reconcile).
	sess.handleConnectionUp()
	clk.Advance(testGrace + time.Second)

	// atomic: the router may invoke this ack from its grace-loop goroutine
	// (graceLoop -> sweepUnmatched -> settlePending -> dropOrphan), so a plain
	// int here is a data race against the assertions below, not merely untidy.
	var acked atomic.Int64
	sess.Router().dispatch(&pahov5.Publish{Topic: "live/backlog", QoS: 1, Payload: []byte("x")},
		func() error { acked.Add(1); return nil })

	assert.Zero(t, acked.Load(), "pre-first-Reconcile backlog must stay UN-ACKED (retained), not PUBACK-dropped")
	assert.Equal(t, int64(0), sess.Router().UnmatchedDroppedCount(),
		"live backlog must not be miscounted as benign orphan cleanup")
	assert.Equal(t, int64(1), sess.Router().CoveredRetainedCount())
	assert.Equal(t, 1, sess.Router().PendingCount())
	assert.Empty(t, fake.unsubscribed(), "the live topic must NOT be unsubscribed before the first plan exists")

	// First Reconcile stashes a plan that does NOT cover the topic: the
	// retained entry is reclassified as a true orphan (acked + dropped) by
	// the reconcile's settle pass.
	require.NoError(t, sess.Reconcile(context.Background(), connectivity.SessionPlan{}))
	// The ack can come from Reconcile's settle pass or from the grace loop,
	// whichever reaches the entry first, so this is an eventual state. Waiting
	// ON it is deterministic; asserting immediately assumed an ordering the
	// router never promised and failed roughly one run in ten.
	wait.Until(t, 5*time.Second, "reclassified orphan is acked to free the broker slot",
		func() bool { return acked.Load() == 1 })
	assert.Equal(t, int64(1), sess.Router().UnmatchedDroppedCount())
	assert.Zero(t, sess.Router().PendingCount())

	require.NoError(t, sess.Close(context.Background()))
}

// + unsettled accounting: a tracked packet whose ack maps
// ErrPacketNotFound to success settles out of the unsettled window (the
// packet cannot be settled on this connection anymore; the broker's
// redelivered copy is tracked fresh).
func TestRouter_TrackedAckAfterReconnectSettlesUnsettledWindow(t *testing.T) {
	r := newRouter(nil, &ports.RecordingExporter{})
	ack := r.trackAcknowledgement(r.ackWithReconnectMapping(func() error {
		return pahov5.ErrPacketNotFound
	}))
	require.Equal(t, 1, r.unsettledSnapshot(2).Count)
	require.NoError(t, ack())
	assert.Zero(t, r.unsettledSnapshot(2).Count)
	assert.Equal(t, int64(1), r.AckAfterReconnectCount())
}

// keep context import used when build tags shuffle helpers around.
var _ = context.Background
