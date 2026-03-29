//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"

	"testing"
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
// gobridgesync — wait until all runtimes report ReadyForTraffic
// ---------------------------------------------------------------------------

// gobridgesync waits until all runtimes report ReadyForTraffic via DeepHealth.
// On timeout, logs detailed health for each bridge and fails the test.
func gobridgesync(t *testing.T, timeout time.Duration, runtimes ...*goruntime.Runtime) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allReady := true
		for _, rt := range runtimes {
			dh := rt.DeepHealth(context.Background())
			if !dh.ReadyForTraffic {
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
		t.Logf("gobridgesync: instance=%s running=%v healthy=%v ready=%v sessions=%+v",
			dh.InstanceID, dh.Running, dh.Healthy, dh.ReadyForTraffic, dh.Sessions)
	}
	t.Fatalf("gobridgesync: timed out waiting for %d bridges to be ready", len(runtimes))
}
