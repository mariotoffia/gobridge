package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time check.
var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for Amazon SQS. It long-polls for
// messages and emits each as a ports.Delivery whose Ack/Retry/Extend
// operations map to the SQS receipt-handle lifecycle.
type Receiver struct {
	cfg    ReceiverConfig
	client sqsAPI
	logger *slog.Logger
}

// NewReceiver creates an SQS Receiver.
func NewReceiver(cfg ReceiverConfig, logger *slog.Logger) (*Receiver, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Receiver{cfg: cfg, logger: logger}, nil
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

	return r.pollLoop(ctx, queueURL, emit)
}

func (r *Receiver) pollLoop(
	ctx context.Context,
	queueURL string,
	emit func(context.Context, ports.Delivery) error,
) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

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
			if r.logger != nil {
				r.logger.Warn("sqs: ReceiveMessage failed, retrying",
					"queue", queueURL,
					"error", err,
				)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}

		for _, msg := range output.Messages {
			del := r.convertMessage(ctx, queueURL, msg)

			if err := emit(ctx, del); err != nil {
				return err
			}
		}
	}
}

func (r *Receiver) convertMessage(
	ctx context.Context,
	queueURL string,
	msg sqstypes.Message,
) *sqsDelivery {
	receiptHandle := aws.ToString(msg.ReceiptHandle)
	body := aws.ToString(msg.Body)

	env := &domain.Envelope{
		ID:        aws.ToString(msg.MessageId),
		Subject:   queueURL,
		Payload:   []byte(body),
		CreatedAt: time.Now(),
	}

	headers := attributesToHeaders(msg.MessageAttributes, msg.Attributes)

	if r.cfg.SNSUnwrap {
		if unwrapped, ok := trySNSUnwrap(body, headers); ok {
			env.Subject = unwrapped.subject
			env.Payload = []byte(unwrapped.message)
		}
	}

	env.Headers = headers

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
