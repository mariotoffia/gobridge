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
	output, err := r.loadClient().ReceiveMessage(pollCtx, &awssqs.ReceiveMessageInput{
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
	r.metrics.Timer(MetricSQSPollLatency, elapsed,
		shared.Tag{Key: TagKeyQueueURL, Value: queueURL})
	// SQSReceiveLatency measures actual receive WORK per message, not
	// the intentional long-poll idle: on a quiet queue a message that
	// arrives 19s into a 20s wait would otherwise record ~19s of
	// deliberate idling and drown the real signal. The idle portion is
	// excluded per message via its broker SentTimestamp — see
	// receiveWorkLatency. Emitted only when the poll returned messages.
	if len(output.Messages) > 0 {
		receiveEnd := r.clock().Now()
		for _, msg := range output.Messages {
			r.metrics.Timer(MetricSQSReceiveLatency,
				receiveWorkLatency(pollStart, receiveEnd, msg.Attributes),
				shared.Tag{Key: TagKeyQueueURL, Value: queueURL})
		}
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "sqs: received",
			"queue_url", queueURL,
			"count", len(output.Messages),
		)
	}

	results := make([]rawInbound, 0, len(output.Messages))
	for _, msg := range output.Messages {
		env, receiptHandle, convErr := r.convertMessage(ctx, queueURL, msg)
		if convErr != nil {
			// Drop poison messages: they will hit the broker's
			// MaxReceiveCount-driven DLQ via VisibilityTimeout
			// expiration without us issuing a Delete (which would
			// suppress the redrive policy). Emitting the malformed
			// metric makes the drop visible in dashboards.
			r.metrics.Counter(MetricSQSMalformedMessages, 1,
				shared.Tag{Key: TagKeyQueueURL, Value: queueURL})
			if r.logger != nil {
				r.logger.Warn("sqs: dropping malformed message",
					"queue_url", queueURL,
					"message_id", aws.ToString(msg.MessageId),
					"error", convErr,
				)
			}
			_ = receiptHandle
			continue
		}
		results = append(results, rawInbound{env: env, receiptHandle: receiptHandle})
	}
	return results, nil
}

// headerSQSSentTimestamp is the envelope header carrying the broker's
// SentTimestamp system attribute, parsed to time.Time by
// attributesToHeaders.
const headerSQSSentTimestamp = "sqs.SentTimestamp"

// attrSentTimestamp is the raw SQS system-attribute key.
const attrSentTimestamp = "SentTimestamp"

// receiveWorkLatency returns the receive-work portion of a long poll
// for one message: the interval from the moment the message could
// first have been handed over — its broker SentTimestamp when it
// arrived mid-poll, else the poll start — until the poll returned.
// This excludes the intentional long-poll idle on a quiet queue, so
// SQSReceiveLatency reflects work instead of echoing WaitTimeSeconds.
// Broker/local clock skew can place SentTimestamp after receiveEnd;
// the result is clamped at zero.
func receiveWorkLatency(pollStart, receiveEnd time.Time, sysAttrs map[string]string) time.Duration {
	start := pollStart
	if v, ok := sysAttrs[attrSentTimestamp]; ok {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
			if ts := time.UnixMilli(ms); ts.After(start) {
				start = ts
			}
		}
	}
	d := receiveEnd.Sub(start)
	if d < 0 {
		return 0
	}
	return d
}

// convertMessage translates a single SDK message into a *messaging.Envelope
// plus the receipt handle the delivery uses for Ack/Retry/Extend. Returns
// a wrapped *shared.BridgeError when NewEnvelope rejects the input so the
// poll loop can surface a malformed-message metric and skip the message.
func (r *Receiver) convertMessage(
	ctx context.Context,
	queueURL string,
	msg sqstypes.Message,
) (*messaging.Envelope, string, error) {
	receiptHandle := aws.ToString(msg.ReceiptHandle)
	body := aws.ToString(msg.Body)

	headers := attributesToHeaders(msg.MessageAttributes, msg.Attributes)

	// T08: no fallback to the configured queue name/URL. Subject comes
	// only from an explicit "Subject" message attribute (or the inner
	// SNS Subject when SNSUnwrap is enabled); otherwise it is empty.
	subject := ""
	if v, ok := headers["Subject"].(string); ok && v != "" {
		subject = v
	}

	payload := []byte(body)

	if r.cfg.SNSUnwrap {
		if unwrapped, ok := trySNSUnwrap(body, headers); ok {
			if logging.TraceEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelTrace, "sqs: SNS unwrap",
					"queue_url", queueURL,
					"message_id", aws.ToString(msg.MessageId),
					"new_subject", unwrapped.subject,
					"has_subject", unwrapped.hasSubject,
				)
			}
			if unwrapped.hasSubject {
				subject = unwrapped.subject
			}
			payload = []byte(unwrapped.message)
		}
	}

	id := aws.ToString(msg.MessageId)
	if id == "" {
		id = generateEnvelopeID()
	}

	// Headers go through EnvelopeInput so NewEnvelope's
	// StripReservedHeaders is the single chokepoint for reserved-prefix
	// sanitation — see I4 in the round-1 reviewer notes.
	//
	// CreatedAt prefers the broker's SentTimestamp (parsed into the
	// sqs.SentTimestamp header by attributesToHeaders) over the bridge
	// receive time, so TTL/expiry policies measure the message's true
	// age — including the time it spent queued — rather than restarting
	// the clock at every hop.
	createdAt := r.clock().Now()
	if ts, ok := headers[headerSQSSentTimestamp].(time.Time); ok && !ts.IsZero() {
		createdAt = ts
	}
	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:        id,
		Subject:   subject,
		Payload:   payload,
		Headers:   headers,
		CreatedAt: createdAt,
	}, r.clock().Now())
	if err != nil {
		return nil, receiptHandle, wrapEnvelopeErr(err)
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "sqs: converting",
			"queue_url", queueURL,
			"message_id", env.ID,
			"body_len", len(body),
		)
	}

	return env, receiptHandle, nil
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
		if k == attrSentTimestamp {
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
//
// hasSubject is true only when the inner SNS Subject field is present
// and non-empty. Callers must NOT promote TopicArn into
// Envelope.Subject when hasSubject is false — TopicArn remains
// available via headers["sns.topic_arn"].
type snsPayload struct {
	subject    string
	message    string
	hasSubject bool
}

// trySNSUnwrap detects an SNS-over-SQS notification envelope and pulls
// the inner subject/message out of it. A body qualifies only when it is
// JSON with Type == "Notification" AND a non-empty TopicArn — the shape
// SNS actually delivers. Requiring the Type field prevents arbitrary
// producer JSON that merely contains a TopicArn key from being
// unwrapped with forged sns.* headers. (The transport-level guarantee
// that the body really came from SNS is the queue policy restricting
// sqs:SendMessage to the topic — an operator concern documented with
// the sns_unwrap option, not enforceable here.) The original SNS
// metadata is preserved in headers under sns.* keys. When the SNS
// notification has no Subject field, snsPayload.subject is empty and
// hasSubject=false so callers leave Envelope.Subject untouched (T08: no
// fallback to TopicArn or queue name).
func trySNSUnwrap(body string, headers map[string]any) (snsPayload, bool) {
	var raw struct {
		Type     string `json:"Type"`
		TopicArn string `json:"TopicArn"`
		Subject  string `json:"Subject"`
		Message  string `json:"Message"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil ||
		raw.Type != "Notification" || raw.TopicArn == "" {
		return snsPayload{}, false
	}

	headers["sns.topic_arn"] = raw.TopicArn

	out := snsPayload{message: raw.Message}
	if raw.Subject != "" {
		out.subject = raw.Subject
		out.hasSubject = true
		headers["sns.subject"] = raw.Subject
	}

	return out, true
}

// ensureClient lazily creates the SDK SQS client for the receiver,
// honouring an injected fake (cfg.Client) when present.
func (r *Receiver) ensureClient(ctx context.Context) error {
	r.initMu.Lock()
	defer r.initMu.Unlock()

	if r.loadClient() != nil {
		return nil
	}
	if r.cfg.Client != nil {
		r.storeClient(r.cfg.Client)
		return nil
	}

	cfg, err := buildAWSConfig(ctx, r.cfg.Region, r.cfg.Endpoint, r.cfg.Profile)
	if err != nil {
		return err
	}
	r.storeClient(awssqs.NewFromConfig(cfg))

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "sqs: receiver initialized",
			"region", r.cfg.Region,
			"endpoint", r.cfg.Endpoint,
		)
	}

	return nil
}
