package integration_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

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
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ---------------------------------------------------------------------------
// SQS helpers
// ---------------------------------------------------------------------------

func setupSQSQueue(t *testing.T, prefix string) (string, *awssqs.Client) {
	t.Helper()
	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue(prefix)
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "5",
	})
	return queueURL, client
}

func newSQSReceiver(t *testing.T, queueURL string) *sqsadapter.Receiver {
	t.Helper()
	// The SQS adapter builds its own AWS client via LoadDefaultConfig.
	// Provide static credentials so tests don't depend on host AWS config.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ep := sqslocal.Endpoint(t)
	r, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
	}, slog.Default())
	if err != nil {
		t.Fatalf("newSQSReceiver: %v", err)
	}
	return r
}

func newSQSSender(t *testing.T, queueURL string) *sqsadapter.Sender {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ep := sqslocal.Endpoint(t)
	s, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueURL: queueURL,
		Endpoint: ep,
		Region:   "us-west-1",
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("newSQSSender: %v", err)
	}
	return s
}

func sendToSQS(t *testing.T, client *awssqs.Client, queueURL string, body string, headers map[string]string) {
	t.Helper()
	attrs := make(map[string]sqstypes.MessageAttributeValue, len(headers))
	for k, v := range headers {
		attrs[k] = sqstypes.MessageAttributeValue{
			DataType:    strPtr("String"),
			StringValue: strPtr(v),
		}
	}
	_, err := client.SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl:          &queueURL,
		MessageBody:       &body,
		MessageAttributes: attrs,
	})
	if err != nil {
		t.Fatalf("sendToSQS: %v", err)
	}
}

func pollSQS(t *testing.T, client *awssqs.Client, queueURL string, count int, timeout time.Duration) []string {
	t.Helper()
	var bodies []string
	deadline := time.Now().Add(timeout)
	for len(bodies) < count && time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl:            &queueURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
		})
		if err != nil {
			t.Logf("pollSQS: %v", err)
			time.Sleep(200 * time.Millisecond) // OTHER: backoff on transient SQS error
			continue
		}
		for _, msg := range out.Messages {
			bodies = append(bodies, *msg.Body)
			_, _ = client.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
				QueueUrl:      &queueURL,
				ReceiptHandle: msg.ReceiptHandle,
			})
		}
	}
	return bodies
}

func newSQSSenderFIFO(t *testing.T, queueURL, groupID string) *sqsadapter.Sender {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ep := sqslocal.Endpoint(t)
	s, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueURL:       queueURL,
		Endpoint:       ep,
		Region:         "us-west-1",
		Timeout:        5 * time.Second,
		FIFO:           true,
		MessageGroupID: groupID,
	})
	if err != nil {
		t.Fatalf("newSQSSenderFIFO: %v", err)
	}
	return s
}

func newSQSReceiverWithVisibility(t *testing.T, queueURL string, visibilityTimeout int32) *sqsadapter.Receiver {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ep := sqslocal.Endpoint(t)
	autoExtend := false
	r, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       1,
		WaitTimeSeconds:   1,
		VisibilityTimeout: visibilityTimeout,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	if err != nil {
		t.Fatalf("newSQSReceiverWithVisibility: %v", err)
	}
	return r
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// MQTT helpers
// ---------------------------------------------------------------------------

func setupMQTTSession(t *testing.T, clientID string, mode connectivity.SessionMode) *paho.Session {
	t.Helper()
	url := mqttlocal.BrokerURL(t)
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       clientID,
		KeepAlive:      10,
		ConnectTimeout: 10 * time.Second,
		CleanStart:     true,
	}, mode, nil)

	ctx := context.Background()
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("setupMQTTSession Start %q: %v", clientID, err)
	}

	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
	}

	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

func setupMQTTSender(t *testing.T, sess *paho.Session) *paho.Sender {
	t.Helper()
	return paho.NewSender(sess, paho.SenderOptions{
		QoS:     1,
		Timeout: 5 * time.Second,
	})
}

// mqttCollector subscribes to a topic on a dedicated MQTT client and
// collects received envelopes. Call stop() to cancel and wait for cleanup.
type mqttCollector struct {
	mu       sync.Mutex
	messages []*messaging.Envelope
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newMQTTCollector(t *testing.T, topic string, clientIDPrefix string) *mqttCollector {
	t.Helper()
	url := mqttlocal.BrokerURL(t)
	clientID := mqttlocal.UniqueClientID(clientIDPrefix)

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       clientID,
		KeepAlive:      10,
		ConnectTimeout: 10 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)

	ctx := context.Background()
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("collector Start: %v", err)
	}

	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
	}

	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		_ = sess.Close(ctx)
		t.Fatalf("collector Reconcile: %v", err)
	}

	// Wait for the broker to acknowledge the SUBSCRIBE. We can't wait
	// for ServiceLevelFull here because the receiver (and its handler)
	// is constructed below — Full requires HandlersRegistered > 0.
	// Subs-active is the right precondition for "safe to construct
	// receiver and not miss messages from subsequent publishes".
	wait.Until(t, 5*time.Second, "collector subscriptions active", func() bool {
		h := sess.Health(ctx)
		return h.Connected && h.SubscriptionsActive == h.SubscriptionsWanted
	})

	recv := paho.NewReceiver("collector-"+clientID, sess)
	recvCtx, recvCancel := context.WithCancel(ctx)

	c := &mqttCollector{cancel: recvCancel}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		err := recv.Run(recvCtx, func(ctx context.Context, del ports.Delivery) error {
			c.mu.Lock()
			c.messages = append(c.messages, del.Envelope())
			c.mu.Unlock()
			return del.Ack(ctx)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("collector Receiver.Run: %v", err)
		}
	}()

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

	leaseTable := ddblocal.UniqueTable("e2e-leases")
	outboxTable := ddblocal.UniqueTable("e2e-outbox")

	leaseStore := dblease.NewStore(client, dblease.WithTableName(leaseTable))
	if err := leaseStore.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure lease table: %v", err)
	}
	ddblocal.CleanupTable(t, client, leaseTable)

	outboxStore := dboutbox.NewStore(client,
		dboutbox.WithTableName(outboxTable),
		dboutbox.WithStaleClaimDuration(500*time.Millisecond),
	)
	if err := outboxStore.CreateTable(context.Background()); err != nil {
		t.Fatalf("create outbox table: %v", err)
	}
	ddblocal.CleanupTable(t, client, outboxTable)

	return leaseStore, outboxStore
}

// ---------------------------------------------------------------------------
// Runtime helpers
// ---------------------------------------------------------------------------

func e2eFastSessionConfig(sessionID string) session.Config {
	cfg := session.DefaultConfig(sessionID, true)
	cfg.LeaseTTL = 800 * time.Millisecond
	cfg.RenewInterval = 150 * time.Millisecond
	cfg.RenewJitter = 20 * time.Millisecond
	cfg.StepDownGrace = 200 * time.Millisecond
	cfg.DrainStrategy = persistence.NewFixedPoll(100 * time.Millisecond)
	cfg.DrainBatchSize = 50
	return cfg
}

func e2eWaitFor(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	wait.Until(t, timeout, desc, fn)
}

// ---------------------------------------------------------------------------
// DLQ store for E2E tests
// ---------------------------------------------------------------------------

type e2eDLQStore struct {
	mu      sync.Mutex
	entries []routing.DLQEntry
}

func (s *e2eDLQStore) Write(_ context.Context, entry routing.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *e2eDLQStore) List(_ context.Context, _ routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}

func (s *e2eDLQStore) Get(_ context.Context, id string) (routing.DLQEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.ID() == id {
			return e, nil
		}
	}
	return routing.DLQEntry{}, shared.ErrNotFound
}

func (s *e2eDLQStore) Delete(_ context.Context, _ []string) (int, error) { return 0, nil }
func (s *e2eDLQStore) DeleteByFilter(_ context.Context, _ routing.DLQFilter) (int, error) {
	return 0, nil
}
func (s *e2eDLQStore) Purge(_ context.Context, _ time.Time) (int, error) { return 0, nil }

func (s *e2eDLQStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ---------------------------------------------------------------------------
// Route config helpers
// ---------------------------------------------------------------------------

// directHoldCaps is the standard set of source capabilities required
// for direct_hold routes to pass validation.
var directHoldCaps = []ports.Capability{
	ports.CapSourceRedelivery,
	ports.CapVisibilityExtension,
}

// ---------------------------------------------------------------------------
// Unique ID helpers
// ---------------------------------------------------------------------------

var idCounter uint64
var idMu sync.Mutex

func uniqueID(prefix string) string {
	idMu.Lock()
	idCounter++
	n := idCounter
	idMu.Unlock()
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}
