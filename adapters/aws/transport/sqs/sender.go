package sqs

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time checks.
var (
	_ ports.Sender      = (*Sender)(nil)
	_ ports.BatchSender = (*Sender)(nil)
)

// Sender implements ports.Sender and ports.BatchSender for Amazon SQS.
type Sender struct {
	cfg      SenderConfig
	client   sqsAPI
	queueURL string
	initMu   sync.Mutex
	logger   *slog.Logger
	metrics  ports.MetricsExporter
}

// NewSender creates an SQS Sender. The sender resolves its queue URL
// lazily on the first Send call unless QueueURL is already set.
func NewSender(cfg SenderConfig) (*Sender, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	return &Sender{
		cfg:      cfg,
		queueURL: cfg.QueueURL,
		logger:   cfg.Logger,
		metrics:  m,
	}, nil
}

// Send submits a single envelope to SQS.
func (s *Sender) Send(ctx context.Context, env *domain.Envelope) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqs: sending",
			"queue_url", s.queueURL,
			"envelope_id", env.ID,
			"payload_len", len(env.Payload),
		)
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	input := s.buildSendInput(env)

	start := time.Now()
	_, err := s.client.SendMessage(sendCtx, input)
	if err != nil {
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "sqs: send failed",
				"queue_url", s.queueURL, "error", err)
		}
		return MapError(err)
	}

	s.metrics.Timer(domain.MetricSQSSendLatency, time.Since(start),
		domain.Tag{Key: domain.TagKeyQueueURL, Value: s.queueURL})

	return nil
}

// SendBatch sends multiple envelopes in batches of up to 10.
// Returns the number of successfully sent messages. When some batches
// fail (partial failures or API errors), the method continues sending
// the remaining batches and returns a combined error with the total
// successful count.
func (s *Sender) SendBatch(ctx context.Context, envs []*domain.Envelope) (int, error) {
	if err := s.ensureClient(ctx); err != nil {
		return 0, err
	}

	var (
		sent int
		errs []error
	)

	for i := 0; i < len(envs); i += s.cfg.BatchSize {
		end := i + s.cfg.BatchSize
		if end > len(envs) {
			end = len(envs)
		}
		batch := envs[i:end]

		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "sqs: sending batch",
				"queue_url", s.queueURL,
				"chunk_size", len(batch),
				"chunk_offset", i,
			)
		}

		entries := make([]sqstypes.SendMessageBatchRequestEntry, 0, len(batch))
		for j, env := range batch {
			entry := s.buildBatchEntry(j, env)
			entries = append(entries, entry)
		}

		batchCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)

		start := time.Now()
		result, err := s.client.SendMessageBatch(batchCtx, &awssqs.SendMessageBatchInput{
			QueueUrl: aws.String(s.queueURL),
			Entries:  entries,
		})

		cancel()

		if err != nil {
			errs = append(errs, MapError(err))
			continue
		}

		s.metrics.Timer(domain.MetricSQSSendBatchLatency, time.Since(start),
			domain.Tag{Key: domain.TagKeyQueueURL, Value: s.queueURL})

		sent += len(result.Successful)

		if len(result.Failed) > 0 {
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(ctx, logging.LevelDebug, "sqs: batch partial failure",
					"queue_url", s.queueURL,
					"sent", len(result.Successful),
					"failed", len(result.Failed),
				)
			}
			for _, f := range result.Failed {
				base := domain.ErrUnavailable
				if f.SenderFault {
					base = domain.ErrInvalidPayload
				}
				errs = append(errs, base.
					Wrap(fmt.Errorf("sqs batch entry %s failed: %s",
						derefStr(f.Id), derefStr(f.Message))).
					With("code", derefStr(f.Code)).
					With("sender_fault", f.SenderFault))
			}
		}
	}

	if len(errs) > 0 {
		return sent, errors.Join(errs...)
	}

	return sent, nil
}

func (s *Sender) buildSendInput(env *domain.Envelope) *awssqs.SendMessageInput {
	input := &awssqs.SendMessageInput{
		QueueUrl:    aws.String(s.queueURL),
		MessageBody: aws.String(string(env.Payload)),
	}

	if s.cfg.DelaySeconds > 0 {
		input.DelaySeconds = s.cfg.DelaySeconds
	}

	attrs := headersToAttributes(env.Headers)
	if env.Subject != "" {
		if attrs == nil {
			attrs = make(map[string]sqstypes.MessageAttributeValue, 1)
		}
		attrs["Subject"] = sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(env.Subject),
		}
	}
	if len(attrs) > 0 {
		input.MessageAttributes = attrs
	}

	s.applyFIFO(input, env)

	return input
}

func (s *Sender) buildBatchEntry(idx int, env *domain.Envelope) sqstypes.SendMessageBatchRequestEntry {
	entry := sqstypes.SendMessageBatchRequestEntry{
		Id:          aws.String(strconv.Itoa(idx)),
		MessageBody: aws.String(string(env.Payload)),
	}

	if s.cfg.DelaySeconds > 0 {
		entry.DelaySeconds = s.cfg.DelaySeconds
	}

	attrs := headersToAttributes(env.Headers)
	if env.Subject != "" {
		if attrs == nil {
			attrs = make(map[string]sqstypes.MessageAttributeValue, 1)
		}
		attrs["Subject"] = sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(env.Subject),
		}
	}
	if len(attrs) > 0 {
		entry.MessageAttributes = attrs
	}

	if s.cfg.isFIFO() {
		groupID, dedupID := extractFIFOFields(env.Headers)
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

func (s *Sender) applyFIFO(input *awssqs.SendMessageInput, env *domain.Envelope) {
	if !s.cfg.isFIFO() {
		return
	}

	groupID, dedupID := extractFIFOFields(env.Headers)
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
				return domain.ErrUnavailable.Wrap(fmt.Errorf("sqs sender: build AWS config: %w", err))
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

func generateDeduplicationID(env *domain.Envelope) string {
	h := md5.New()
	h.Write(env.Payload)
	h.Write([]byte(env.Subject))
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
