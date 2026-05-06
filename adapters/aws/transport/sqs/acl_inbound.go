package sqs

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// rawInbound is a non-SDK pair returned by pollAndConvert: it bundles the
// translated envelope with the SDK receipt handle so the polling loop in
// receiver.go can hand both to newDelivery without ever naming an SDK
// type itself.
type rawInbound struct {
	env           *messaging.Envelope
	receiptHandle string
}

// pollAndConvert performs one ReceiveMessage long-poll and translates each
// returned SDK message into a messaging.Envelope. It also emits the
// SQS-poll/per-message receive-latency metrics. All SDK types stay
// inside this ACL file.
func (r *Receiver) pollAndConvert(
	ctx context.Context,
	queueURL string,
	pollTimeout time.Duration,
) ([]rawInbound, error) {
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
		return nil, err
	}

	elapsed := r.clock().Since(pollStart)
	r.metrics.Timer(shared.MetricSQSPollLatency, elapsed,
		shared.Tag{Key: shared.TagKeyQueueURL, Value: queueURL})
	if len(output.Messages) > 0 {
		perMsg := elapsed / time.Duration(len(output.Messages))
		r.metrics.Timer(shared.MetricSQSReceiveLatency, perMsg,
			shared.Tag{Key: shared.TagKeyQueueURL, Value: queueURL})
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "sqs: received",
			"queue_url", queueURL,
			"count", len(output.Messages),
		)
	}

	results := make([]rawInbound, 0, len(output.Messages))
	for _, msg := range output.Messages {
		env, receiptHandle := r.convertMessage(ctx, queueURL, msg)
		results = append(results, rawInbound{env: env, receiptHandle: receiptHandle})
	}
	return results, nil
}

// convertMessage translates a single SDK message into a *messaging.Envelope
// plus the receipt handle the delivery uses for Ack/Retry/Extend.
func (r *Receiver) convertMessage(
	ctx context.Context,
	queueURL string,
	msg sqstypes.Message,
) (*messaging.Envelope, string) {
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

	env := &messaging.Envelope{
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

	return env, receiptHandle
}

// attributesToHeaders converts SQS message attributes and system attributes
// into an Envelope headers map. Headers with the reserved x-bridge.* prefix
// are stripped to prevent injection from external sources.
func attributesToHeaders(
	msgAttrs map[string]sqstypes.MessageAttributeValue,
	sysAttrs map[string]string,
) map[string]any {
	h := make(map[string]any, len(msgAttrs)+len(sysAttrs))

	for k, attr := range msgAttrs {
		if messaging.IsReservedHeader(k) {
			continue
		}
		switch {
		case attr.StringValue != nil:
			h[k] = *attr.StringValue
		case attr.BinaryValue != nil:
			h[k] = attr.BinaryValue
		}
	}

	for k, v := range sysAttrs {
		key := "sqs." + k
		if k == "SentTimestamp" {
			if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
				h[key] = time.UnixMilli(ms)
				continue
			}
		}
		if k == "ApproximateReceiveCount" {
			if n, err := strconv.Atoi(v); err == nil {
				h[key] = n
				continue
			}
		}
		h[key] = v
	}

	return h
}

// snsPayload is the subset of an SNS notification relevant for unwrapping.
type snsPayload struct {
	subject string
	message string
}

// trySNSUnwrap detects an SNS-over-SQS notification envelope and pulls
// the inner subject/message out of it. The original SNS metadata is
// preserved in headers under sns.* keys.
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

// ensureClient lazily creates the SDK SQS client for the receiver,
// honouring an injected fake (cfg.Client) when present.
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
		return err
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
