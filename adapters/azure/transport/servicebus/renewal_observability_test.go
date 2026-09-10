package servicebus

// renewal_observability_test.go
//
// the shared session renewer must NOT die permanently after a burst
//     of consecutive failures — it must keep covering every session the
//     poll loop accepts afterwards (renewal continuity across re-accepts).
// both renewers must make renewal degradation alertable — a failure
//     counter and a renewer-stopped/degraded signal, not just the
//     success counter.
//
// Timing is driven by the fake clock + channel/counter synchronisation
// (waitUntil, signalClock, <-renewed) — no time.Sleep (TESTS.md).

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// + (session renewer): a session whose lock renewal fails past the
// consecutive-failure threshold must NOT kill the renewer. After a fresh
// session is accepted (sessionGen bumps, mirroring ensureSessionSeam) the
// renewer must renew the NEW session — proving renewal continuity across
// re-accepts. It must also emit the failure counter and the degraded
// signal for the failing session.
func TestRunSessionRenewer_RecoversAcrossReacceptAfterFailures(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	metrics := &namedMetrics{}

	var fails atomic.Int32
	failing := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			fails.Add(1)
			return errors.New("renew boom")
		},
	}
	var renewsHealthy atomic.Int32
	healthy := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewsHealthy.Add(1)
			return nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		UseSessions:  true,
		LockDuration: 10 * time.Second,
		Client:       failing,
		Clock:        fake,
		Metrics:      metrics,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(context.Background())) // installs `failing`

	renewCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go recv.runSessionRenewer(renewCtx, 5*time.Second)

	waitUntil(t, 5*time.Second, func() bool { return fake.TickerCount() >= 1 },
		"renewer ticker never registered")

	// Drive MORE than autoExtendMaxFailures ticks on the failing session.
	// Pre-fix the renewer would have returned (died) at the threshold.
	for i := 1; i <= autoExtendMaxFailures+1; i++ {
		want := int32(i)
		fake.Advance(6 * time.Second)
		waitUntil(t, 5*time.Second, func() bool { return fails.Load() >= want },
			"renewer stopped renewing the failing session (must not die on consecutive failures)")
	}

	// the failing session must have emitted the failure counter and
	// the degraded signal (exactly once per episode).
	require.GreaterOrEqual(t, metrics.count(MetricASBLockRenewalFailures), int64(autoExtendMaxFailures))
	require.Equal(t, int64(1), metrics.count(MetricASBLockRenewerStopped))
	// the session renewer's stopped signal is scoped "session"
	// (degraded but still running) — distinct from a delivery renewer's
	// "delivery" scope (imminent redelivery).
	require.True(t, metrics.hasTag(MetricASBLockRenewerStopped, asbTagKeyRenewerScope, asbRenewerScopeSession))
	require.False(t, metrics.hasTag(MetricASBLockRenewerStopped, asbTagKeyRenewerScope, asbRenewerScopeDelivery))

	// Re-accept a healthy session: swap the live seam and bump the
	// generation, exactly as ensureSessionSeam does on rotation.
	recv.swapStack(receiverStack{client: healthy})
	recv.sessionGen.Add(1)

	fake.Advance(6 * time.Second) // next tick → renew via the NEW session
	waitUntil(t, 5*time.Second, func() bool { return renewsHealthy.Load() >= 1 },
		"renewer must renew the re-accepted session after a prior session's failures")

	cancel()
}

// (per-delivery auto-extend): the loop must emit the failure counter
// on every renewal error and the renewer-stopped signal when it gives up
// after autoExtendMaxFailures consecutive failures.
func TestAutoExtendLoop_EmitsFailureAndStoppedMetrics(t *testing.T) {
	t.Parallel()

	renewed := make(chan struct{}, 1)
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			signal(renewed)
			return errors.New("always fail")
		},
	}
	metrics := &namedMetrics{}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	clk := newSignalClock()
	d := newDelivery(context.Background(), env, mock, nil, msg,
		deliveryTuning{lockDuration: 2 * time.Second, autoExtend: true},
		nil, nil, metrics, clk)
	defer d.stop()

	<-clk.started // renewal ticker armed

	for i := 0; i < autoExtendMaxFailures; i++ {
		clk.Advance(1100 * time.Millisecond) // tick fires (always fails)
		<-renewed                            // block until the goroutine processed the tick
	}
	<-clk.stopped // loop returned after the consecutive-failure threshold

	require.GreaterOrEqual(t, metrics.count(MetricASBLockRenewalFailures), int64(autoExtendMaxFailures))
	require.Equal(t, int64(1), metrics.count(MetricASBLockRenewerStopped))
	// the per-delivery renewer's stopped signal is scoped
	// "delivery" (the message WILL redeliver), distinct from "session".
	require.True(t, metrics.hasTag(MetricASBLockRenewerStopped, asbTagKeyRenewerScope, asbRenewerScopeDelivery))
	require.False(t, metrics.hasTag(MetricASBLockRenewerStopped, asbTagKeyRenewerScope, asbRenewerScopeSession))
	// The success counter must NOT have fired (every renewal failed).
	require.Zero(t, metrics.count(MetricASBLockRenewals))
}
