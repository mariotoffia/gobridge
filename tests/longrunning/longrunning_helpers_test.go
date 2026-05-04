//go:build longrunning

package longrunning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os/exec"
	"strings"
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
	time.Sleep(500 * time.Millisecond) // OTHER: let container fully stop before restarting
	out, err := exec.Command("docker", "start", name).CombinedOutput()
	if err != nil {
		t.Fatalf("dockerRestart %q: start failed: %v\n%s", name, err, out)
	}
}

// ---------------------------------------------------------------------------
// MQTT session helpers for custom brokers
// ---------------------------------------------------------------------------

const defaultKeepAlive uint16 = 30

// resolveKeepAlive returns the first value from the variadic slice, or
// defaultKeepAlive (30) if none is provided.
func resolveKeepAlive(vals []uint16) uint16 {
	if len(vals) == 0 {
		return defaultKeepAlive
	}
	return vals[0]
}

// setupMQTTSessionWithBroker creates an MQTT session against a custom broker
// URL. Unlike setupMQTTSession, it accepts the broker URL and receive maximum
// as parameters, making it suitable for per-test BrokerInstance usage.
//
// The optional keepAlive parameter overrides the default 30s MQTT KeepAlive.
// When testing broker stop/restart scenarios (docker kill + docker run),
// use a low value (e.g. 5) so autopaho detects the broken TCP connection
// within seconds instead of the default 30s. This directly reduces the
// reconnection delay after broker.Restart().
func setupMQTTSessionWithBroker(
	t *testing.T, brokerURL, clientID string,
	mode domain.SessionMode, receiveMax uint16,
	keepAlive ...uint16,
) *paho.Session {
	t.Helper()
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       clientID,
		KeepAlive:      resolveKeepAlive(keepAlive),
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
//
// The optional keepAlive parameter overrides the default 30s MQTT KeepAlive.
// When testing broker stop/restart scenarios (docker kill + docker run),
// use a low value (e.g. 5) so autopaho detects the broken TCP connection
// within seconds instead of the default 30s. This directly reduces the
// reconnection delay after broker.Restart().
func newMQTTSessionWithBroker(
	t *testing.T, brokerURL, clientID string,
	mode domain.SessionMode, receiveMax uint16,
	keepAlive ...uint16,
) *paho.Session {
	t.Helper()
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       clientID,
		KeepAlive:      resolveKeepAlive(keepAlive),
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

// newMQTTCollectorWithBroker creates a collector that subscribes to the given
// topic on a custom broker URL. The optional keepAlive parameter overrides the
// default 30s MQTT KeepAlive. When testing broker stop/restart scenarios, use a
// low value (e.g. 5) to speed up disconnect detection after broker.Restart().
func newMQTTCollectorWithBroker(
	t *testing.T, brokerURL, topic, clientIDPrefix string,
	keepAlive ...uint16,
) *mqttCollector {
	t.Helper()
	clientID := mqttlocal.UniqueClientID(clientIDPrefix)

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       clientID,
		KeepAlive:      resolveKeepAlive(keepAlive),
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
	waitSubReady(t, sess, 5*time.Second)

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
// sendProbe — black-box end-to-end pipeline readiness gate
// ---------------------------------------------------------------------------

// sendProbe injects a canary message into the SQS input queue and blocks
// until the collector receives it. This proves the full pipeline is
// operational: SQS receiver -> route runner -> outbox drainer -> MQTT
// session -> broker delivery -> collector subscription. No internal struct
// access required. The probe uses a unique payload marker so it can be
// distinguished from test data.
func sendProbe(
	t *testing.T,
	sqsClient *awssqs.Client,
	inputQueueURL string,
	collector *mqttCollector,
	timeout time.Duration,
) {
	t.Helper()
	probeID := fmt.Sprintf("__probe__%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"probe":"%s"}`, probeID)
	_, err := sqsClient.SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl:    &inputQueueURL,
		MessageBody: &body,
	})
	require.NoError(t, err, "send probe message")

	lrWaitFor(t, timeout, "probe arrival: "+probeID, func() bool {
		for _, m := range collector.getMessages() {
			if strings.Contains(string(m.Payload), probeID) {
				return true
			}
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// awaitBridgeReady — HTTP-based black-box bridge readiness
// ---------------------------------------------------------------------------

// awaitBridgeReady polls the bridge's HTTP /api/v1/monitor/deephealth
// endpoint until ready_for_traffic=true and service_level=full. This is
// the same check a load balancer or Kubernetes readiness probe would use.
func awaitBridgeReady(t *testing.T, monitorURL string, timeout time.Duration) {
	t.Helper()
	endpoint := monitorURL + "/api/v1/monitor/deephealth"
	lrWaitFor(t, timeout, "bridge ready via HTTP "+endpoint, func() bool {
		resp, err := http.Get(endpoint)
		if err != nil || resp.StatusCode != 200 {
			return false
		}
		defer resp.Body.Close()
		var dh struct {
			ReadyForTraffic bool   `json:"ready_for_traffic"`
			ServiceLevel    string `json:"service_level"`
		}
		if json.NewDecoder(resp.Body).Decode(&dh) != nil {
			return false
		}
		return dh.ReadyForTraffic && dh.ServiceLevel == "full"
	})
}

// ---------------------------------------------------------------------------
// newPersistentCollectorWithBroker — collector with persistent session
// ---------------------------------------------------------------------------

// newPersistentCollectorWithBroker creates a collector that uses a persistent
// MQTT session (CleanStart=false, SessionExpiryInterval=300). The broker
// queues messages for this client during disconnection, eliminating the
// subscriber-before-publisher race on broker restart.
func newPersistentCollectorWithBroker(
	t *testing.T, brokerURL, topic, clientIDPrefix string,
) *mqttCollector {
	t.Helper()
	clientID := mqttlocal.UniqueClientID(clientIDPrefix)
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              clientID,
		KeepAlive:             5,
		ConnectTimeout:        15 * time.Second,
		CleanStart:            false,
		SessionExpiryInterval: 300,
	}, domain.SessionPersistent, testLogger(t))

	ctx := context.Background()
	require.NoError(t, sess.Start(ctx), "persistent collector Start at %s", brokerURL)
	select {
	case <-sess.Events():
	case <-time.After(5 * time.Second):
	}
	require.NoError(t, sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}), "persistent collector Reconcile")
	waitSubReady(t, sess, 5*time.Second)

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
// gobridgesync — wait until all runtimes report ReadyForTraffic
// ---------------------------------------------------------------------------

// waitSubReady polls until the MQTT session reports ServiceLevelFull,
// replacing the brittle time.Sleep(200ms) pattern after Reconcile.
// waitSubReady waits for the broker to ACK all pending SUBSCRIBE frames.
// It deliberately does NOT require ServiceLevelFull because tests
// typically construct the receiver (handler) AFTER calling Reconcile;
// Full would require HandlersRegistered > 0 which has not happened yet.
func waitSubReady(t *testing.T, sess *paho.Session, timeout time.Duration) {
	t.Helper()
	lrWaitFor(t, timeout, "MQTT subscriptions active", func() bool {
		h := sess.Health(context.Background())
		return h.Connected && h.SubscriptionsActive == h.SubscriptionsWanted
	})
}

// requireMQTTSessionReady asserts that the runtime reports the given
// session as Connected and Ready. Fails fast (no polling) — call after
// gobridgesync returns to verify the helper saw the session at all.
// gobridgesync silently skips runtime entries with sessCfg == nil
// (see runtime/bridge_health.go); this assertion catches that
// misconfiguration so the test fails loudly instead of passing
// vacuously.
func requireMQTTSessionReady(t *testing.T, rt *goruntime.Runtime, sessionID string) {
	t.Helper()
	dh := rt.DeepHealth(context.Background())
	for _, sd := range dh.Sessions {
		if sd.SessionID == sessionID {
			require.True(t, sd.Connected, "session %s not connected", sessionID)
			require.True(t, sd.Ready, "session %s not ready", sessionID)
			return
		}
	}
	t.Fatalf("session %s missing from runtime health (likely sessCfg==nil at AddRoute)", sessionID)
}

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
		time.Sleep(50 * time.Millisecond) // SYNC: poll for bridge readiness
	}
	// Dump health for debugging on failure
	for _, rt := range runtimes {
		dh := rt.DeepHealth(context.Background())
		t.Logf("gobridgesync: instance=%s running=%v healthy=%v ready=%v service_level=%s sessions=%+v",
			dh.InstanceID, dh.Running, dh.Healthy, dh.ReadyForTraffic, dh.ServiceLevel, dh.Sessions)
	}
	t.Fatalf("gobridgesync: timed out waiting for %d bridges to be ready", len(runtimes))
}
