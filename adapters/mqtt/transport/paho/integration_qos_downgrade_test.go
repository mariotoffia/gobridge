package paho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	runtimesession "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// The QoS downgrade against a broker that really grants less than it is asked
// for. Mosquitto's max_qos caps every SUBACK, so a QoS 1 subscription is granted
// QoS 0 on each fresh SUBSCRIBE. The session and the runtime session manager
// driving it are the production ones; only the session's clock is fake, so the
// confirmation probes and the re-check fire when the test advances it while
// every SUBSCRIBE, SUBACK and delivery crosses a real connection.
//
// Recovery goes through a broker restart. Mosquitto does not reload max_qos on
// a reload signal (mosquitto.conf(5): "Not reloaded on reload signal"), so it
// cannot lift the cap without dropping the connection. The re-check that finds
// the requested QoS granted again on a live connection is covered by the unit
// test TestQoSDowngrade_RecheckGrantingRequestedQoS_RestoresSubscription.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

const (
	downgradeTopic      = "qos-downgrade/real-broker"
	downgradeReceiverID = "rx-qos-downgrade"
	// retainedReplayWindow is how long the handler must stay silent after the
	// re-check SUBACK. A broker replays retained messages right behind the
	// SUBACK, within milliseconds on a local broker.
	retainedReplayWindow = time.Second
)

func TestIntegration_QoSDowngrade_RealBrokerCapIsAcceptedAsBestEffortAndRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithMaxQoS(0))

	clk := testClock()
	logs := &recordingLogHandler{}
	rec := &ports.RecordingExporter{}
	clientID := mqttlocal.UniqueClientID("qos-downgrade-real")
	s := NewSession(SessionOptions{
		BrokerURLs:            []string{broker.URL()},
		ClientID:              clientID,
		KeepAlive:             5,
		ConnectTimeout:        10 * time.Second,
		SessionExpiryInterval: 60,
		ReconnectDelay:        200 * time.Millisecond,
		ReconnectMaxDelay:     time.Second,
		Clock:                 clk,
	}, connectivity.SessionPersistent, slog.New(logs), rec)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	received := recordDeliveries(t, s, downgradeReceiverID, downgradeTopic)

	plan := planAtQoS(downgradeTopic, 1)
	plan.ExpectedReceiverIDs = []string{downgradeReceiverID}
	mgr := runtimesession.NewFromConfig(runtimesession.Config{
		SessionID: clientID,
		Plan:      plan,
	}, s, nil, "owner-"+clientID, nil)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Run(ctx) }()
	requireRunning := func(phase string) {
		t.Helper()
		select {
		case err := <-runErr:
			t.Fatalf("manager Run returned %s: %v", phase, err)
		default:
		}
	}

	// The first SUBACK from the capped broker granted QoS 0: the downgrade is
	// recorded and confirming, and the session is degraded but running.
	awaitReconciles(t, s, rec, 2)
	d, recorded := downgradeState(s, downgradeTopic)
	require.True(t, recorded, "the broker's lower grant must be recorded")
	require.Equal(t, byte(0), d.granted)
	require.Equal(t, 1, d.confirmations, "the manager's second reconcile must not re-SUBSCRIBE")
	require.False(t, d.accepted())
	h := s.Health(ctx)
	require.Equal(t, ports.ServiceLevelDegraded, h.ServiceLevel)
	require.NotNil(t, h.SubscriptionsSatisfied)
	require.False(t, *h.SubscriptionsSatisfied, "a lower grant still confirming is not satisfied")
	require.Empty(t, h.BestEffortTopics)
	requireRunning("while the downgrade was confirming")

	// Two confirmation probes, each a fresh SUBSCRIBE the broker caps again,
	// accept the grant as best effort.
	for n := 2; n <= qosDowngradeConfirmations; n++ {
		advanceAndAwait(t, s, clk, qosDowngradeConfirmInterval, fmt.Sprintf("confirmation %d", n), func() bool {
			d, ok := downgradeState(s, downgradeTopic)
			return ok && d.confirmations == n
		})
	}
	d, recorded = downgradeState(s, downgradeTopic)
	require.True(t, recorded, "the confirmed downgrade must still be recorded")
	require.True(t, d.accepted(), "%d identical SUBACKs accept the grant", qosDowngradeConfirmations)
	h = s.Health(ctx)
	require.Equal(t, ports.ServiceLevelFull, h.ServiceLevel)
	require.Equal(t, []string{downgradeTopic}, h.BestEffortTopics)
	require.NotNil(t, h.SubscriptionsSatisfied)
	require.True(t, *h.SubscriptionsSatisfied)
	requireGauge(t, rec, clientID, 1)
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelError, "best effort"),
		"acceptance is announced once, at Error")
	requireRunning("after accepting the downgrade")

	// The subscription carries traffic at the granted QoS. The capped broker
	// disconnects a client publishing above QoS 0, so every publish until the
	// cap is lifted is QoS 0.
	publishOnce(t, broker.URL(), downgradeTopic, 0, false, "live-at-granted-qos")
	require.Equal(t, brokerMessage{payload: "live-at-granted-qos", qos: 0},
		wait.RequireReceive(t, received, brokerWait))
	requireRunning("after a delivery at the granted QoS")

	// The re-check re-SUBSCRIBEs with retain handling 1, so the broker must not
	// replay a retained message to the subscription that already exists; retain
	// handling 0 would replay it on every re-check.
	publishOnce(t, broker.URL(), downgradeTopic, 0, true, "retained-state")
	require.Equal(t, brokerMessage{payload: "retained-state", qos: 0},
		wait.RequireReceive(t, received, brokerWait), "the retained message arrives once, live")
	before, recorded := downgradeState(s, downgradeTopic)
	require.True(t, recorded, "the accepted downgrade must be recorded before the re-check")
	errorLogs := logs.messageCountContaining(slog.LevelError, "")
	advanceAndAwait(t, s, clk, DefaultQoSRecheckInterval, "the re-check SUBACK was recorded", func() bool {
		d, ok := downgradeState(s, downgradeTopic)
		return ok && d.due.After(before.due)
	})
	after, recorded := downgradeState(s, downgradeTopic)
	require.True(t, recorded, "a re-check still granted lower must keep the downgrade")
	require.Zero(t, after.noVerdictRounds, "the re-check SUBSCRIBE must have been granted")
	require.Equal(t, before.confirmations, after.confirmations)
	require.Equal(t, before.acceptedAt, after.acceptedAt, "a re-check still granted lower changes nothing")
	require.Equal(t, before.due.Add(DefaultQoSRecheckInterval), after.due, "the next re-check is one interval on")
	requireGauge(t, rec, clientID, 1)
	require.Equal(t, errorLogs, logs.messageCountContaining(slog.LevelError, ""), "an unchanged grant logs nothing")
	wait.Silent(t, received, retainedReplayWindow)

	// Clearing the retained message is delivered like any other message, which
	// also proves the order: nothing, not even a late replay, arrived before it.
	publishOnce(t, broker.URL(), downgradeTopic, 0, true, "")
	require.Equal(t, brokerMessage{qos: 0}, wait.RequireReceive(t, received, brokerWait))
	requireRunning("after the re-check")

	// The restarted broker has no cap. The session reconnects on its own, and
	// the manager's reconcile on SessionConnected is granted the requested QoS:
	// the downgrade clears.
	broker.RestartWith(mqttlocal.WithMaxQoS(2))
	wait.Until(t, brokerWait, "the reconnect's SUBACK granted the requested QoS", func() bool {
		_, recorded := downgradeState(s, downgradeTopic)
		return !recorded
	})
	awaitReloadGate(t, s, brokerWait, "await the recovering reconcile")
	qos, active := activeQoS(s, downgradeTopic)
	require.True(t, active, "the subscription is active again")
	require.Equal(t, byte(1), qos, "at the requested QoS")
	requireGauge(t, rec, clientID, 0)
	require.Equal(t, 1, logs.messageCountContaining(slog.LevelInfo, "requested subscription QoS again"),
		"the recovery is announced once, at Info")
	h = s.Health(ctx)
	require.Equal(t, ports.ServiceLevelFull, h.ServiceLevel)
	require.Empty(t, h.BestEffortTopics)
	publishOnce(t, broker.URL(), downgradeTopic, 1, false, "recovered-at-qos1")
	require.Equal(t, brokerMessage{payload: "recovered-at-qos1", qos: 1},
		wait.RequireReceive(t, received, brokerWait))
	requireRunning("after the recovery")

	// Stopping the manager is the only way Run ends: never as unrecoverable.
	cancel()
	err := wait.RequireReceive(t, runErr, 5*time.Second)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errors.Is(err, runtimesession.ErrSessionUnrecoverable))
}
