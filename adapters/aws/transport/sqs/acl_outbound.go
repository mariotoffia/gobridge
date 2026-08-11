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

	// sqsMaxMessageBytes is the DEFAULT maximum SQS message size (256 KiB),
	// shared between the body and every attribute name, type and value.
	// Used as a conservative ceiling so a pathological header set cannot
	// build a request SQS would reject for size. Queues configured with a
	// larger MaximumMessageSize can raise it via WithMaxMessageBytes so an
	// oversized body does not silently drop all attributes (Finding 4).
	sqsMaxMessageBytes = 262144

	// sqsSubjectAttributeName is the reserved SQS message-attribute name
	// carrying the envelope Subject. buildAttributes writes it from
	// env.Subject(); headersToAttributes skips any same-named header so the
	// reserved slot and a stray "Subject" header cannot double-charge the
	// attribute budget (Finding 7).
	sqsSubjectAttributeName = "Subject"
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
		// Route through the auth grace so a transient static-key rotation /
		// IAM-propagation window classifies temporary (retryable) instead of
		// permanent (Finding: c8-auth-permanent). classify ALSO reports a
		// permanent authorization failure to the reactive-recovery hook
		// so a hard key revocation forces an immediate re-resolve.
		return s.classify(err)
	}

	// A successful send authenticated: end any pending auth-failure streak so
	// a later rotation gap gets a fresh grace window.
	s.authGrace.reset()
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
		// A whole-batch auth failure gets the same bounded grace as a
		// single send (Finding: c8-auth-permanent) and reports a permanent
		// authorization failure to the reactive-recovery hook.
		e := s.classify(err)
		for j := range results {
			results[j].Err = e
		}
		return results
	}

	// The batch call itself authenticated: reset the auth-failure streak even
	// when individual entries were rejected (those are per-entry SenderFaults,
	// not an auth condition).
	s.authGrace.reset()
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
		// Classify the failed-entry Code with the SAME policy as MapError
		// (KMS + throttling + service codes) BEFORE falling back to the
		// SenderFault verdict, so a per-entry retryable target outage (KMS
		// grant still propagating, KMS/request throttling, a transient
		// InternalError) stays retryable instead of becoming a terminal reject
		// that costs the source its retry (Chunk 13). A Code outside
		// that set falls back to the SenderFault verdict: a request the caller
		// malformed is rejected, anything else is treated as transient.
		base, matched := classifyBatchEntryCode(derefStr(f.Code))
		if !matched {
			base = shared.ErrUnavailable
			if f.SenderFault {
				base = shared.ErrInvalidPayload
			}
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
	maxBytes := s.maxMessageBytes
	if maxBytes <= 0 {
		maxBytes = sqsMaxMessageBytes
	}

	budget := sqsMaxMessageAttributes
	hasSubject := env.Subject() != ""
	// Seed the size budget with the body AND — when a Subject attribute is
	// reserved below — the Subject's own bytes, BEFORE attribute selection
	// (Finding 4). The Subject is appended AFTER the budget loop, so a body
	// just under the ceiling could otherwise be pushed over the real broker
	// limit by the Subject bytes that were never charged.
	seedBytes := len(env.Payload())
	if hasSubject {
		budget-- // reserve the Subject slot added below
		seedBytes += subjectAttributeSize(env.Subject())
	}

	attrs, dropped := headersToAttributes(env.Headers(), budget, seedBytes, maxBytes)

	if hasSubject {
		if attrs == nil {
			attrs = make(map[string]sqstypes.MessageAttributeValue, 1)
		}
		attrs[sqsSubjectAttributeName] = sqstypes.MessageAttributeValue{
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

// egressAttributeRank orders eligible headers when the SQS attribute
// count/size caps force a drop. Rationale: a peer bridge's SQS ingress
// strips reserved x-bridge.* attributes unconditionally (see
// attributesToHeaders and the messaging.NewEnvelope chokepoint), so
// keeping every bridge header ahead of application data sacrifices real
// payload metadata for headers the next hop discards. Only the truly
// required propagation headers outrank application data:
//
//	rank 0 — essential propagation: traceparent / tracestate (W3C keys,
//	         NOT x-bridge.*-prefixed, so they survive a peer bridge's
//	         reserved-header strip) and x-bridge.idempotency-key (the
//	         cross-hop identity/dedup contract). The ordering/dedup
//	         keys ride the native MessageGroupId/MessageDeduplicationId
//	         fields and never compete for an attribute slot.
//	rank 1 — application headers: the payload's own metadata.
//	rank 2 — remaining bridge-to-bridge reserved headers (correlation-id,
//	         causation-id, tenant-id, forwarded-from/hop): best-effort,
//	         sacrificed first under cap pressure.
func egressAttributeRank(name string) int {
	switch {
	case strings.EqualFold(name, messaging.HeaderTraceParent),
		strings.EqualFold(name, messaging.HeaderTraceState),
		strings.EqualFold(name, messaging.HeaderIdempotencyKey):
		return 0
	case messaging.IsBridgeToBridgeHeader(name):
		return 2
	default:
		return 1
	}
}

// headersToAttributes converts envelope headers into SQS message
// attributes, enforcing SQS limits deterministically:
//
//   - INTERNAL-ONLY reserved headers (route-id, route-override,
//     source-id, content-type) are stripped — they are bridge dispatch
//     bookkeeping and must not leak to a consumer. BRIDGE-TO-BRIDGE
//     reserved headers (correlation/causation/idempotency/tenant/
//     forwarded/trace) are preserved as attributes.
//   - FIFO ordering/dedup headers are skipped; they map to the native
//     MessageGroupId / MessageDeduplicationId fields.
//   - sqs.* receiver-injected system headers are skipped.
//   - Names SQS would reject (charset, length, reserved AWS./Amazon.
//     prefix, leading/trailing/consecutive periods) are dropped, since a
//     single bad name fails the whole send.
//   - At most maxAttrs attributes are emitted. Eligible keys are ranked
//     by egressAttributeRank (essential propagation first, application
//     headers next, other bridge headers last) then by name, and the
//     highest-ranked maxAttrs kept — selection is deterministic and
//     never sacrifices application data for a bridge header the next
//     hop's ingress strips anyway.
//   - Cumulative size (message body + each attribute's name, type and
//     value) is capped at sqsMaxMessageBytes. bodyBytes seeds the size
//     accumulator so the 256 KiB budget SQS charges across body AND
//     attributes is respected: an attribute that would push the
//     body+attribute total over the ceiling is dropped (and counted)
//     here instead of failing the whole send at SQS (MapError).
//
// It returns the attribute map (nil when empty) and the number of
// eligible headers dropped by the count/size caps so the caller can
// surface the loss. Name-invalid and unsupported-type headers are not
// counted — they could never be SQS attributes.
func headersToAttributes(headers map[string]any, maxAttrs int, seedBytes int, maxBytes int) (map[string]sqstypes.MessageAttributeValue, int) {
	if len(headers) == 0 || maxAttrs <= 0 {
		return nil, 0
	}
	if maxBytes <= 0 {
		maxBytes = sqsMaxMessageBytes
	}

	type candidate struct {
		name  string
		value sqstypes.MessageAttributeValue
		size  int
		rank  int
	}

	eligible := make([]candidate, 0, len(headers))
	for k, v := range headers {
		// FIFO fields map to native SQS message fields, not attributes.
		if k == messaging.HeaderOrderingKey || k == messaging.HeaderDeduplicationID {
			continue
		}
		// The Subject attribute is reserved and written separately by
		// buildAttributes from env.Subject(). A stray "Subject" header (kept
		// as a plain header by SQS->SQS ingress) must NOT also compete for a
		// budget slot: it would double-charge the 10-attribute limit and the
		// reserved write would overwrite it, dropping a real application
		// header on a relay carrying >=10 headers (Finding 7).
		if k == sqsSubjectAttributeName {
			continue
		}
		// Receiver-injected SQS system metadata is not user attribute data.
		if strings.HasPrefix(k, "sqs.") {
			continue
		}
		// Strip ONLY internal-only reserved headers; bridge-to-bridge
		// reserved headers are preserved.
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
			name:  k,
			value: av,
			size:  len(k) + sz,
			rank:  egressAttributeRank(k),
		})
	}

	if len(eligible) == 0 {
		return nil, 0
	}

	// Rank per egressAttributeRank: essential propagation headers
	// (idempotency-key, traceparent/tracestate) first, application
	// headers next, remaining bridge-to-bridge reserved headers last —
	// they are stripped by a peer bridge's ingress, so they must not
	// displace real application data under cap pressure. Within a rank,
	// sort by name so selection is deterministic regardless of Go's
	// randomised map iteration order.
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].rank != eligible[j].rank {
			return eligible[i].rank < eligible[j].rank
		}
		return eligible[i].name < eligible[j].name
	})

	attrs := make(map[string]sqstypes.MessageAttributeValue, min(len(eligible), maxAttrs))
	dropped := 0
	// Seed with the pre-charged body (and reserved-Subject) size: SQS
	// charges its message-size ceiling against the body and attributes
	// together, so attributes are measured against the budget the body —
	// and the Subject appended after this loop — already consume.
	sizeBytes := seedBytes
	for _, c := range eligible {
		if len(attrs) >= maxAttrs {
			dropped++
			continue
		}
		if sizeBytes+c.size > maxBytes {
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

// subjectAttributeSize is the byte size the reserved "Subject" attribute
// contributes to the SQS message-size budget: attribute name + "String"
// data type + subject value, mirroring the name-inclusive accounting
// headersToAttributes applies to every other candidate (Finding 4).
func subjectAttributeSize(subject string) int {
	return len(sqsSubjectAttributeName) + len("String") + len(subject)
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
// review: Subject is now a logical event subject (no longer
// implicitly populated from the queue name/URL on receive) and may be
// empty. Mixing it into the hash is benign: when env.ID is set it is
// the primary disambiguator, so distinct logical messages do not
// collide just because they share an empty Subject. Conversely, two
// envelopes that share payload+id+subject deliberately collide so
// SQS dedup treats them as duplicates. When env.ID is empty the
// CreatedAt timestamp keeps each call unique. No semantic change is
// required.
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
		} else if s.cfg.InitialCredentials != nil {
			// A resolved `credentials_uri` builds the initial client with
			// static material instead of the ambient SDK chain (Finding 3).
			// Temporary (STS) material is rejected here (Finding 6).
			c, err := rebuildSQSClient(initCtx, s.cfg.Region, s.cfg.Endpoint, s.cfg.Profile, s.cfg.InitialCredentials)
			if err != nil {
				return err
			}
			client = c
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
