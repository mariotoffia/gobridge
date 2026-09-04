//go:build longrunning

package longrunning_test

import (
	"context"
	"errors"
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
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
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
	// The AWS emulator serves every AWS API this suite touches, not just SQS,
	// so it gets the same 2g the Make target hands it. The default has to
	// match, because running the package directly is a documented path and an
	// out-of-memory emulator does not fail loudly here — the helper simply
	// restarts it, silently discarding every queue mid-test.
	awsMem := envOr("GOBRIDGE_AWS_EMULATOR_MEMORY", "2g")
	awsCPU := envOr("GOBRIDGE_AWS_EMULATOR_CPUS", "3.0")
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
	flocilocal.Configure(
		flocilocal.WithMemory(awsMem),
		flocilocal.WithCPUs(awsCPU),
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

// lrSourceVisibilityTimeout is the visibility timeout every source queue in
// this suite is created with. It is not just a queue setting: it is the
// RECOVERY INTERVAL. When a visibility extension fails — the store blinked, the
// emulator stopped answering — the message stays invisible until this elapses
// and SQS redelivers it. Any test that waits for progress must therefore
// tolerate a gap at least this long, or it gives up exactly when the recovery
// it is testing would have happened.
const lrSourceVisibilityTimeout = 30 * time.Second

func setupSQSQueue(t *testing.T, prefix string) (string, *awssqs.Client) {
	t.Helper()
	client := newSQSClient(t)
	name := uniqueQueueName(prefix)
	queueURL := createSQSQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": fmt.Sprintf("%d", int(lrSourceVisibilityTimeout.Seconds())),
	})
	return queueURL, client
}

func newSQSReceiver(t *testing.T, queueURL string) *sqsadapter.Receiver {
	t.Helper()
	r, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Client:            newSQSClient(t),
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
		Client:   newSQSClient(t),
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
	waitConnected(t, sess, 15*time.Second)

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
	// received counts every delivery, including those a counting collector
	// does not retain, so len(messages) is not the count.
	received int
	// countOnly drops the envelope after counting it. The zero value RETAINS,
	// deliberately: a collector built by a struct literal somewhere else in the
	// suite must keep the behaviour it had before this field existed.
	countOnly bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// record accounts for one delivery and returns the running total. It is the
// ONLY writer of received and messages: a handler that appended directly would
// leave count() reading zero forever while getMessages() filled up, which is
// exactly the drift this method exists to prevent.
func (c *mqttCollector) record(env *messaging.Envelope) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received++
	if !c.countOnly {
		c.messages = append(c.messages, env)
	}
	return c.received
}

// newMQTTCollector collects and RETAINS every delivered envelope, so a test can
// assert on their contents through getMessages.
//
// Do not use it in a test that only counts deliveries and runs for a long time:
// retaining hundreds of thousands of envelopes is hundreds of megabytes of the
// TEST's own heap, which a leak detector then reads as the bridge leaking. Use
// newCountingMQTTCollector there.
func newMQTTCollector(
	t *testing.T, topic string, clientIDPrefix string,
) *mqttCollector {
	t.Helper()
	return newCollector(t, topic, clientIDPrefix, false)
}

// newCountingMQTTCollector counts deliveries without retaining them. It is what
// a soak or volume test needs: the same subscription and settlement path, and a
// heap that stays flat so a real leak in the bridge is visible against it.
// getMessages returns nothing for a counting collector.
func newCountingMQTTCollector(
	t *testing.T, topic string, clientIDPrefix string,
) *mqttCollector {
	t.Helper()
	return newCollector(t, topic, clientIDPrefix, true)
}

func newCollector(
	t *testing.T, topic string, clientIDPrefix string, countOnly bool,
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
	waitConnected(t, sess, 15*time.Second)

	recv := paho.NewReceiver("collector-"+clientID, sess)
	recvCtx, recvCancel := context.WithCancel(ctx)

	c := &mqttCollector{cancel: recvCancel, countOnly: countOnly}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		err := recv.Run(recvCtx, func(ctx context.Context, del ports.Delivery) error {
			n := c.record(del.Envelope())
			if n <= 5 || n%500 == 0 {
				t.Logf("collector: received msg #%d id=%s", n, del.Envelope().ID())
			}
			return del.Ack(ctx)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("collector Receiver.Run: %v", err)
		}
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
	return c.received
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
	cfg.LeaseTTL = 5 * time.Second
	cfg.RenewInterval = 400 * time.Millisecond
	cfg.RenewJitter = 50 * time.Millisecond
	cfg.RenewCallTimeout = time.Second
	cfg.StepDownGrace = 500 * time.Millisecond
	cfg.DrainStrategy = persistence.NewFixedPoll(200 * time.Millisecond)
	cfg.DrainBatchSize = 100
	return cfg
}

// lrLoadSurvivingSessionConfig is the lease profile for a test that puts the
// STORE under sustained load — thousands of messages through a shared outbox,
// or several instances competing at once.
//
// lrSessionConfig above compresses lease timing to make failover fast to
// observe, and that compression bounds a single renew call at one second. Under
// sustained outbox write load the store misses that bound; three consecutive
// misses step the owner down and cancel every delivery in flight, so the test
// fails for a reason that has nothing to do with what it is proving. The
// published high-availability preset tolerates it (3 s per renew call, 45 s
// TTL) and is what a deployment under that load would actually run.
//
// It costs nothing where handover is GRACEFUL: a released lease is acquired at
// once, without waiting out the TTL. Use lrSessionConfig where the point IS the
// wait — an ungraceful death with no release behind it.
func lrLoadSurvivingSessionConfig(sessionID string) session.Config {
	cfg := session.HAConfig(sessionID, true)
	cfg.DrainStrategy = persistence.NewFixedPoll(200 * time.Millisecond)
	return cfg
}

func lrThroughputSessionConfig(sessionID string) session.Config {
	// Throughput tests isolate sustained delivery from compressed failover timing.
	cfg := session.DefaultConfig(sessionID, true)
	cfg.DrainStrategy = persistence.NewFixedPoll(200 * time.Millisecond)
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

// lrWaitForProgress waits until counter reaches want, and gives up only when
// the count STOPS MOVING for stallWindow — not when a wall clock runs out.
//
// Use it for a progress gate: "wait until enough has flowed, then kill an
// instance". A wall-clock deadline there is an undeclared throughput
// assertion. On a slow machine it fails a test whose actual claim (no message
// lost across a restart) is still perfectly provable, just later. Throughput is
// asserted by the tests written for it — UC63, UC65, UC80, UC81 — not by a
// barrier that only meant "get going before I continue".
//
// A stalled pipeline still fails, and fails FASTER and more usefully than the
// wall clock did: the message says how far it got and that nothing moved for
// stallWindow, instead of "condition not met in 60s" whether the count was
// climbing steadily or frozen at zero.
//
// ceiling bounds a pipeline that creeps forever without ever stalling, so a
// pathological run cannot hold the suite open indefinitely.
func lrWaitForProgress(
	t *testing.T, desc string, counter func() int, want int,
	stallWindow, ceiling time.Duration,
) {
	t.Helper()
	started := clock.System.Now()
	for {
		got := counter()
		if got >= want {
			return
		}
		if elapsed := clock.System.Since(started); elapsed > ceiling {
			t.Fatalf("%s: reached %d/%d in %s; still progressing but past the %s ceiling",
				desc, got, want, elapsed.Round(time.Second), ceiling)
		}
		if !wait.Poll(stallWindow, func() bool { return counter() > got }) {
			t.Fatalf("%s: stalled at %d/%d — nothing delivered for %s",
				desc, got, want, stallWindow)
		}
	}
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
	sendBulkRangeToSQS(t, client, queueURL, 0, count, headersFn)
}

func sendBulkRangeToSQS(
	t *testing.T,
	client *awssqs.Client,
	queueURL string,
	start, count int,
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
			seq := start + j
			id := fmt.Sprintf("msg-%d", seq)
			body := fmt.Sprintf(`{"seq":%d}`, seq)
			entry := sqstypes.SendMessageBatchRequestEntry{
				Id:          &id,
				MessageBody: &body,
			}
			if headersFn != nil {
				hdrs := headersFn(seq)
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
		require.NoError(t, err, "SendMessageBatch offset=%d", start+i)
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
		if e.ID() == id {
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
