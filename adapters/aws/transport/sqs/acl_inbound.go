package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
//
// The client is passed in (not re-loaded here) so the receive uses the SAME
// snapshot the caller binds to every resulting delivery's settlement
// (Finding: c8-settle-client): a credential rotation between receive and
// settle must not split the batch across two principals.
func (r *Receiver) pollAndConvert(
	ctx context.Context,
	client sqsAPI,
	queueURL string,
	maxMessages int32,
	pollTimeout time.Duration,
) ([]rawInbound, error) {
	pollStart := r.clock().Now()
	pollCtx, pollCancel := context.WithTimeout(ctx, pollTimeout)
	output, err := client.ReceiveMessage(pollCtx, &awssqs.ReceiveMessageInput{
		QueueUrl:              aws.String(queueURL),
		MaxNumberOfMessages:   maxMessages,
		WaitTimeSeconds:       r.cfg.WaitTimeSeconds,
		VisibilityTimeout:     r.cfg.VisibilityTimeout,
		MessageAttributeNames: []string{"All"},
		// MessageSystemAttributeNames replaces the deprecated
		// AttributeNames field (Finding 9). It requests every system
		// attribute — including ApproximateReceiveCount, which the runtime
		// retry cap reads (attributesToHeaders → "sqs.ApproximateReceiveCount")
		// and the FIFO dedup/ordering lift below relies on.
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{sqstypes.MessageSystemAttributeNameAll},
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
			// Emitting the malformed metric makes the drop visible in
			// dashboards regardless of which drop strategy applies below.
			r.metrics.Counter(MetricSQSMalformedMessages, 1,
				shared.Tag{Key: TagKeyQueueURL, Value: queueURL})

			recvCount := approximateReceiveCount(msg.Attributes)

			// Adapter-enforced poison backstop (Chunk 13). When
			// poison_max_receives is configured and this malformed message has
			// resurfaced at least that many times, DELETE it to break an
			// otherwise-unbounded redelivery hot loop on a source queue with no
			// native redrive policy. The delete is a controlled, observable
			// drop (SQSPoisonDropped + Error log). poison_max_receives should
			// sit ABOVE the queue's native maxReceiveCount so native redrive
			// still wins where configured — a message only climbs this high
			// when nothing is draining it. When the backstop is disabled (0)
			// the message is dropped WITHOUT a Delete (below), so the queue's
			// own MaxReceiveCount redrive policy owns it; issuing a Delete then
			// would suppress that redrive policy.
			if r.cfg.PoisonMaxReceives > 0 && recvCount >= int(r.cfg.PoisonMaxReceives) {
				r.dropPoisonMessage(ctx, client, queueURL, receiptHandle, aws.ToString(msg.MessageId), recvCount, convErr)
				continue
			}

			if r.logger != nil {
				// Escalate to Error once a malformed message has been
				// redelivered past the sanity bound: the drop-without-delete
				// strategy relies entirely on the queue's redrive policy to
				// stop the loop, so a high receive count is a strong signal
				// that NO redrive policy is configured and the message will
				// otherwise return every visibility timeout forever
				// (Finding 6). Settlement behaviour is deliberately
				// unchanged — only the operator signal is escalated.
				if recvCount >= poisonReceiveCountThreshold {
					r.logger.Error("sqs: poison message repeatedly redelivered; "+
						"verify the source queue has a redrive policy (maxReceiveCount) to a DLQ, "+
						"or set poison_max_receives to enable the adapter backstop",
						"queue_url", queueURL,
						"message_id", aws.ToString(msg.MessageId),
						"approximate_receive_count", recvCount,
						"error", convErr,
					)
				} else {
					r.logger.Warn("sqs: dropping malformed message",
						"queue_url", queueURL,
						"message_id", aws.ToString(msg.MessageId),
						"error", convErr,
					)
				}
			}
			continue
		}
		results = append(results, rawInbound{env: env, receiptHandle: receiptHandle})
	}
	return results, nil
}

// dropPoisonMessage deletes a malformed ("poison") message the receiver
// cannot convert, as the adapter-enforced backstop once poison_max_receives is
// reached (Chunk 13). It is a controlled, observable drop that breaks an
// otherwise-unbounded redelivery hot loop on a source queue with no native
// redrive policy. Best effort and bounded by the settlement timeout: a failed
// delete counts a settlement error and lets the message redeliver (the loop
// continues, no worse than the disabled-backstop path) rather than wedging the
// poll loop for the TCP RTO.
//
// The delete uses the SAME client snapshot the poll bound to the batch
// (Finding: c8-settle-client), so a credential rotation between receive and
// this settlement cannot split them across two principals.
//
// ponytail: the ceiling is a local delete (a controlled drop) rather than a
// bridge-DLQ handoff — a conversion failure happens before the message becomes
// a ports.Delivery, so there is no delivery-level poison sink the receiver can
// route it to without a shared ports mechanism.
func (r *Receiver) dropPoisonMessage(
	ctx context.Context,
	client sqsAPI,
	queueURL, receiptHandle, messageID string,
	recvCount int,
	convErr error,
) {
	delCtx, cancel := boundedSettleContext(ctx, sqsSettlementTimeout)
	defer cancel()

	_, err := client.DeleteMessage(delCtx, &awssqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		r.metrics.Counter(MetricSQSSettlementErrors, 1,
			shared.Tag{Key: TagKeyQueueURL, Value: queueURL})
		if r.logger != nil {
			r.logger.Error("sqs: poison-message backstop delete failed; message will redeliver",
				"queue_url", queueURL,
				"message_id", messageID,
				"approximate_receive_count", recvCount,
				"error", err,
			)
		}
		return
	}

	r.metrics.Counter(MetricSQSPoisonDropped, 1,
		shared.Tag{Key: TagKeyQueueURL, Value: queueURL})
	if r.logger != nil {
		r.logger.Error("sqs: poison message deleted by adapter backstop "+
			"(poison_max_receives reached); message dropped — no native redrive "+
			"policy caught it",
			"queue_url", queueURL,
			"message_id", messageID,
			"approximate_receive_count", recvCount,
			"poison_max_receives", r.cfg.PoisonMaxReceives,
			"error", convErr,
		)
	}
}

// checkRedrivePolicy performs a best-effort startup check of the source
// queue's native redrive policy (maxReceiveCount -> DLQ) and reconciles it
// with the adapter poison backstop (Chunk 13 + destructive-preemption
// follow-up).
//
// It NEVER fails startup for a permission or availability reason: a
// GetQueueAttributes error (commonly a missing sqs:GetQueueAttributes grant)
// degrades to a Warn (backstop on) or debug (backstop off) so a least-
// privilege deployment is not blocked by an advisory check.
//
// It DOES fail startup (shared.ErrInvalidConfig, permanent) for one
// loss-critical misconfiguration: the queue has a READABLE native redrive
// policy AND the poison backstop would fire at or before it
// (poison_max_receives <= native maxReceiveCount). The backstop DELETES the
// message (destroying the payload) whereas native redrive MOVES it to a DLQ
// (preserving it), so a backstop that fires first silently pre-empts the DLQ
// and loses data. Returning here aborts Run before Started() so a readiness
// probe never observes a ready route for a destructive config.
//
// Behaviour by mode:
//   - backstop OFF (poison_max_receives == 0): a queue with NO redrive policy
//     emits SQSMissingRedrivePolicy + a Warn; a queue with one is silent.
//   - backstop ON: verify it will not pre-empt native redrive (above). A queue
//     with NO native redrive is the expected backstop use case (the backstop
//     is the sole bound) and is accepted.
func (r *Receiver) checkRedrivePolicy(ctx context.Context, client sqsAPI, queueURL string) error {
	if client == nil {
		return nil
	}

	out, err := client.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameRedrivePolicy},
	})
	if err != nil {
		// Cannot verify (likely no sqs:GetQueueAttributes grant). Do NOT fail
		// startup — a least-privilege deployment must not be blocked by an
		// advisory check. With the backstop configured we could not confirm it
		// will not pre-empt a native DLQ, so Warn loudly rather than the silent
		// debug used when the backstop is off.
		if r.cfg.PoisonMaxReceives > 0 {
			if r.logger != nil {
				r.logger.Warn("sqs: could not read source queue redrive policy to verify it against "+
					"poison_max_receives (permission?); proceeding best-effort — ensure poison_max_receives "+
					"stays ABOVE any native maxReceiveCount so native redrive (which preserves the payload to "+
					"a DLQ) wins over the adapter's destructive delete",
					"queue_url", queueURL,
					"poison_max_receives", r.cfg.PoisonMaxReceives,
					"error", err,
				)
			}
		} else if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug,
				"sqs: could not verify source queue redrive policy (permission?)",
				"queue_url", queueURL,
				"error", err,
			)
		}
		return nil
	}

	policy := ""
	if out != nil {
		policy = out.Attributes[string(sqstypes.QueueAttributeNameRedrivePolicy)]
	}
	maxReceive, haveMax := parseMaxReceiveCount(policy)

	if r.cfg.PoisonMaxReceives > 0 {
		// Loss-critical guard: a readable native redrive policy the backstop
		// would fire at or before means the adapter's DeleteMessage destroys
		// the payload before SQS can move it to the DLQ. Refuse to start.
		if haveMax && int(r.cfg.PoisonMaxReceives) <= maxReceive {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"sqs: poison_max_receives (%d) must be greater than the source queue's native redrive "+
					"maxReceiveCount (%d): the adapter backstop DELETES a poison message (losing the "+
					"payload) whereas native redrive MOVES it to the DLQ (preserving it), so a backstop "+
					"at or below maxReceiveCount silently pre-empts the DLQ and loses data. Raise "+
					"poison_max_receives above %d, or set it to 0 to rely on native redrive.",
				r.cfg.PoisonMaxReceives, maxReceive, maxReceive))
		}
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug,
				"sqs: poison backstop reconciled with native redrive policy",
				"queue_url", queueURL,
				"poison_max_receives", r.cfg.PoisonMaxReceives,
				"native_max_receive_count", maxReceive,
				"has_native_redrive", haveMax,
			)
		}
		return nil
	}

	// Backstop OFF: advisory missing-redrive signal (unchanged behaviour).
	if policy != "" {
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "sqs: source queue has a redrive policy",
				"queue_url", queueURL,
			)
		}
		return nil
	}

	r.metrics.Counter(MetricSQSMissingRedrivePolicy, 1,
		shared.Tag{Key: TagKeyQueueURL, Value: queueURL})
	if r.logger != nil {
		r.logger.Warn("sqs: source queue has NO native redrive policy "+
			"(maxReceiveCount -> DLQ); a malformed message the bridge cannot "+
			"convert will redeliver forever. Configure a redrive policy on the "+
			"queue, or set poison_max_receives to enable the adapter backstop",
			"queue_url", queueURL,
		)
	}
	return nil
}

// parseMaxReceiveCount extracts maxReceiveCount from an SQS RedrivePolicy
// attribute value — a JSON document such as
// {"deadLetterTargetArn":"...","maxReceiveCount":"5"}. SQS renders the count
// as a JSON string, but a bare number is tolerated too. Returns (0, false)
// when the policy is empty, unparseable, or omits the field; callers treat
// "unknown" as "cannot verify" and fall back to best-effort behaviour rather
// than blocking startup.
func parseMaxReceiveCount(policy string) (int, bool) {
	if policy == "" {
		return 0, false
	}
	var raw struct {
		MaxReceiveCount any `json:"maxReceiveCount"`
	}
	if err := json.Unmarshal([]byte(policy), &raw); err != nil {
		return 0, false
	}
	switch v := raw.MaxReceiveCount.(type) {
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// poisonReceiveCountThreshold is the ApproximateReceiveCount at or above
// which a repeatedly-redelivered malformed ("poison") message is escalated
// from a Warn to an Error log (Finding 6). Malformed messages are dropped
// WITHOUT a Delete so the queue's own maxReceiveCount redrive policy can
// move them to a DLQ; with NO redrive policy the message reappears every
// visibility timeout forever, so crossing this bound strongly indicates a
// missing redrive policy. 10 mirrors the AWS console's default suggested
// maxReceiveCount.
const poisonReceiveCountThreshold = 10

// approximateReceiveCount parses the SQS ApproximateReceiveCount system
// attribute (the number of times this message has been delivered). Returns
// 0 when the attribute is absent or unparseable.
func approximateReceiveCount(sysAttrs map[string]string) int {
	if v, ok := sysAttrs["ApproximateReceiveCount"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
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
//
// Bridge-to-bridge identity: a peer bridge's egress deliberately propagates
// HeaderIdempotencyKey as the x-bridge.idempotency-key SQS message attribute
// — this attribute is bridge-set (see headersToAttributes). The dedup and
// ordering keys, by contrast, are lifted from the message's NATIVE FIFO
// coordinates MessageDeduplicationId / MessageGroupId: a peer bridge sets
// them deliberately (see extractFIFOFields on the egress side), but they are
// the queue's own FIFO fields and are lifted from ANY FIFO producer, not
// only a bridge.
// NewEnvelope strips every x-bridge.* key from untrusted Headers, so those
// values are LIFTED into EnvelopeInput's first-class fields — the trusted,
// anti-spoof-safe path — before the strip; otherwise dedup and ordering
// silently vanish at the receiving hop of a bridge→SQS→bridge topology.
// The lift reads THIS SQS message's own attributes and system fields; the
// SNS-unwrap path does not interact, since a peer bridge never publishes
// via SNS (it sends directly to SQS).
func (r *Receiver) convertMessage(
	ctx context.Context,
	queueURL string,
	msg sqstypes.Message,
) (*messaging.Envelope, string, error) {
	receiptHandle := aws.ToString(msg.ReceiptHandle)
	body := aws.ToString(msg.Body)

	headers := attributesToHeaders(msg.MessageAttributes, msg.Attributes)

	// no fallback to the configured queue name/URL. Subject comes
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
	generatedID := id == ""
	if generatedID {
		id = generateEnvelopeID()
	}

	// Headers go through EnvelopeInput so NewEnvelope's
	// StripReservedHeaders is the single chokepoint for reserved-prefix
	// sanitation.
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
		ID:              id,
		Subject:         subject,
		Payload:         payload,
		Headers:         headers,
		CreatedAt:       createdAt,
		IdempotencyKey:  bridgeAttrString(msg.MessageAttributes, messaging.HeaderIdempotencyKey),
		DeduplicationID: msg.Attributes[string(sqstypes.MessageSystemAttributeNameMessageDeduplicationId)],
		OrderingKey:     msg.Attributes[string(sqstypes.MessageSystemAttributeNameMessageGroupId)],
	}, r.clock().Now())
	if err != nil {
		return nil, receiptHandle, wrapEnvelopeErr(err)
	}
	if generatedID {
		// SQS always supplies a MessageId, so this is the defensive path for a
		// message that arrives without one. Such a message is converted under a
		// FRESH identity on every redelivery; declare that instability
		// (ports.Receiver "Envelope identity") rather than presenting it as a
		// stable key to dedup. SetHeader is the trusted per-key setter — the
		// reserved key would be stripped from EnvelopeInput.Headers.
		env.SetHeader(messaging.HeaderGeneratedID, "true")
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "sqs: converting",
			"queue_url", queueURL,
			"message_id", env.ID(),
			"body_len", len(body),
		)
	}

	return env, receiptHandle, nil
}

// bridgeAttrString reads a reserved bridge-to-bridge header from the message
// attributes as a string (case-insensitive on the x-bridge. namespace,
// consistent with messaging.IsReservedHeader and the amqp10 adapter's
// bridgeHeaderString). Returns "" when absent or not a plain String attribute.
//
// An exact-case match is preferred before the case-insensitive fold scan
// (Finding 12): egress always emits the canonical lower-case key, so when a
// message carries BOTH the canonical key and a case-variant of it, the lift
// is deterministic instead of picking a random winner from Go's randomised
// map iteration. When only case-variants exist the smallest key (byte order)
// wins so the result is still stable.
func bridgeAttrString(attrs map[string]sqstypes.MessageAttributeValue, key string) string {
	if v, ok := attrs[key]; ok {
		return bridgeAttrStringValue(v)
	}
	matchKey, found := "", false
	for k := range attrs {
		if strings.EqualFold(k, key) && (!found || k < matchKey) {
			matchKey, found = k, true
		}
	}
	if found {
		return bridgeAttrStringValue(attrs[matchKey])
	}
	return ""
}

// bridgeAttrStringValue returns the plain-String value of a bridge message
// attribute. Exact "String" match is deliberate: egress emits the
// idempotency key with the plain "String" DataType (see attributeValue in
// acl_outbound.go). SQS custom labels like "String.custom" are intentionally
// NOT accepted — honouring them would widen ingress beyond the shape egress
// produces.
func bridgeAttrStringValue(v sqstypes.MessageAttributeValue) string {
	if aws.ToString(v.DataType) == "String" {
		return aws.ToString(v.StringValue)
	}
	return ""
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
// hasSubject=false so callers leave Envelope.Subject untouched (no
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

	// A resolved `credentials_uri` builds the initial client with static
	// material instead of the ambient SDK chain (Finding 3). Temporary
	// (STS) material is rejected by rebuildSQSClient (Finding 6).
	if r.cfg.InitialCredentials != nil {
		client, err := rebuildSQSClient(ctx, r.cfg.Region, r.cfg.Endpoint, r.cfg.Profile, r.cfg.InitialCredentials)
		if err != nil {
			return err
		}
		r.storeClient(client)
	} else {
		cfg, err := buildAWSConfig(ctx, r.cfg.Region, r.cfg.Endpoint, r.cfg.Profile)
		if err != nil {
			return err
		}
		r.storeClient(awssqs.NewFromConfig(cfg))
	}

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "sqs: receiver initialized",
			"region", r.cfg.Region,
			"endpoint", r.cfg.Endpoint,
		)
	}

	return nil
}
