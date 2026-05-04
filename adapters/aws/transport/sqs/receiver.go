package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time check.
var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for Amazon SQS. It long-polls for
// messages and emits each as a ports.Delivery whose Ack/Retry/Extend
// operations map to the SQS receipt-handle lifecycle.
type Receiver struct {
	cfg         ReceiverConfig
	client      sqsAPI
	logger      *slog.Logger
	metrics     ports.MetricsExporter
	clk         clock.Clock
	initMu      sync.Mutex
	started     chan struct{}
	startedOnce sync.Once
}

// NewReceiver creates an SQS Receiver.
func NewReceiver(cfg ReceiverConfig, logger *slog.Logger) (*Receiver, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	l := cfg.Logger
	if l == nil {
		l = logger
	}
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System
	}
	return &Receiver{cfg: cfg, logger: l, metrics: m, clk: clk, started: make(chan struct{})}, nil
}

func (r *Receiver) clock() clock.Clock {
	if r.clk != nil {
		return r.clk
	}
	return clock.System
}

// Started returns a channel that is closed once the receiver's poll
// loop is live and ready to process messages. It satisfies
// ports.ReceiverStartedSignaler.
func (r *Receiver) Started() <-chan struct{} { return r.started }

// Run starts the long-poll loop. For each received SQS message it
// creates a Delivery and calls emit synchronously, providing natural
// backpressure. Run blocks until ctx is cancelled or an unrecoverable
// error occurs.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	initCtx, initCancel := context.WithTimeout(ctx, r.cfg.InitTimeout)
	defer initCancel()

	if err := r.ensureClient(initCtx); err != nil {
		return err
	}

	queueURL, err := resolveQueueURL(initCtx, r.client, r.cfg.QueueURL, r.cfg.QueueName)
	if err != nil {
		return err
	}

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "sqs: receiver starting",
			"queue_url", queueURL,
			"max_messages", r.cfg.MaxMessages,
			"visibility_timeout", r.cfg.VisibilityTimeout,
			"auto_extend", r.cfg.autoExtendEnabled(),
		)
	}

	return r.pollLoop(ctx, queueURL, emit)
}

func (r *Receiver) pollLoop(
	ctx context.Context,
	queueURL string,
	emit func(context.Context, ports.Delivery) error,
) error {
	backoff := newPollBackoffFromConfig(r.cfg)
	pollTimeout := time.Duration(r.cfg.WaitTimeSeconds+10) * time.Second

	r.startedOnce.Do(func() { close(r.started) })

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		pollStart := r.clock().Now()
		pollCtx, pollCancel := context.WithTimeout(ctx, pollTimeout)
		output, err := r.client.ReceiveMessage(pollCtx, &awssqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   r.cfg.MaxMessages,
			WaitTimeSeconds:       r.cfg.WaitTimeSeconds,
			VisibilityTimeout:     r.cfg.VisibilityTimeout,
			MessageAttributeNames: []string{"All"},
			AttributeNames:        []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
		})
		pollCancel()

		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := backoff.next()
			if r.logger != nil {
				r.logger.Warn("sqs: ReceiveMessage failed, retrying",
					"queue", queueURL,
					"error", err,
					"retry_after", delay,
				)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-r.clock().After(delay):
			}
			continue
		}

		r.metrics.Timer(domain.MetricSQSPollLatency, r.clock().Since(pollStart),
			domain.Tag{Key: domain.TagKeyQueueURL, Value: queueURL})
		if len(output.Messages) > 0 {
			perMsg := r.clock().Since(pollStart) / time.Duration(len(output.Messages))
			r.metrics.Timer(domain.MetricSQSReceiveLatency, perMsg,
				domain.Tag{Key: domain.TagKeyQueueURL, Value: queueURL})
		}
		backoff.reset()

		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "sqs: received",
				"queue_url", queueURL,
				"count", len(output.Messages),
			)
		}

		for _, msg := range output.Messages {
			// Create a per-delivery context so that auto-extend failure
			// can cancel processing without affecting other deliveries.
			// The cancel func is passed into convertMessage/newDelivery
			// so it is set BEFORE any auto-extend goroutine starts.
			deliveryCtx, deliveryCancel := context.WithCancel(ctx)
			del := r.convertMessage(deliveryCtx, queueURL, msg, deliveryCancel)

			if err := emit(deliveryCtx, del); err != nil {
				deliveryCancel()
				return err
			}
		}
	}
}

// pollBackoff implements exponential backoff with jitter for poll loops.
type pollBackoff struct {
	initial    time.Duration
	max        time.Duration
	multiplier float64
	current    time.Duration
}

func newPollBackoffFromConfig(cfg ReceiverConfig) *pollBackoff {
	return &pollBackoff{
		initial:    cfg.PollBackoffInitial,
		max:        cfg.PollBackoffMax,
		multiplier: cfg.PollBackoffMultiplier,
		current:    cfg.PollBackoffInitial,
	}
}

func (b *pollBackoff) next() time.Duration {
	delay := b.current

	jitter := time.Duration(float64(delay) * 0.25 * (2*rand.Float64() - 1))
	delay += jitter

	b.current = time.Duration(float64(b.current) * b.multiplier)
	if b.current > b.max {
		b.current = b.max
	}

	return delay
}

func (b *pollBackoff) reset() {
	b.current = b.initial
}

func (r *Receiver) convertMessage(
	ctx context.Context,
	queueURL string,
	msg sqstypes.Message,
	processingCancel context.CancelFunc,
) *sqsDelivery {
	receiptHandle := aws.ToString(msg.ReceiptHandle)
	body := aws.ToString(msg.Body)

	headers := attributesToHeaders(msg.MessageAttributes, msg.Attributes)

	subject := r.cfg.QueueName
	if subject == "" {
		subject = r.cfg.QueueURL
	}
	if v, ok := headers["Subject"].(string); ok && v != "" {
		subject = v
	}

	env := &domain.Envelope{
		ID:        aws.ToString(msg.MessageId),
		Subject:   subject,
		Payload:   []byte(body),
		CreatedAt: r.clock().Now(),
	}

	if r.cfg.SNSUnwrap {
		if unwrapped, ok := trySNSUnwrap(body, headers); ok {
			if logging.TraceEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelTrace, "sqs: SNS unwrap",
					"queue_url", queueURL,
					"message_id", env.ID,
					"new_subject", unwrapped.subject,
				)
			}
			env.Subject = unwrapped.subject
			env.Payload = []byte(unwrapped.message)
		}
	}

	env.Headers = headers

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "sqs: converting",
			"queue_url", queueURL,
			"message_id", env.ID,
			"body_len", len(body),
		)
	}

	return newDelivery(
		ctx,
		env,
		r.client,
		queueURL,
		receiptHandle,
		r.cfg.VisibilityTimeout,
		r.cfg.autoExtendEnabled(),
		processingCancel,
		r.logger,
		r.metrics,
		r.clock(),
	)
}

func (r *Receiver) ensureClient(ctx context.Context) error {
	r.initMu.Lock()
	defer r.initMu.Unlock()

	if r.client != nil {
		return nil
	}
	if r.cfg.Client != nil {
		r.client = r.cfg.Client
		return nil
	}

	cfg, err := buildAWSConfig(ctx, r.cfg.Region, r.cfg.Endpoint, r.cfg.Profile)
	if err != nil {
		return domain.ErrUnavailable.Wrap(fmt.Errorf("sqs receiver: build AWS config: %w", err))
	}
	r.client = awssqs.NewFromConfig(cfg)

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "sqs: receiver initialized",
			"region", r.cfg.Region,
			"endpoint", r.cfg.Endpoint,
		)
	}

	return nil
}

// snsPayload is the subset of an SNS notification relevant for unwrapping.
type snsPayload struct {
	subject string
	message string
}

func trySNSUnwrap(body string, headers map[string]any) (snsPayload, bool) {
	var raw struct {
		TopicArn string `json:"TopicArn"`
		Subject  string `json:"Subject"`
		Message  string `json:"Message"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil || raw.TopicArn == "" {
		return snsPayload{}, false
	}

	subject := raw.TopicArn
	if raw.Subject != "" {
		subject = raw.Subject
		headers["sns.subject"] = raw.Subject
	}
	headers["sns.topic_arn"] = raw.TopicArn

	return snsPayload{subject: subject, message: raw.Message}, true
}
