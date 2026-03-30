//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Shared processors and wrapper senders for UC7-UC41.
// ═══════════════════════════════════════════════════════════════════════════

// concurrencyTracker records the maximum number of concurrent Process calls.
type concurrencyTracker struct {
	current atomic.Int64
	max     atomic.Int64
}

func (p *concurrencyTracker) Name() string { return "concurrency-tracker" }

func (p *concurrencyTracker) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	cur := p.current.Add(1)
	for {
		old := p.max.Load()
		if cur <= old || p.max.CompareAndSwap(old, cur) {
			break
		}
	}
	defer p.current.Add(-1)
	return next(ctx, env)
}

func (p *concurrencyTracker) maxConcurrency() int64 { return p.max.Load() }

// faultySender wraps a real sender and returns transient errors for a
// configurable percentage of calls. Thread-safe.
type faultySender struct {
	inner       ports.Sender
	failPercent int // 0-100
	calls       atomic.Int64
}

func newFaultySender(inner ports.Sender, failPercent int) *faultySender {
	return &faultySender{inner: inner, failPercent: failPercent}
}

func (s *faultySender) Send(ctx context.Context, env *domain.Envelope) error {
	s.calls.Add(1)
	if rand.IntN(100) < s.failPercent {
		return domain.ErrUnavailable.WithMessage("faulty sender injected failure")
	}
	return s.inner.Send(ctx, env)
}

// slowProcessor adds a configurable delay to each message.
type slowProcessor struct {
	delay time.Duration
	name  string
}

func newSlowProcessor(name string, delay time.Duration) *slowProcessor {
	return &slowProcessor{name: name, delay: delay}
}

func (p *slowProcessor) Name() string { return p.name }

func (p *slowProcessor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return next(ctx, env)
}

// filterProcessor drops messages that don't match the predicate,
// returning ErrMessageFiltered for rejected messages.
type filterProcessor struct {
	keep func(env *domain.Envelope) bool
}

func (p *filterProcessor) Name() string { return "filter" }

func (p *filterProcessor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	if !p.keep(env) {
		return domain.ErrMessageFiltered.WithMessage("filtered by predicate")
	}
	return next(ctx, env)
}

// pausableSender can be paused and resumed. While paused, Send blocks.
type pausableSender struct {
	inner  ports.Sender
	mu     sync.Mutex
	paused bool
	ch     chan struct{}
}

func newPausableSender(inner ports.Sender) *pausableSender {
	return &pausableSender{inner: inner, ch: make(chan struct{})}
}

func (s *pausableSender) Send(ctx context.Context, env *domain.Envelope) error {
	s.mu.Lock()
	if s.paused {
		ch := s.ch
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		s.mu.Unlock()
	}
	return s.inner.Send(ctx, env)
}

func (s *pausableSender) Pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.paused {
		s.paused = true
		s.ch = make(chan struct{})
	}
}

func (s *pausableSender) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused {
		s.paused = false
		close(s.ch)
	}
}

// slowSender wraps a sender with a fixed delay per call.
type slowSender struct {
	inner ports.Sender
	delay time.Duration
}

func newSlowSender(inner ports.Sender, delay time.Duration) *slowSender {
	return &slowSender{inner: inner, delay: delay}
}

func (s *slowSender) Send(ctx context.Context, env *domain.Envelope) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.inner.Send(ctx, env)
}

// alwaysFailSender always returns a transient error.
type alwaysFailSender struct{}

func (s *alwaysFailSender) Send(_ context.Context, _ *domain.Envelope) error {
	return domain.ErrUnavailable.WithMessage("always-fail sender")
}

// permanentFailSender always returns a permanent error, forcing DLQ routing.
type permanentFailSender struct{}

func (s *permanentFailSender) Send(_ context.Context, _ *domain.Envelope) error {
	return domain.ErrInvalidPayload.WithMessage("permanent-fail sender")
}

// chainOrderProcessor appends its stage name to a "chain_order" header
// to verify processor execution order.
type chainOrderProcessor struct {
	stage string
}

func (p *chainOrderProcessor) Name() string { return "chain-" + p.stage }

func (p *chainOrderProcessor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	if env.Headers == nil {
		env.Headers = make(map[string]any)
	}
	env.Headers["stage_"+p.stage] = "true"
	if prev, ok := env.Headers["chain_order"].(string); ok {
		env.Headers["chain_order"] = prev + "," + p.stage
	} else {
		env.Headers["chain_order"] = p.stage
	}
	return next(ctx, env)
}

// setupFIFOQueue creates an SQS FIFO queue with content-based dedup.
func setupFIFOQueue(t *testing.T, prefix string) (string, *awssqs.Client) {
	t.Helper()
	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue(prefix) + ".fifo"
	result, err := client.CreateQueue(context.Background(), &awssqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "true",
			"VisibilityTimeout":         "30",
		},
	})
	if err != nil {
		t.Fatalf("create FIFO queue: %v", err)
	}
	queueURL := *result.QueueUrl
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &awssqs.DeleteQueueInput{
			QueueUrl: aws.String(queueURL),
		})
	})
	return queueURL, client
}

// sendBulkToSQSFIFO sends messages to a FIFO queue with group IDs.
func sendBulkToSQSFIFO(t *testing.T, client *awssqs.Client, queueURL string, count int, groupFn func(i int) string) {
	t.Helper()
	for i := 0; i < count; i++ {
		body := fmt.Sprintf(`{"seq":%d}`, i)
		input := &awssqs.SendMessageInput{
			QueueUrl:       aws.String(queueURL),
			MessageBody:    aws.String(body),
			MessageGroupId: aws.String(groupFn(i)),
		}
		if _, err := client.SendMessage(context.Background(), input); err != nil {
			t.Fatalf("send FIFO msg %d: %v", i, err)
		}
	}
}

// pollSQSBodies polls until expected messages are received, returns bodies.
func pollSQSBodies(t *testing.T, client *awssqs.Client, queueURL string, expected int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var bodies []string
	for time.Now().Before(deadline) && len(bodies) < expected {
		out, err := client.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl:            aws.String(queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
			MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{
				sqstypes.MessageSystemAttributeNameAll,
			},
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			continue
		}
		for _, msg := range out.Messages {
			bodies = append(bodies, *msg.Body)
			_, _ = client.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
				QueueUrl:      aws.String(queueURL),
				ReceiptHandle: msg.ReceiptHandle,
			})
		}
	}
	return bodies
}

// noopReceiver blocks until context is cancelled (for Inject-based tests).
type noopReceiver struct{}

func (r *noopReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

// ---------------------------------------------------------------------------
// Docker control helpers
// ---------------------------------------------------------------------------

// dockerKill kills a Docker container by name.
func dockerKill(t *testing.T, name string) {
	t.Helper()
	out, err := exec.Command("docker", "kill", name).CombinedOutput()
	if err != nil {
		t.Logf("dockerKill %q: %v\n%s", name, err, out)
	}
}

// dockerRestart restarts a Docker container by name (kill + start).
func dockerRestart(t *testing.T, name string) {
	t.Helper()
	dockerKill(t, name)
	time.Sleep(500 * time.Millisecond)
	out, err := exec.Command("docker", "start", name).CombinedOutput()
	if err != nil {
		t.Fatalf("dockerRestart %q: start failed: %v\n%s", name, err, out)
	}
}

// ---------------------------------------------------------------------------
// MQTT session helpers for custom brokers
// ---------------------------------------------------------------------------

// setupMQTTSessionWithBroker creates an MQTT session against a custom broker
// URL. Unlike setupMQTTSession, it accepts the broker URL and receive maximum
// as parameters, making it suitable for per-test BrokerInstance usage.
func setupMQTTSessionWithBroker(
	t *testing.T, brokerURL, clientID string,
	mode domain.SessionMode, receiveMax uint16,
) *paho.Session {
	t.Helper()
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       clientID,
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     true,
		ReceiveMaximum: receiveMax,
	}, mode, testLogger(t))

	ctx := context.Background()
	require.NoError(t, sess.Start(ctx),
		"MQTT session Start %q at %s", clientID, brokerURL)

	select {
	case <-sess.Events():
	case <-time.After(5 * time.Second):
	}

	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

// newMQTTSessionWithBroker creates an MQTT session against a custom broker
// URL WITHOUT starting it. The runtime's SessionManager starts it.
func newMQTTSessionWithBroker(
	t *testing.T, brokerURL, clientID string,
	mode domain.SessionMode, receiveMax uint16,
) *paho.Session {
	t.Helper()
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       clientID,
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     mode == domain.SessionEphemeral,
		ReceiveMaximum: receiveMax,
	}, mode, testLogger(t))

	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

// ---------------------------------------------------------------------------
// newMQTTCollectorWithBroker — collector against a custom broker URL
// ---------------------------------------------------------------------------

func newMQTTCollectorWithBroker(
	t *testing.T, brokerURL, topic, clientIDPrefix string,
) *mqttCollector {
	t.Helper()
	clientID := mqttlocal.UniqueClientID(clientIDPrefix)

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       clientID,
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, testLogger(t))

	ctx := context.Background()
	require.NoError(t, sess.Start(ctx), "collector Start at %s", brokerURL)

	select {
	case <-sess.Events():
	case <-time.After(5 * time.Second):
	}

	require.NoError(t, sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}), "collector Reconcile")
	time.Sleep(300 * time.Millisecond)

	recv := paho.NewReceiver("collector-"+clientID, sess)
	recvCtx, recvCancel := context.WithCancel(ctx)

	c := &mqttCollector{cancel: recvCancel}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			c.mu.Lock()
			c.messages = append(c.messages, del.Envelope())
			c.mu.Unlock()
			return nil
		})
	}()

	t.Cleanup(func() {
		recvCancel()
		c.wg.Wait()
		_ = sess.Close(context.Background())
	})

	return c
}

// ---------------------------------------------------------------------------
// errorClassSender — returns errors based on the "error_type" header
// ---------------------------------------------------------------------------

// errorClassSender inspects the "error_type" header on each envelope and
// returns the corresponding sentinel error. If no header is set, or the
// value is unrecognised, it delegates to the inner sender.
//
//   - "transient" -> domain.ErrUnavailable
//   - "permanent" -> domain.ErrInvalidPayload
//   - anything else -> inner.Send
type errorClassSender struct {
	inner ports.Sender
}

func (s *errorClassSender) Send(ctx context.Context, env *domain.Envelope) error {
	if env.Headers != nil {
		if et, ok := env.Headers["error_type"].(string); ok {
			switch et {
			case "transient":
				return domain.ErrUnavailable.WithMessage("errorClassSender: transient")
			case "permanent":
				return domain.ErrInvalidPayload.WithMessage("errorClassSender: permanent")
			}
		}
	}
	return s.inner.Send(ctx, env)
}

// ---------------------------------------------------------------------------
// Unique ID helpers for collector deduplication
// ---------------------------------------------------------------------------

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool { return &b }

// ---------------------------------------------------------------------------
// Route config helpers
// ---------------------------------------------------------------------------

var directHoldCaps = []ports.Capability{
	ports.CapSourceRedelivery,
	ports.CapVisibilityExtension,
}

// countUnique returns the number of unique envelope IDs in the collector.
func countUnique(c *mqttCollector) int {
	msgs := c.getMessages()
	seen := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		seen[m.ID] = struct{}{}
	}
	return len(seen)
}

// ---------------------------------------------------------------------------
// gobridgesync — wait until all runtimes report ReadyForTraffic
// ---------------------------------------------------------------------------

// gobridgesync waits until all runtimes report ReadyForTraffic and
// ServiceLevel Full via DeepHealth. On timeout, logs detailed health
// for each bridge and fails the test.
func gobridgesync(t *testing.T, timeout time.Duration, runtimes ...*goruntime.Runtime) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allReady := true
		for _, rt := range runtimes {
			dh := rt.DeepHealth(context.Background())
			if !dh.ReadyForTraffic || dh.ServiceLevel != ports.ServiceLevelFull {
				allReady = false
				break
			}
		}
		if allReady {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Dump health for debugging on failure
	for _, rt := range runtimes {
		dh := rt.DeepHealth(context.Background())
		t.Logf("gobridgesync: instance=%s running=%v healthy=%v ready=%v service_level=%s sessions=%+v",
			dh.InstanceID, dh.Running, dh.Healthy, dh.ReadyForTraffic, dh.ServiceLevel, dh.Sessions)
	}
	t.Fatalf("gobridgesync: timed out waiting for %d bridges to be ready", len(runtimes))
}
