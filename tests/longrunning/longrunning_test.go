//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	dblease "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	dboutbox "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ---------------------------------------------------------------------------
// TestMain — Configure test infrastructure
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	// Container resource limits — tunable via env vars for different machines.
	// Defaults are generous enough for 5+ concurrent bridges.
	mqttMem := envOr("GOBRIDGE_MQTT_MEMORY", "256m")
	mqttCPU := envOr("GOBRIDGE_MQTT_CPUS", "3.0")
	sqsMem := envOr("GOBRIDGE_SQS_MEMORY", "512m")
	sqsCPU := envOr("GOBRIDGE_SQS_CPUS", "3.0")
	ddbMem := envOr("GOBRIDGE_DDB_MEMORY", "512m")
	ddbCPU := envOr("GOBRIDGE_DDB_CPUS", "3.0")

	mqttlocal.Configure(
		mqttlocal.WithPersistence(true),
		mqttlocal.WithMaxInflightMessages(65534), // Mosquitto treats 65535 as 0 (default 20)
		mqttlocal.WithMaxQueuedMessages(65534),
		mqttlocal.WithMaxQueuedBytes(0),
		mqttlocal.WithMemory(mqttMem),
		mqttlocal.WithCPUs(mqttCPU),
	)
	ddblocal.Configure(
		ddblocal.WithMemory(ddbMem),
		ddblocal.WithCPUs(ddbCPU),
	)
	sqslocal.Configure(
		sqslocal.WithMemory(sqsMem),
		sqslocal.WithCPUs(sqsCPU),
	)
	rabbitmqlocal.Configure(
		rabbitmqlocal.WithCleanOrphans(true),
	)
	artemislocal.Configure(
		artemislocal.WithCleanOrphans(true),
	)

	code := m.Run()

	rabbitmqlocal.Shutdown()
	artemislocal.Shutdown()

	os.Exit(code)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// testLogger returns a structured logger for test diagnostics.
// Control via GOBRIDGE_LOG_LEVEL env var:
//
//	trace — per-message flow tracing (very verbose, use for pipeline debugging)
//	debug — component lifecycle, batch operations, resolver decisions
//	info  — operational events (bridge start/stop, delivery counts)
//	warn  — only warnings (default — keeps output clean during normal runs)
//
// Usage in tests:
//
//	log := testLogger(t)
//	rt := goruntime.New(goruntime.WithLogger(log), ...)
//	if isTrace() { t.Logf("msg flow: %v", someExpensiveValue) }
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	level := testLogLevel()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				if lvl, ok := a.Value.Any().(slog.Level); ok && lvl <= slog.Level(-8) {
					a.Value = slog.StringValue("TRACE")
				}
			}
			return a
		},
	}))
}

func testLogLevel() slog.Level {
	switch os.Getenv("GOBRIDGE_LOG_LEVEL") {
	case "trace":
		return slog.Level(-8) // LevelTrace from bridge/logging
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	default:
		return slog.LevelWarn
	}
}

// isTrace returns true when GOBRIDGE_LOG_LEVEL=trace. Use to guard
// expensive diagnostic operations (e.g., dumping per-message details).
func isTrace() bool { return testLogLevel() <= slog.Level(-8) }

// isDebug returns true when GOBRIDGE_LOG_LEVEL=trace or debug.
func isDebug() bool { return testLogLevel() <= slog.LevelDebug }

// isInfo returns true when GOBRIDGE_LOG_LEVEL is trace, debug, or info.
func isInfo() bool { return testLogLevel() <= slog.LevelInfo }

// ---------------------------------------------------------------------------
// SQS helpers
// ---------------------------------------------------------------------------

func setupSQSQueue(t *testing.T, prefix string) (string, *awssqs.Client) {
	t.Helper()
	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue(prefix)
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "30",
	})
	return queueURL, client
}

func newSQSReceiver(t *testing.T, queueURL string) *sqsadapter.Receiver {
	t.Helper()
	r, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Client:            sqslocal.Client(t),
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 30,
	}, testLogger(t))
	require.NoError(t, err, "newSQSReceiver")
	return r
}

func newSQSSender(t *testing.T, queueURL string) *sqsadapter.Sender {
	t.Helper()
	s, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueURL: queueURL,
		Client:   sqslocal.Client(t),
		Timeout:  10 * time.Second,
		Logger:   testLogger(t),
	})
	require.NoError(t, err, "newSQSSender")
	return s
}

// ---------------------------------------------------------------------------
// MQTT helpers
// ---------------------------------------------------------------------------

func setupMQTTSession(
	t *testing.T, clientID string, mode connectivity.SessionMode,
) *paho.Session {
	t.Helper()
	url := mqttlocal.BrokerURL(t)
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       clientID,
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     true,
		ReceiveMaximum: 65534, // max messages broker can send TO this client concurrently
	}, mode, testLogger(t))

	ctx := context.Background()
	require.NoError(t, sess.Start(ctx), "MQTT session Start %q", clientID)

	select {
	case <-sess.Events():
	case <-time.After(5 * time.Second):
	}

	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

func setupMQTTSender(t *testing.T, sess *paho.Session) *paho.Sender {
	t.Helper()
	return paho.NewSender(sess, paho.SenderOptions{
		QoS:     1,
		Timeout: 10 * time.Second,
	})
}

// ---------------------------------------------------------------------------
// mqttCollector — subscribes and collects envelopes
// ---------------------------------------------------------------------------

type mqttCollector struct {
	mu       sync.Mutex
	messages []*messaging.Envelope
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newMQTTCollector(
	t *testing.T, topic string, clientIDPrefix string,
) *mqttCollector {
	t.Helper()
	url := mqttlocal.BrokerURL(t)
	clientID := mqttlocal.UniqueClientID(clientIDPrefix)

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       clientID,
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     true,
		ReceiveMaximum: 65534,
	}, connectivity.SessionEphemeral, testLogger(t))

	ctx := context.Background()
	require.NoError(t, sess.Start(ctx), "collector Start")

	select {
	case <-sess.Events():
	case <-time.After(5 * time.Second):
	}

	recv := paho.NewReceiver("collector-"+clientID, sess)
	recvCtx, recvCancel := context.WithCancel(ctx)

	c := &mqttCollector{cancel: recvCancel}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			c.mu.Lock()
			c.messages = append(c.messages, del.Envelope())
			n := len(c.messages)
			c.mu.Unlock()
			if n <= 5 || n%500 == 0 {
				t.Logf("collector: received msg #%d id=%s", n, del.Envelope().ID)
			}
			return nil
		})
	}()

	require.NoError(t, sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}), "collector Reconcile")
	waitSubReady(t, sess, 5*time.Second)

	t.Cleanup(func() {
		recvCancel()
		c.wg.Wait()
		_ = sess.Close(context.Background())
	})

	return c
}

func (c *mqttCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

func (c *mqttCollector) getMessages() []*messaging.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]*messaging.Envelope, len(c.messages))
	copy(cp, c.messages)
	return cp
}

// ---------------------------------------------------------------------------
// DynamoDB helpers
// ---------------------------------------------------------------------------

func setupDynamoStores(t *testing.T) (ports.LeaseStore, ports.OutboxStore) {
	t.Helper()
	client := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("lr-leases")
	outboxTable := ddblocal.UniqueTable("lr-outbox")

	leaseStore := dblease.NewStore(client,
		dblease.WithTableName(leaseTable),
		dblease.WithGracePeriod(10*time.Second),
	)
	require.NoError(t, leaseStore.EnsureTable(context.Background()), "lease table")
	ddblocal.CleanupTable(t, client, leaseTable)

	outboxStore := dboutbox.NewStore(client,
		dboutbox.WithTableName(outboxTable),
		dboutbox.WithStaleClaimDuration(1*time.Second),
	)
	require.NoError(t, outboxStore.CreateTable(context.Background()), "outbox table")
	ddblocal.CleanupTable(t, client, outboxTable)

	return leaseStore, outboxStore
}

// setupDynamoStoresForRestart returns stores configured for tests that
// involve broker kill/restart. The longer StaleClaimDuration (15s) prevents
// the outbox drainer from rapidly re-claiming records during broker downtime,
// which would otherwise exhaust MaxReplayAttempts and orphan records.
func setupDynamoStoresForRestart(t *testing.T) (ports.LeaseStore, ports.OutboxStore) {
	t.Helper()
	client := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("lr-leases")
	outboxTable := ddblocal.UniqueTable("lr-outbox")

	leaseStore := dblease.NewStore(client,
		dblease.WithTableName(leaseTable),
		dblease.WithGracePeriod(10*time.Second),
	)
	require.NoError(t, leaseStore.EnsureTable(context.Background()), "lease table")
	ddblocal.CleanupTable(t, client, leaseTable)

	outboxStore := dboutbox.NewStore(client,
		dboutbox.WithTableName(outboxTable),
		dboutbox.WithStaleClaimDuration(15*time.Second),
	)
	require.NoError(t, outboxStore.CreateTable(context.Background()), "outbox table")
	ddblocal.CleanupTable(t, client, outboxTable)

	return leaseStore, outboxStore
}

// ---------------------------------------------------------------------------
// Runtime / session config helpers
// ---------------------------------------------------------------------------

func lrSessionConfig(sessionID string) session.Config {
	cfg := session.DefaultConfig(sessionID, true)
	cfg.LeaseTTL = 2 * time.Second
	cfg.RenewInterval = 400 * time.Millisecond
	cfg.RenewJitter = 50 * time.Millisecond
	cfg.StepDownGrace = 500 * time.Millisecond
	cfg.DrainStrategy = persistence.NewFixedPoll(200 * time.Millisecond)
	cfg.DrainBatchSize = 100
	return cfg
}

// ---------------------------------------------------------------------------
// Polling / wait helpers
// ---------------------------------------------------------------------------

func lrWaitFor(
	t *testing.T, timeout time.Duration, desc string, fn func() bool,
) {
	t.Helper()
	wait.Until(t, timeout, desc, fn)
}

// ---------------------------------------------------------------------------
// Bulk SQS helpers
// ---------------------------------------------------------------------------

func sendBulkToSQS(
	t *testing.T,
	client *awssqs.Client,
	queueURL string,
	count int,
	headersFn func(i int) map[string]string,
) {
	t.Helper()
	const batchSize = 10
	for i := 0; i < count; i += batchSize {
		end := i + batchSize
		if end > count {
			end = count
		}
		entries := make([]sqstypes.SendMessageBatchRequestEntry, 0, end-i)
		for j := i; j < end; j++ {
			id := fmt.Sprintf("msg-%d", j)
			body := fmt.Sprintf(`{"seq":%d}`, j)
			entry := sqstypes.SendMessageBatchRequestEntry{
				Id:          &id,
				MessageBody: &body,
			}
			if headersFn != nil {
				hdrs := headersFn(j)
				attrs := make(map[string]sqstypes.MessageAttributeValue, len(hdrs))
				for k, v := range hdrs {
					attrs[k] = sqstypes.MessageAttributeValue{
						DataType:    strPtr("String"),
						StringValue: strPtr(v),
					}
				}
				entry.MessageAttributes = attrs
			}
			entries = append(entries, entry)
		}
		_, err := client.SendMessageBatch(context.Background(),
			&awssqs.SendMessageBatchInput{
				QueueUrl: &queueURL,
				Entries:  entries,
			})
		require.NoError(t, err, "SendMessageBatch offset=%d", i)
	}
}

func pollAllSQS(
	t *testing.T,
	client *awssqs.Client,
	queueURL string,
	expected int,
	timeout time.Duration,
) []string {
	t.Helper()
	var bodies []string
	deadline := time.Now().Add(timeout)
	for len(bodies) < expected && time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(context.Background(),
			&awssqs.ReceiveMessageInput{
				QueueUrl:            &queueURL,
				MaxNumberOfMessages: 10,
				WaitTimeSeconds:     2,
			})
		if err != nil {
			t.Logf("pollAllSQS: %v", err)
			time.Sleep(200 * time.Millisecond) // OTHER: backoff on transient SQS error
			continue
		}
		for _, msg := range out.Messages {
			bodies = append(bodies, *msg.Body)
			_, _ = client.DeleteMessage(context.Background(),
				&awssqs.DeleteMessageInput{
					QueueUrl:      &queueURL,
					ReceiptHandle: msg.ReceiptHandle,
				})
		}
	}
	return bodies
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Unique ID helpers
// ---------------------------------------------------------------------------

var lrIDCounter atomic.Uint64

func uniqueID(prefix string) string {
	n := lrIDCounter.Add(1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

func assertMessageCount(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s: message count = %d, want %d", name, got, want)
	}
}

// ---------------------------------------------------------------------------
// DLQ store
// ---------------------------------------------------------------------------

type lrDLQStore struct {
	mu      sync.Mutex
	entries []routing.DLQEntry
}

func (s *lrDLQStore) Write(_ context.Context, entry routing.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *lrDLQStore) List(_ context.Context, _ routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}
func (s *lrDLQStore) Get(_ context.Context, id string) (routing.DLQEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.ID == id {
			return e, nil
		}
	}
	return routing.DLQEntry{}, shared.ErrNotFound
}

func (s *lrDLQStore) Delete(_ context.Context, _ []string) (int, error) { return 0, nil }
func (s *lrDLQStore) DeleteByFilter(_ context.Context, _ routing.DLQFilter) (int, error) {
	return 0, nil
}
func (s *lrDLQStore) Purge(_ context.Context, _ time.Time) (int, error) { return 0, nil }

func (s *lrDLQStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *lrDLQStore) getEntries() []routing.DLQEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]routing.DLQEntry, len(s.entries))
	copy(cp, s.entries)
	return cp
}
