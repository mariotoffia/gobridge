package sqs

import (
	"context"
	"crypto/md5"
	"encoding/hex"
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

// sendOne builds the SDK SendMessageInput, issues SendMessage and emits
// the per-call latency metric. All SDK contact for single-message send
// is concentrated here so sender.go can stay SDK-free.
func (s *Sender) sendOne(ctx context.Context, env *messaging.Envelope) error {
	input := s.buildSendInput(env)

	start := s.clock().Now()
	_, err := s.client.SendMessage(ctx, input)
	if err != nil {
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "sqs: send failed",
				"queue_url", s.queueURL, "error", err)
		}
		return MapError(err)
	}

	s.metrics.Timer(shared.MetricSQSSendLatency, s.clock().Since(start),
		shared.Tag{Key: shared.TagKeyQueueURL, Value: s.queueURL})

	return nil
}

// sendBatchChunk sends one chunk of envelopes (≤ BatchSize) in a single
// SDK SendMessageBatch call. It returns the count of successful sends
// and any per-entry / call-level errors. The chunk-level timeout is
// owned here so sender.go does not need the SDK or the input types.
func (s *Sender) sendBatchChunk(
	ctx context.Context,
	batch []*messaging.Envelope,
) (int, []error) {
	entries := make([]sqstypes.SendMessageBatchRequestEntry, 0, len(batch))
	for j, env := range batch {
		entries = append(entries, s.buildBatchEntry(j, env))
	}

	batchCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)

	start := s.clock().Now()
	result, err := s.client.SendMessageBatch(batchCtx, &awssqs.SendMessageBatchInput{
		QueueUrl: aws.String(s.queueURL),
		Entries:  entries,
	})
	cancel()

	if err != nil {
		return 0, []error{MapError(err)}
	}

	s.metrics.Timer(shared.MetricSQSSendBatchLatency, s.clock().Since(start),
		shared.Tag{Key: shared.TagKeyQueueURL, Value: s.queueURL})

	sent := len(result.Successful)

	if len(result.Failed) == 0 {
		return sent, nil
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "sqs: batch partial failure",
			"queue_url", s.queueURL,
			"sent", sent,
			"failed", len(result.Failed),
		)
	}

	errs := make([]error, 0, len(result.Failed))
	for _, f := range result.Failed {
		base := shared.ErrUnavailable
		if f.SenderFault {
			base = shared.ErrInvalidPayload
		}
		errs = append(errs, base.
			Wrap(fmt.Errorf("sqs batch entry %s failed: %s",
				derefStr(f.Id), derefStr(f.Message))).
			With("code", derefStr(f.Code)).
			With("sender_fault", f.SenderFault))
	}
	return sent, errs
}

func (s *Sender) buildSendInput(env *messaging.Envelope) *awssqs.SendMessageInput {
	input := &awssqs.SendMessageInput{
		QueueUrl:    aws.String(s.queueURL),
		MessageBody: aws.String(string(env.Payload)),
	}

	if s.cfg.DelaySeconds > 0 {
		input.DelaySeconds = s.cfg.DelaySeconds
	}

	attrs := headersToAttributes(env.Headers())
	if env.Subject() != "" {
		if attrs == nil {
			attrs = make(map[string]sqstypes.MessageAttributeValue, 1)
		}
		attrs["Subject"] = sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(env.Subject()),
		}
	}
	if len(attrs) > 0 {
		input.MessageAttributes = attrs
	}

	s.applyFIFO(input, env)

	return input
}

func (s *Sender) buildBatchEntry(idx int, env *messaging.Envelope) sqstypes.SendMessageBatchRequestEntry {
	entry := sqstypes.SendMessageBatchRequestEntry{
		Id:          aws.String(strconv.Itoa(idx)),
		MessageBody: aws.String(string(env.Payload)),
	}

	if s.cfg.DelaySeconds > 0 {
		entry.DelaySeconds = s.cfg.DelaySeconds
	}

	attrs := headersToAttributes(env.Headers())
	if env.Subject() != "" {
		if attrs == nil {
			attrs = make(map[string]sqstypes.MessageAttributeValue, 1)
		}
		attrs["Subject"] = sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(env.Subject()),
		}
	}
	if len(attrs) > 0 {
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

// headersToAttributes converts Envelope headers into SQS message attributes.
// FIFO-specific fields (MessageGroupId, MessageDeduplicationId) are extracted
// via extractFIFOFields separately and must not appear in the attribute map.
func headersToAttributes(headers map[string]any) map[string]sqstypes.MessageAttributeValue {
	if len(headers) == 0 {
		return nil
	}

	attrs := make(map[string]sqstypes.MessageAttributeValue, len(headers))

	for k, v := range headers {
		if k == messaging.HeaderOrderingKey || k == messaging.HeaderDeduplicationID {
			continue
		}
		// Skip SQS system attributes injected by the receiver — they are
		// receiver metadata and must not be forwarded as user attributes.
		if strings.HasPrefix(k, "sqs.") {
			continue
		}
		// Skip bridge-reserved headers; they are injected per-hop and
		// should not consume SQS attribute slots.
		if messaging.IsReservedHeader(k) {
			continue
		}

		switch val := v.(type) {
		case string:
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(val),
			}
		case []byte:
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    aws.String("Binary"),
				BinaryValue: val,
			}
		case int, int32, int64, float32, float64:
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    aws.String("Number"),
				StringValue: aws.String(fmt.Sprintf("%v", val)),
			}
		case time.Time:
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(val.Format(time.RFC3339Nano)),
			}
		}
	}

	if len(attrs) == 0 {
		return nil
	}
	return attrs
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
	h.Write(env.Payload)
	h.Write([]byte(env.Subject()))
	if env.ID != "" {
		h.Write([]byte(env.ID))
	} else {
		h.Write([]byte(env.CreatedAt.String()))
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

	if s.client != nil && s.queueURL != "" {
		return nil
	}

	initCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	if s.client == nil {
		if s.cfg.Client != nil {
			s.client = s.cfg.Client
		} else {
			cfg, err := buildAWSConfig(initCtx, s.cfg.Region, s.cfg.Endpoint, s.cfg.Profile)
			if err != nil {
				return err
			}
			s.client = awssqs.NewFromConfig(cfg)
		}
	}

	url, err := resolveQueueURL(initCtx, s.client, s.cfg.QueueURL, s.cfg.QueueName)
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
