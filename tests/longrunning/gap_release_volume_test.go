//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	sessioncfg "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// The release gate's message-conservation proof.
//
// The other no-loss proofs move a few hundred to a few thousand messages —
// enough to show the mechanism works, not enough to fill the receive window
// often enough for an ordering, back-pressure or recovery defect that needs
// SUSTAINED concurrency to appear. A release claim of "no loss" rests on
// whichever proof exercised the least.
//
// This one derives its volume from the setting that governs concurrency rather
// than picking a round number. The receiving session's MQTT Receive Maximum is
// how many unacknowledged deliveries may be in flight at once, so the volume is
// that window refilled releaseWindowRefills times. Every refill is a fresh
// cycle of fill, drain and acknowledge — including across a broker restart in
// the middle of the stream.
//
// It runs on the PUBLISHED lease profile (session.HAConfig), not on the
// compressed timing the failover proofs use. That is not a convenience: the
// compressed profile bounds a lease-renew call at one second, which sustained
// outbox write load against the store makes it miss, so the owner steps down
// mid-stream and cancels every delivery in flight. Conservation under load has
// to be measured on a profile tuned for load, which is also the profile that
// ships.
//
// It asserts two different things, and the second is what makes the first
// meaningful:
//
//   - CONSERVATION. Every published message arrives, and the set of delivered
//     identities is exactly the set that was published — nothing lost and
//     nothing invented.
//   - DUPLICATE ACCOUNTING. Delivery is at-least-once, so duplicates are legal
//     and the gate reports how many it saw. A proof that forbade them would be
//     asserting a guarantee this bridge does not make; a proof that ignored
//     them would hide a redelivery storm behind a green tick.
// ═══════════════════════════════════════════════════════════════════════════

const (
	// releaseWindowRefills is how many times the release volume must refill the
	// session receive window. Twenty-five keeps the run bounded (minutes, not
	// hours) while making sustained-concurrency behaviour, not first-message
	// behaviour, the thing under test.
	releaseWindowRefills = 25

	releaseVolumeTopic   = "release/volume/output"
	releaseVolumeTimeout = 900 * time.Second
)

func TestGAP_ReleaseVolumeConservation(t *testing.T) {
	_ = withFreshInfra(t)

	// The exercised volume, and its derivation, are part of the evidence: a
	// reviewer must be able to see what the number came from.
	// The collector leaves receive_maximum unset, so it runs on the adapter's
	// byte-bounded default — the same window a deployment gets.
	receiveWindow := int(paho.DefaultReceiveMaximum)
	volume := receiveWindow * releaseWindowRefills
	restartAt := volume / 3
	t.Logf("release volume: %d messages = collector receive window %d x %d refills; "+
		"broker restart at %d", volume, receiveWindow, releaseWindowRefills, restartAt)

	// Broker queue limits are raised so the BRIDGE's conservation is what is
	// under test; a broker that discarded its own queue would be measuring
	// Mosquitto, not gobridge.
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithPersistence(true),
		mqttlocal.WithMaxInflightMessages(65534),
		mqttlocal.WithMaxQueuedMessages(65534),
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "release-volume-in")
	leaseStore, outboxStore := setupDynamoStoresForRestart(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), releaseVolumeTimeout)
	defer cancel()

	collector := newPersistentCollectorWithBroker(t, broker.URL(), releaseVolumeTopic, "release-volume-col")

	sessionID := mqttlocal.UniqueClientID("release-volume-session")
	// The bridge session publishes; its own inbound window must not be the
	// throttle under test, which is the collector's.
	bridgeSession := newMQTTSessionWithBroker(t, broker.URL(), sessionID,
		connectivity.SessionExclusive, 65535, 5)
	sender := setupMQTTSender(t, bridgeSession)
	receiver := newSQSReceiver(t, sqsInURL)
	sessionConfig := sessioncfg.HAConfig(sessionID, true)

	rt := goruntime.New(
		goruntime.WithInstanceID("release-volume-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "release-volume-route",
		Policy: routing.RoutePolicy{
			// The durable mode: a broker restart mid-stream must be survivable
			// from persistence, which is what a release-volume no-loss claim is
			// actually about.
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "release-volume-bind", Address: releaseVolumeTopic},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "release-volume-bind", SessionID: sessionID},
		},
	}, receiver, sender, bridgeSession, &sessionConfig))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)
	requireMQTTSessionReady(t, rt, sessionID)

	t.Logf("release volume: publishing %d messages", volume)
	sendBulkToSQS(t, sqsInClient, sqsInURL, volume, nil)

	// Restart the broker with the stream in flight: recovery under sustained
	// load is the case a small proof cannot reach.
	lrWaitFor(t, 300*time.Second, fmt.Sprintf("collector >= %d before restart", restartAt),
		func() bool { return collector.count() >= restartAt })
	t.Logf("release volume: restarting broker at collector=%d", collector.count())
	broker.StopGraceful()
	broker.RestartGraceful()
	sendProbe(t, sqsInClient, sqsInURL, collector, 60*time.Second)
	requireMQTTSessionReady(t, rt, sessionID)

	lrWaitFor(t, 600*time.Second, fmt.Sprintf("collector >= %d", volume),
		func() bool { return countUnique(collector) >= volume })

	// The outbox is a separate durability question from what the collector saw:
	// a healthy collector cannot mask records stranded in persistence.
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		pending, supported, err := rt.OutboxPending(ctx,
			persistence.OutboxPartitionKey(sessionID, ""))
		require.NoError(collect, err)
		require.True(collect, supported, "outbox store must report pending depth")
		assert.Zero(collect, pending, "outbox must drain")
	}, 300*time.Second, 500*time.Millisecond)

	total := collector.count()
	unique := countUnique(collector)
	duplicates := total - unique

	// The exercised numbers go in the report, not only in the assertion: this
	// line IS the release evidence.
	t.Logf("release volume evidence: published=%d unique=%d total=%d duplicates=%d dlq=%d "+
		"(receive window %d x %d refills, one broker restart)",
		volume, unique, total, duplicates, dlq.count(), receiveWindow, releaseWindowRefills)

	require.GreaterOrEqualf(t, unique, volume,
		"conservation: every one of the %d published messages must arrive; %d unique did", volume, unique)
	require.LessOrEqualf(t, unique, volume+1,
		"no message may be invented: %d unique delivered for %d published (+1 for the recovery probe)",
		unique, volume)
	// Duplicates are legal — delivery is at-least-once — but a STORM is not.
	// Redelivering a tenth of the stream means settlement is not keeping up
	// with the broker, which conservation alone would hide behind a green tick.
	require.Lessf(t, duplicates, volume/10,
		"redelivery storm: %d duplicates for %d published is past the tenth this bound allows",
		duplicates, volume)
	assert.Zero(t, dlq.count(), "no message may reach the DLQ on a healthy path")
}
