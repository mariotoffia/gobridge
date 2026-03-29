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

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time check.
var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for Amazon SQS. It long-polls for
// messages and emits each as a ports.Delivery whose Ack/Retry/Extend
// operations map to the SQS receipt-handle lifecycle.
type Receiver struct {
	cfg     ReceiverConfig
	client  sqsAPI
	logger  *slog.Logger
	metrics ports.MetricsExporter
	initMu  sync.Mutex
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
	return &Receiver{cfg: cfg, logger: l, metrics: m}, nil
}

// Run starts the long-poll loop. For each received SQS message it
// creates a Delivery and calls emit synchronously, providing natural
// backpressure. Run blocks until ctx is cancelled or an unrecoverable
// error occurs.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if err := r.ensureClient(ctx); err != nil {
		return err
	}

	queueURL, err := resolveQueueURL(ctx, r.client, r.cfg.QueueURL, r.cfg.QueueName)
	if err != nil {
		return err
	}

	logging.DebugContext(r.logger, ctx, "sqs: receiver starting",
		"queue_url", queueURL,
		"max_messages", r.cfg.MaxMessages,
		"visibility_timeout", r.cfg.VisibilityTimeout,
		"auto_extend", r.cfg.autoExtendEnabled(),
	)

	return r.pollLoop(ctx, queueURL, emit)
}

func (r *Receiver) pollLoop(
	ctx context.Context,
	queueURL string,
	emit func(context.Context, ports.Delivery) error,
) error {
	backoff := newPollBackoff()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		pollStart := time.Now()
		output, err := r.client.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   r.cfg.MaxMessages,
			WaitTimeSeconds:       r.cfg.WaitTimeSeconds,
			VisibilityTimeout:     r.cfg.VisibilityTimeout,
			MessageAttributeNames: []string{"All"},
			AttributeNames:        []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
		})

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
			case <-time.After(delay):
			}
			continue
		}

		r.metrics.Timer(domain.MetricSQSPollLatency, time.Since(pollStart),
			domain.Tag{Key: domain.TagKeyQueueURL, Value: queueURL})
		backoff.reset()

		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "sqs: received",
				"queue_url", queueURL,
				"count", len(output.Messages),
			)
		}

		for _, msg := range output.Messages {
			del := r.convertMessage(ctx, queueURL, msg)

			// Create a per-delivery context so that auto-extend failure
			// can cancel processing without affecting other deliveries.
			deliveryCtx, deliveryCancel := context.WithCancel(ctx)
			del.processingCancel = deliveryCancel

			if err := emit(deliveryCtx, del); err != nil {
				deliveryCancel()
				return err
			}
		}
	}
}

// pollBackoff implements exponential backoff with jitter for poll loops.
type pollBackoff struct {
	current time.Duration
}

const (
	pollBackoffInitial    = time.Second
	pollBackoffMax        = 30 * time.Second
	pollBackoffMultiplier = 2
)

func newPollBackoff() *pollBackoff {
	return &pollBackoff{current: pollBackoffInitial}
}

func (b *pollBackoff) next() time.Duration {
	delay := b.current

	jitter := time.Duration(float64(delay) * 0.25 * (2*rand.Float64() - 1))
	delay += jitter

	b.current *= pollBackoffMultiplier
	if b.current > pollBackoffMax {
		b.current = pollBackoffMax
	}

	return delay
}

func (b *pollBackoff) reset() {
	b.current = pollBackoffInitial
}

func (r *Receiver) convertMessage(
	ctx context.Context,
	queueURL string,
	msg sqstypes.Message,
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
		CreatedAt: time.Now(),
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
		r.logger,
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

	logging.DebugContext(r.logger, ctx, "sqs: receiver initialized",
		"region", r.cfg.Region,
		"endpoint", r.cfg.Endpoint,
	)

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
