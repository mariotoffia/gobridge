//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	amqp091adapter "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// =========================================================================
// UC92: Broker Kill/Restart with RabbitMQ (SharedOutbox)
//
// SQS-IN -> [Bridge (SharedOutbox)] -> RabbitMQ exchange -> queue
//                                                            |
//                                                       Collector
//
// After ~300 messages are collected, the RabbitMQ Docker container is
// killed and restarted (docker kill + docker start, preserving port
// mappings). The exchange, queue, and binding are non-durable and must
// be re-created after restart.
//
// SharedOutbox guarantees: the DynamoDB outbox retains undelivered
// records and the drainer retries them once the AMQP session
// auto-reconnects to the restarted broker.
//
// Validates:
//   - Session auto-reconnects after broker restart
//   - SharedOutbox retries all undelivered outbox records
//   - Total unique messages across pre- and post-restart >= 1,000
//   - DLQ remains empty
//
// Topology:
//   SQS-IN -> [Bridge (SharedOutbox)] -> RabbitMQ (kill/restart)
//                                             |
//                                        Collector(s)
// =========================================================================

const (
	uc92MsgCount = 1000
	uc92KillAt   = 300
)

func TestUC92_RabbitMQ_BrokerKillRestart(t *testing.T) {
	_ = withFreshInfra(t)

	// --- RabbitMQ infrastructure ---
	exchangeName := rabbitmqlocal.UniqueExchange("uc92-ex")
	queueName := rabbitmqlocal.UniqueQueue("uc92-q")
	routingKey := "uc92"

	rabbitmqlocal.CreateExchange(t, exchangeName, "direct")
	rabbitmqlocal.CreateQueue(t, queueName)
	rabbitmqlocal.BindQueue(t, queueName, exchangeName, routingKey)

	// --- SQS + DynamoDB (restart-tuned stale claim) ---
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc92-in")
	leaseStore, outboxStore := setupDynamoStoresForRestart(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// AMQP session with short heartbeat for fast disconnect detection.
	sessionID := uniqueID("uc92-sess")
	amqpSess := amqp091adapter.NewSession(amqp091adapter.SessionOptions{
		BrokerURL:      rabbitmqlocal.Endpoint(t),
		Heartbeat:      5 * time.Second,
		ConnectTimeout: 30 * time.Second,
	}, domain.SessionExclusive, testLogger(t))
	t.Cleanup(func() { _ = amqpSess.Close(context.Background()) })

	amqpSnd := newRabbitMQSender(t, amqpSess, exchangeName, routingKey)
	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)

	// --- Bridge runtime ---
	rt := goruntime.New(
		goruntime.WithInstanceID("uc92-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc92-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc92-bind", Address: exchangeName},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc92-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, amqpSnd, amqpSess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 15*time.Second, rt)

	// --- Pre-kill collector ---
	collector := newRabbitMQCollector(t, queueName)

	// --- Inject messages into SQS ---
	t.Logf("UC92: sending %d messages to SQS-IN", uc92MsgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, uc92MsgCount, nil)

	// --- Wait until collector has ~killAt messages ---
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector >= %d before kill", uc92KillAt),
		func() bool { return collector.count() >= uc92KillAt })

	beforeKill := collector.count()
	t.Logf("UC92: killing broker at collector=%d", beforeKill)

	// --- Kill RabbitMQ container ---
	containerName := findContainerByPrefix(t, "gobridge-rabbitmq-")
	dockerKill(t, containerName)

	time.Sleep(5 * time.Second) // OTHER: scenario timing — keep broker down before restart

	// --- Restart the SAME container (preserves port mappings) ---
	t.Log("UC92: restarting RabbitMQ container")
	restartOut, err := exec.Command("docker", "start", containerName).CombinedOutput()
	require.NoError(t, err, "docker start %s: %s", containerName, restartOut)

	// Wait for the management API to become responsive.
	// RabbitMQ cold-start requires booting Erlang + all plugins.
	waitForRabbitMQManagement(t, 90*time.Second)

	// Re-create non-durable exchange, queue, and binding on the fresh broker.
	rabbitmqlocal.CreateExchange(t, exchangeName, "direct")
	rabbitmqlocal.CreateQueue(t, queueName)
	rabbitmqlocal.BindQueue(t, queueName, exchangeName, routingKey)

	// New collector on the restarted broker (old collector's session
	// may or may not auto-recover; a second collector guarantees capture).
	collector2 := newRabbitMQCollector(t, queueName)

	// --- Wait for remaining messages after recovery ---
	lrWaitFor(t, 180*time.Second,
		fmt.Sprintf("total unique >= %d after restart", uc92MsgCount),
		func() bool {
			return mergedUniqueAMQPCount(collector, collector2) >= uc92MsgCount
		})

	total := mergedUniqueAMQPCount(collector, collector2)
	t.Logf("UC92: total unique=%d (pre-kill=%d, collector2=%d), dlq=%d",
		total, beforeKill, collector2.count(), dlq.count())

	require.GreaterOrEqual(t, total, uc92MsgCount,
		"SharedOutbox must deliver >= %d messages after broker restart", uc92MsgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

// ---------------------------------------------------------------------------
// Helpers shared by UC92 and UC93
// ---------------------------------------------------------------------------

// findContainerByPrefix returns the name of the first running Docker
// container whose name matches the given prefix.
func findContainerByPrefix(t *testing.T, prefix string) string {
	t.Helper()
	out, err := exec.Command(
		"docker", "ps", "--format", "{{.Names}}", "--filter", "name="+prefix,
	).Output()
	require.NoError(t, err, "docker ps for prefix %q", prefix)

	name := strings.TrimSpace(string(out))
	require.NotEmpty(t, name, "no running container with prefix %q", prefix)

	if idx := strings.Index(name, "\n"); idx >= 0 {
		name = name[:idx]
	}
	return name
}

// waitForRabbitMQManagement polls the RabbitMQ management API health
// endpoint (with basic auth) until it responds with HTTP 200 and
// status=ok, confirming the broker is ready for queue/exchange declarations.
func waitForRabbitMQManagement(t *testing.T, timeout time.Duration) {
	t.Helper()
	mgmt := rabbitmqlocal.ManagementURL(t)
	lrWaitFor(t, timeout, "RabbitMQ management API ready", func() bool {
		req, err := http.NewRequest(http.MethodGet, mgmt+"/api/healthchecks/node", nil)
		if err != nil {
			return false
		}
		req.SetBasicAuth("guest", "guest")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	})
}

// mergedUniqueAMQPCount returns the number of unique envelope IDs across
// two AMQP collectors. Used in broker resilience tests where a pre-kill
// and post-restart collector capture different portions of the stream.
func mergedUniqueAMQPCount(collectors ...*amqpCollector) int {
	seen := make(map[string]struct{})
	for _, c := range collectors {
		for _, m := range c.getMessages() {
			seen[m.ID] = struct{}{}
		}
	}
	return len(seen)
}
