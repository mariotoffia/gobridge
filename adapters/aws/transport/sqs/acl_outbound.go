package sqs

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// SQS message-attribute limits enforced on egress (Finding 11). SQS
// rejects an entire SendMessage / SendMessageBatch entry that violates
// any of these, so the adapter caps deterministically instead of letting
// a single oversized envelope fail every send.
const (
	// sqsMaxMessageAttributes is the hard limit on message attributes
	// per SQS message.
	sqsMaxMessageAttributes = 10

	// sqsMaxAttributeNameLen is the maximum length of an attribute name.
	sqsMaxAttributeNameLen = 256

	// sqsMaxMessageBytes is the maximum SQS message size (256 KiB),
	// shared between the body and every attribute name, type and value.
	// Used as a conservative ceiling so a pathological header set cannot
	// build a request SQS would reject for size.
	sqsMaxMessageBytes = 262144
)

// sendOne builds the SDK SendMessageInput, issues SendMessage and emits
// the per-call latency metric. All SDK contact for single-message send
// is concentrated here so sender.go can stay SDK-free.
func (s *Sender) sendOne(ctx context.Context, env *messaging.Envelope) error {
	input := s.buildSendInput(env)

	start := s.clock().Now()
	_, err := s.loadClient().SendMessage(ctx, input)
	if err != nil {
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "sqs: send failed",
				"queue_url", s.queueURL, "error", err)
		}
		return MapError(err)
	}

	s.metrics.Timer(MetricSQSSendLatency, s.clock().Since(start),
		shared.Tag{Key: TagKeyQueueURL, Value: s.queueURL})

	return nil
}

// sendBatchChunk sends one chunk of envelopes (≤ BatchSize) in a single
// SDK SendMessageBatch call. It returns one ports.BatchResult per input
// envelope, keyed by the envelope's index WITHIN the chunk: nil Err for
// entries the broker accepted (SQS Successful), the classified error for
// entries it rejected (SQS Failed), and the call error on every entry
// when the whole SendMessageBatch call fails. The chunk-level timeout is
// owned here so sender.go does not need the SDK or the input types.
func (s *Sender) sendBatchChunk(
	ctx context.Context,
	batch []*messaging.Envelope,
) []ports.BatchResult {
	results := make([]ports.BatchResult, len(batch))
	for j := range batch {
		results[j].Index = j
	}

	entries := make([]sqstypes.SendMessageBatchRequestEntry, 0, len(batch))
	for j, env := range batch {
		entries = append(entries, s.buildBatchEntry(j, env))
	}

	batchCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)

	start := s.clock().Now()
	result, err := s.loadClient().SendMessageBatch(batchCtx, &awssqs.SendMessageBatchInput{
		QueueUrl: aws.String(s.queueURL),
		Entries:  entries,
	})
	cancel()

	if err != nil {
		e := MapError(err)
		for j := range results {
			results[j].Err = e
		}
		return results
	}

	s.metrics.Timer(MetricSQSSendBatchLatency, s.clock().Since(start),
		shared.Tag{Key: TagKeyQueueURL, Value: s.queueURL})

	if len(result.Failed) == 0 {
		return results
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "sqs: batch partial failure",
			"queue_url", s.queueURL,
			"sent", len(result.Successful),
			"failed", len(result.Failed),
		)
	}

	for _, f := range result.Failed {
		idx, convErr := strconv.Atoi(derefStr(f.Id))
		if convErr != nil || idx < 0 || idx >= len(results) {
			continue
		}
		base := shared.ErrUnavailable
		if f.SenderFault {
			base = shared.ErrInvalidPayload
		}
		results[idx].Err = base.
			Wrap(fmt.Errorf("sqs batch entry %s failed: %s",
				derefStr(f.Id), derefStr(f.Message))).
			With("code", derefStr(f.Code)).
			With("sender_fault", f.SenderFault)
	}
	return results
}

func (s *Sender) buildSendInput(env *messaging.Envelope) *awssqs.SendMessageInput {
	input := &awssqs.SendMessageInput{
		QueueUrl:    aws.String(s.queueURL),
		MessageBody: aws.String(string(env.Payload())),
	}

	if s.cfg.DelaySeconds > 0 {
		input.DelaySeconds = s.cfg.DelaySeconds
	}

	if attrs := s.buildAttributes(env); len(attrs) > 0 {
		input.MessageAttributes = attrs
	}

	s.applyFIFO(input, env)

	return input
}

func (s *Sender) buildBatchEntry(idx int, env *messaging.Envelope) sqstypes.SendMessageBatchRequestEntry {
	entry := sqstypes.SendMessageBatchRequestEntry{
		Id:          aws.String(strconv.Itoa(idx)),
		MessageBody: aws.String(string(env.Payload())),
	}

	if s.cfg.DelaySeconds > 0 {
		entry.DelaySeconds = s.cfg.DelaySeconds
	}

	if attrs := s.buildAttributes(env); len(attrs) > 0 {
		entry.MessageAttributes = attrs
	}

	if s.cfg.isFIFO() {
		groupID, dedupID := extractFIFOFields(env.Headers())
		if groupID == "" {
			groupID = s.cfg.MessageGroupID
		}
		if groupID != "" {
			entry.MessageGroupId = aws.String(groupID)
		}
		if dedupID == "" {
			dedupID = generateDeduplicationID(env)
		}
		entry.MessageDeduplicationId = aws.String(dedupID)
	}

	return entry
}

// buildAttributes converts envelope headers to SQS message attributes,
// reserving a slot for the Subject attribute when present so the total
// can never exceed sqsMaxMessageAttributes (Finding 11). Headers dropped
// by the count/size caps are surfaced via a debug log and the
// SQSDroppedAttributes counter so the loss is observable.
func (s *Sender) buildAttributes(env *messaging.Envelope) map[string]sqstypes.MessageAttributeValue {
	budget := sqsMaxMessageAttributes
	hasSubject := env.Subject() != ""
	if hasSubject {
		budget-- // reserve the Subject slot added below
	}

	attrs, dropped := headersToAttributes(env.Headers(), budget)

	if hasSubject {
		if attrs == nil {
			attrs = make(map[string]sqstypes.MessageAttributeValue, 1)
		}
		attrs["Subject"] = sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(env.Subject()),
		}
	}

	if dropped > 0 {
		s.metrics.Counter(MetricSQSDroppedAttributes, int64(dropped),
			shared.Tag{Key: TagKeyQueueURL, Value: s.queueURL})
		logging.Debug(s.logger, "sqs: dropped message attributes over SQS limits",
			"queue_url", s.queueURL,
			"dropped", dropped,
			"envelope_id", env.ID(),
		)
	}

	return attrs
}

func (s *Sender) applyFIFO(input *awssqs.SendMessageInput, env *messaging.Envelope) {
	if !s.cfg.isFIFO() {
		return
	}

	groupID, dedupID := extractFIFOFields(env.Headers())
	if groupID == "" {
		groupID = s.cfg.MessageGroupID
	}
	if groupID != "" {
		input.MessageGroupId = aws.String(groupID)
	}

	if dedupID == "" {
		dedupID = generateDeduplicationID(env)
	}
	input.MessageDeduplicationId = aws.String(dedupID)
}

// headersToAttributes converts envelope headers into SQS message
// attributes, enforcing SQS limits deterministically (Findings 7 & 11):
//
//   - INTERNAL-ONLY reserved headers (route-id, route-override,
//     source-id, content-type) are stripped — they are bridge dispatch
//     bookkeeping and must not leak to a consumer. BRIDGE-TO-BRIDGE
//     reserved headers (correlation/causation/idempotency/tenant/
//     forwarded/trace) are preserved AND prioritized: when a cap forces a
//     drop they are kept ahead of application headers so a peer bridge can
//     still correlate and deduplicate across a hop (Finding 7).
//   - FIFO ordering/dedup headers are skipped; they map to the native
//     MessageGroupId / MessageDeduplicationId fields.
//   - sqs.* receiver-injected system headers are skipped.
//   - Names SQS would reject (charset, length, reserved AWS./Amazon.
//     prefix, leading/trailing/consecutive periods) are dropped, since a
//     single bad name fails the whole send.
//   - At most maxAttrs attributes are emitted. Eligible keys are ranked
//     bridge-to-bridge first then by name, and the highest-ranked maxAttrs
//     kept, so selection is deterministic and never sacrifices a
//     propagation header for application metadata.
//   - Cumulative *attribute* size is capped at sqsMaxMessageBytes. The
//     message body shares SQS's 256 KiB budget and is NOT counted here, so
//     a body-dominant oversize is surfaced by SQS (MapError), not pre-capped.
//
// It returns the attribute map (nil when empty) and the number of
// eligible headers dropped by the count/size caps so the caller can
// surface the loss. Name-invalid and unsupported-type headers are not
// counted — they could never be SQS attributes.
func headersToAttributes(headers map[string]any, maxAttrs int) (map[string]sqstypes.MessageAttributeValue, int) {
	if len(headers) == 0 || maxAttrs <= 0 {
		return nil, 0
	}

	type candidate struct {
		name   string
		value  sqstypes.MessageAttributeValue
		size   int
		bridge bool
	}

	eligible := make([]candidate, 0, len(headers))
	for k, v := range headers {
		// FIFO fields map to native SQS message fields, not attributes.
		if k == messaging.HeaderOrderingKey || k == messaging.HeaderDeduplicationID {
			continue
		}
		// Receiver-injected SQS system metadata is not user attribute data.
		if strings.HasPrefix(k, "sqs.") {
			continue
		}
		// Strip ONLY internal-only reserved headers; bridge-to-bridge
		// reserved headers are preserved (Finding 7).
		if messaging.IsInternalOnlyHeader(k) {
			continue
		}
		if !isValidSQSAttributeName(k) {
			continue
		}
		av, sz, ok := attributeValue(v)
		if !ok {
			continue
		}
		eligible = append(eligible, candidate{
			name:   k,
			value:  av,
			size:   len(k) + sz,
			bridge: messaging.IsBridgeToBridgeHeader(k),
		})
	}

	if len(eligible) == 0 {
		return nil, 0
	}

	// Rank bridge-to-bridge propagation headers (idempotency / correlation
	// / causation / tenant / forwarded / trace) first so that when the
	// count or size cap forces a drop, application metadata is sacrificed
	// before a header a peer bridge needs to deduplicate or correlate
	// across the hop. Within a class, sort by name so selection is
	// deterministic regardless of Go's randomised map iteration order.
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].bridge != eligible[j].bridge {
			return eligible[i].bridge
		}
		return eligible[i].name < eligible[j].name
	})

	attrs := make(map[string]sqstypes.MessageAttributeValue, min(len(eligible), maxAttrs))
	dropped := 0
	sizeBytes := 0
	for _, c := range eligible {
		if len(attrs) >= maxAttrs {
			dropped++
			continue
		}
		if sizeBytes+c.size > sqsMaxMessageBytes {
			dropped++
			continue
		}
		attrs[c.name] = c.value
		sizeBytes += c.size
	}

	if len(attrs) == 0 {
		return nil, dropped
	}
	return attrs, dropped
}

// attributeValue builds the SQS MessageAttributeValue for a header value
// and reports its approximate byte size (type name + value bytes). It
// returns ok=false for value types SQS cannot carry as an attribute.
func attributeValue(v any) (sqstypes.MessageAttributeValue, int, bool) {
	switch val := v.(type) {
	case string:
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(val),
		}, len("String") + len(val), true
	case []byte:
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("Binary"),
			BinaryValue: val,
		}, len("Binary") + len(val), true
	case int, int32, int64, float32, float64:
		s := fmt.Sprintf("%v", val)
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("Number"),
			StringValue: aws.String(s),
		}, len("Number") + len(s), true
	case time.Time:
		s := val.Format(time.RFC3339Nano)
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(s),
		}, len("String") + len(s), true
	case bool:
		s := fmt.Sprintf("%t", val)
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(s),
		}, len("String") + len(s), true
	default:
		return sqstypes.MessageAttributeValue{}, 0, false
	}
}

// isValidSQSAttributeName reports whether name is a legal SQS message
// attribute name: 1-256 chars from [A-Za-z0-9_.-], no AWS./Amazon.
// (case-insensitive) reserved prefix, and no leading, trailing or
// consecutive periods.
func isValidSQSAttributeName(name string) bool {
	if name == "" || len(name) > sqsMaxAttributeNameLen {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return false
	}
	if hasFoldPrefix(name, "aws.") || hasFoldPrefix(name, "amazon.") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// hasFoldPrefix reports whether s starts with prefix, case-insensitively.
func hasFoldPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// extractFIFOFields pulls MessageGroupId and MessageDeduplicationId from
// envelope headers. Returns empty strings when not present.
func extractFIFOFields(headers map[string]any) (groupID, dedupID string) {
	if headers == nil {
		return "", ""
	}
	if v, ok := headers[messaging.HeaderOrderingKey]; ok {
		if s, ok := v.(string); ok {
			groupID = s
		}
	}
	if v, ok := headers[messaging.HeaderDeduplicationID]; ok {
		if s, ok := v.(string); ok {
			dedupID = s
		}
	}
	return groupID, dedupID
}

// generateDeduplicationID derives a stable FIFO dedup id from the
// envelope payload, subject and id. md5 is sufficient — SQS only uses
// the value as an opaque key for dedup, not for security.
//
// T08 review: Subject is now a logical event subject (no longer
// implicitly populated from the queue name/URL on receive) and may be
// empty. Mixing it into the hash is benign: when env.ID is set it is
// the primary disambiguator, so distinct logical messages do not
// collide just because they share an empty Subject. Conversely, two
// envelopes that share payload+id+subject deliberately collide so
// SQS dedup treats them as duplicates. When env.ID is empty the
// CreatedAt timestamp keeps each call unique. No semantic change is
// required for T08.
func generateDeduplicationID(env *messaging.Envelope) string {
	h := md5.New()
	h.Write(env.Payload())
	h.Write([]byte(env.Subject()))
	if env.ID() != "" {
		h.Write([]byte(env.ID()))
	} else {
		h.Write([]byte(env.CreatedAt().String()))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ensureClient lazily creates the SDK SQS client for the sender and
// resolves the queue URL. Honours an injected fake (cfg.Client) when
// present.
func (s *Sender) ensureClient(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()

	client := s.loadClient()
	if client != nil && s.queueURL != "" {
		return nil
	}

	initCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	if client == nil {
		if s.cfg.Client != nil {
			client = s.cfg.Client
		} else {
			cfg, err := buildAWSConfig(initCtx, s.cfg.Region, s.cfg.Endpoint, s.cfg.Profile)
			if err != nil {
				return err
			}
			client = awssqs.NewFromConfig(cfg)
		}
		s.storeClient(client)
	}

	url, err := resolveQueueURL(initCtx, client, s.cfg.QueueURL, s.cfg.QueueName)
	if err != nil {
		return err
	}
	s.queueURL = url

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "sqs: sender initialized",
			"queue_url", s.queueURL,
			"region", s.cfg.Region,
		)
	}

	return nil
}
