//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	amqp10adapter "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// =========================================================================
// UC93: Broker Kill/Restart with Artemis (SharedOutbox)
//
// SQS-IN -> [Bridge (SharedOutbox)] -> Artemis address -> Collector
//
// After ~300 messages are collected, the Artemis Docker container is
// killed and restarted (docker kill + docker start, preserving port
// mappings). Unlike RabbitMQ, Artemis auto-creates addresses when
// sender/receiver links re-attach — no manual infrastructure re-creation
// is needed after restart.
//
// SharedOutbox guarantees: the DynamoDB outbox retains undelivered
// records and the drainer retries them once the AMQP 1.0 session
// auto-reconnects to the restarted broker.
//
// Validates:
//   - Session auto-reconnects after broker restart
//   - SharedOutbox retries all undelivered outbox records
//   - Total unique messages across pre- and post-restart >= 1,000
//   - DLQ remains empty
//
// Topology:
//   SQS-IN -> [Bridge (SharedOutbox)] -> Artemis (kill/restart)
//                                             |
//                                        Collector(s)
// =========================================================================

const (
	uc93MsgCount = 1000
	uc93KillAt   = 300
)

func TestUC93_Artemis_BrokerKillRestart(t *testing.T) {
	t.Skip("Artemis Docker container does not restart cleanly (JVM journal recovery); " +
		"requires artemislocal.BrokerInstance with fresh container support (future work)")
	_ = withFreshInfra(t)

	address := artemislocal.UniqueAddress("uc93-addr")

	// --- SQS + DynamoDB (restart-tuned stale claim) ---
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc93-in")
	leaseStore, outboxStore := setupDynamoStoresForRestart(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// AMQP 1.0 session — NOT started; the runtime manages lifecycle.
	sessionID := uniqueID("uc93-sess")
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()

	amqpSess := amqp10adapter.NewSession(amqp10adapter.SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       pass,
		ConnectTimeout: 30 * time.Second,
		IdleTimeout:    2 * time.Minute,
	}, connectivity.SessionExclusive, testLogger(t))
	t.Cleanup(func() { _ = amqpSess.Close(context.Background()) })

	amqpSnd, err := amqp10adapter.NewSender(amqp10adapter.SenderConfig{
		Address: address,
		Timeout: 30 * time.Second,
		Session: amqpSess,
	}, amqpSess)
	require.NoError(t, err, "NewArtemisSender")
	t.Cleanup(func() { _ = amqpSnd.Close(context.Background()) })

	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)

	// --- Bridge runtime ---
	rt := goruntime.New(
		goruntime.WithInstanceID("uc93-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc93-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc93-bind", Address: address},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc93-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, amqpSnd, amqpSess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 15*time.Second, rt)

	// --- Pre-kill collector ---
	collector := newArtemisCollector(t, address)

	// --- Inject messages into SQS ---
	t.Logf("UC93: sending %d messages to SQS-IN", uc93MsgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, uc93MsgCount, nil)

	// --- Wait until collector has ~killAt messages ---
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector >= %d before kill", uc93KillAt),
		func() bool { return collector.count() >= uc93KillAt })

	beforeKill := collector.count()
	t.Logf("UC93: killing broker at collector=%d", beforeKill)

	// --- Restart Artemis container (docker restart sends SIGTERM then
	// waits -t seconds before SIGKILL, then starts the container) ---
	containerName := findContainerByPrefix(t, "gobridge-artemis-")
	t.Log("UC93: restarting Artemis container via docker restart")
	restartOut, restartErr := exec.Command("docker", "restart", "-t", "5", containerName).CombinedOutput()
	require.NoError(t, restartErr, "docker restart %s: %s", containerName, restartOut)

	// Wait for the Artemis web console to become responsive.
	// Artemis cold-start requires booting JVM + all modules.
	waitForArtemisConsole(t, 120*time.Second)

	// Artemis auto-creates addresses when links attach; no manual
	// infrastructure re-creation needed. Create a new collector whose
	// link attachment triggers address creation on the restarted broker.
	collector2 := newArtemisCollector(t, address)

	// --- Wait for remaining messages after recovery ---
	lrWaitFor(t, 180*time.Second,
		fmt.Sprintf("total unique >= %d after restart", uc93MsgCount),
		func() bool {
			return mergedUniqueAMQPCount(collector, collector2) >= uc93MsgCount
		})

	total := mergedUniqueAMQPCount(collector, collector2)
	t.Logf("UC93: total unique=%d (pre-kill=%d, collector2=%d), dlq=%d",
		total, beforeKill, collector2.count(), dlq.count())

	require.GreaterOrEqual(t, total, uc93MsgCount,
		"SharedOutbox must deliver >= %d messages after broker restart", uc93MsgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

// waitForArtemisConsole polls the Artemis web console until it responds,
// confirming the broker is ready to accept AMQP connections.
func waitForArtemisConsole(t *testing.T, timeout time.Duration) {
	t.Helper()
	consoleURL := artemislocal.ConsoleURL(t)
	lrWaitFor(t, timeout, "Artemis console ready", func() bool {
		resp, err := http.Get(consoleURL)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	})
}
