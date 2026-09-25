//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	amqp091adapter "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	amqp10adapter "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

const (
	// isolationRestartTimeout bounds the wait for route A's first supervisor
	// restart. An AMQP 0-9-1 receiver first retries a missing queue inside
	// its own Run (the reconnect-race budget, ~25-40s with jitter) and only
	// then returns the error that the supervisor restarts.
	isolationRestartTimeout = 90 * time.Second
	// isolationRecoveryTimeout bounds route A's recovery once its queue is
	// back: at most one in-Run retry interval or one supervisor backoff
	// (capped at 30s) plus delivery.
	isolationRecoveryTimeout = 2 * time.Minute
	// isolationReentryWindow outlasts the supervisor's first backoff (1s
	// with equal jitter), so route A's Run has been re-entered within it.
	isolationReentryWindow   = 5 * time.Second
	isolationDeliveryTimeout = 30 * time.Second
	isolationBurst           = 5
)

// TestGap_DeletedSourceQueueRestartsOnlyItsRoute runs ONE runtime with two
// routes on different transports — route A consumes a RabbitMQ queue, route
// B an Artemis address — each delivering to its own Artemis sink. Deleting
// route A's queue must restart route A through its supervisor, in isolation:
//
//   - route A is restarted by the supervisor (RouteRestarts tagged with its
//     route_id), not only retried inside the receiver's Run;
//   - the runtime stays healthy and never goes terminal, so liveness stays
//     green, and route B is never restarted;
//   - every message published to route B during the outage is delivered;
//   - once the queue is recreated, route A recovers in place — the same
//     receiver instance runs again — and delivers what is published to it.
//
// Mutation check: re-add the closed-receiver latch in RouteRunner.Run (a Run
// that follows a Close returns route.ErrRouteTerminal) — route A's first
// restart then makes the runtime terminal, stopping route B too, and route A
// never recovers.
func TestGap_DeletedSourceQueueRestartsOnlyItsRoute(t *testing.T) {
	const (
		routeA     = "isolation-rabbitmq-route"
		routeB     = "isolation-artemis-route"
		routingKey = "isolation"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	exchange := rabbitmqlocal.UniqueExchange("isolation-ex")
	queueA := rabbitmqlocal.UniqueQueue("isolation-a")
	rabbitmqlocal.CreateExchange(t, exchange, "direct")
	rabbitmqlocal.CreateQueue(t, queueA)
	rabbitmqlocal.BindQueue(t, queueA, exchange, routingKey)
	addrB := artemislocal.UniqueAddress("isolation-b")

	// The sinks are read by collectors attached before the runtime starts.
	sinkA := artemislocal.UniqueAddress("isolation-sink-a")
	sinkB := artemislocal.UniqueAddress("isolation-sink-b")
	collectA := newArtemisCollector(t, sinkA)
	collectB := newArtemisCollector(t, sinkB)

	// Route receivers come from the adapters' factories, the path the bridge
	// builds every route with (AMQP 0-9-1 in its managed, hand-off mode).
	rmqFactory := amqp091adapter.NewFactory(testLogger(t))
	rxA, err := rmqFactory.NewReceiver(ctx, ports.ReceiverSpec{
		ID:     routeA,
		Config: &amqp091adapter.Config{Receiver: amqp091adapter.ReceiverParams{QueueName: queueA}},
	}, setupRabbitMQSession(t, connectivity.SessionEphemeral))
	require.NoError(t, err, "route A receiver")
	artFactory := amqp10adapter.NewFactory(testLogger(t))
	rxB, err := artFactory.NewReceiver(ctx, ports.ReceiverSpec{
		ID:     routeB,
		Config: &amqp10adapter.Config{Receiver: amqp10adapter.ReceiverParams{Address: addrB, LinkCredit: 10}},
	}, setupArtemisSession(t, connectivity.SessionEphemeral))
	require.NoError(t, err, "route B receiver")
	sinkSess := setupArtemisSession(t, connectivity.SessionEphemeral)

	dlq := &lrDLQStore{}
	metrics := &ports.RecordingExporter{}
	rt := goruntime.New(
		goruntime.WithInstanceID("gap-route-isolation"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
		goruntime.WithMetrics(metrics),
	)
	addRoute := func(id, sink string, rx ports.Receiver, caps []ports.Capability) {
		require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
			ID: id,
			Policy: routing.RoutePolicy{
				DeliveryMode: routing.DeliveryDirectHold,
				DispatchMode: routing.DispatchSingle,
			},
			Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: id + "-sink", Address: sink}),
			SourceCapabilities: caps,
		}, rx, newArtemisSender(t, sinkSess, sink), nil, nil), "AddRoute %s", id)
	}
	addRoute(routeA, sinkA, rxA, rmqFactory.Capabilities())
	addRoute(routeB, sinkB, rxB, artFactory.Capabilities())

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = rt.Stop(stopCtx)
	})
	gobridgesync(t, 30*time.Second, rt)

	pubA := newRabbitMQSender(t, setupRabbitMQSession(t, connectivity.SessionEphemeral), exchange, routingKey)
	pubB := newArtemisSender(t, setupArtemisSession(t, connectivity.SessionEphemeral), addrB)

	// Warm up: each route delivers.
	sendToRabbitMQ(t, pubA, 1, "iso-a-warm")
	sendToArtemis(t, pubB, 1, "iso-b-warm")
	wantA := seqIDs("iso-a-warm", 1)
	wantB := seqIDs("iso-b-warm", 1)
	requireDelivered(t, collectA, wantA, isolationDeliveryTimeout, "route A warm-up")
	requireDelivered(t, collectB, wantB, isolationDeliveryTimeout, "route B warm-up")

	// Outage: route A's source queue disappears.
	deleted := clock.System.Now()
	deleteRabbitMQQueue(t, queueA)
	sendToArtemis(t, pubB, isolationBurst, "iso-b-outage-early")
	wantB = append(wantB, seqIDs("iso-b-outage-early", isolationBurst)...)

	lrWaitFor(t, isolationRestartTimeout, "route A restarted by its supervisor", func() bool {
		return routeRestarts(metrics, routeA) > 0
	})
	t.Logf("route A first supervisor restart %s after its queue was deleted",
		clock.System.Since(deleted).Round(time.Second))
	// The supervisor re-runs route A once its first backoff (under 1s)
	// elapses; the runtime must stay live across that re-entry.
	if wait.Poll(isolationReentryWindow, rt.Terminal) {
		t.Fatalf("runtime went terminal when the supervisor re-ran route A: %v", rt.ComponentErrors())
	}
	assertLive(t, rt, "after route A restarted")

	sendToArtemis(t, pubB, isolationBurst, "iso-b-outage-late")
	wantB = append(wantB, seqIDs("iso-b-outage-late", isolationBurst)...)
	requireDelivered(t, collectB, wantB, isolationDeliveryTimeout, "route B during route A's outage")
	assert.Zero(t, routeRestarts(metrics, routeB), "route B must not restart while route A is down")

	// Recovery: the queue comes back and route A resumes on the same receiver.
	rabbitmqlocal.CreateQueue(t, queueA)
	rabbitmqlocal.BindQueue(t, queueA, exchange, routingKey)
	recreated := clock.System.Now()
	sendToRabbitMQ(t, pubA, isolationBurst, "iso-a-after")
	wantA = append(wantA, seqIDs("iso-a-after", isolationBurst)...)
	requireDelivered(t, collectA, wantA, isolationRecoveryTimeout, "route A after its queue was recreated")
	t.Logf("route A delivered again %s after its queue was recreated (restarts=%d)",
		clock.System.Since(recreated).Round(time.Second), routeRestarts(metrics, routeA))

	// Zero loss on both routes; nothing dead-lettered; runtime still live.
	assert.Empty(t, missingIDs(collectA, wantA), "route A lost messages")
	assert.Empty(t, missingIDs(collectB, wantB), "route B lost messages")
	assert.Zero(t, dlq.count(), "DLQ must stay empty")
	assert.Zero(t, routeRestarts(metrics, routeB), "route B must never restart")
	assertLive(t, rt, "after route A recovered")
	t.Logf("route A: delivered=%d unique=%d; route B: delivered=%d unique=%d",
		collectA.count(), countUniqueAMQP(collectA), collectB.count(), countUniqueAMQP(collectB))
}

// assertLive requires the runtime to be running, healthy and not terminal:
// the state a liveness probe reads.
func assertLive(t *testing.T, rt *goruntime.Runtime, when string) {
	t.Helper()
	require.False(t, rt.Terminal(), "runtime must not be terminal %s (component errors: %v)", when, rt.ComponentErrors())
	require.True(t, rt.Healthy(), "runtime must stay healthy %s", when)
	require.True(t, rt.IsRunning(), "runtime must keep running %s", when)
}

// routeRestarts sums the supervisor's RouteRestarts counter for routeID.
func routeRestarts(rec *ports.RecordingExporter, routeID string) int64 {
	var n int64
	for _, e := range rec.FindEntries(shared.MetricRouteRestarts) {
		if slices.Contains(e.Tags, shared.Tag{Key: shared.TagKeyRouteID, Value: routeID}) {
			n += e.IValue
		}
	}
	return n
}

// seqIDs lists the envelope IDs sendToRabbitMQ and sendToArtemis assign.
func seqIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return ids
}

// missingIDs returns the ids the collector has not received.
func missingIDs(c *amqpCollector, ids []string) []string {
	got := make(map[string]bool)
	for _, env := range c.getMessages() {
		got[env.ID()] = true
	}
	var missing []string
	for _, id := range ids {
		if !got[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

func requireDelivered(t *testing.T, c *amqpCollector, ids []string, timeout time.Duration, desc string) {
	t.Helper()
	lrWaitFor(t, timeout, desc, func() bool { return len(missingIDs(c, ids)) == 0 })
}

// deleteRabbitMQQueue deletes a queue through the management API with the
// credentials of the broker endpoint.
func deleteRabbitMQQueue(t *testing.T, name string) {
	t.Helper()
	ep, err := url.Parse(rabbitmqlocal.Endpoint(t))
	require.NoError(t, err, "parse RabbitMQ endpoint")
	pass, _ := ep.User.Password()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete,
		fmt.Sprintf("%s/api/queues/%%2F/%s", rabbitmqlocal.ManagementURL(t), url.PathEscape(name)), nil)
	require.NoError(t, err, "delete queue request")
	req.SetBasicAuth(ep.User.Username(), pass)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "delete queue %s", name)
	defer func() { _ = resp.Body.Close() }()
	require.Less(t, resp.StatusCode, 300, "delete queue %s: HTTP %d", name, resp.StatusCode)
}
