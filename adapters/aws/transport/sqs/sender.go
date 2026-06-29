package sqs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	clk      clock.Clock
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
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System
	}
	return &Sender{
		cfg:      cfg,
		queueURL: cfg.QueueURL,
		logger:   cfg.Logger,
		metrics:  m,
		clk:      clk,
	}, nil
}

func (s *Sender) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

// Send submits a single envelope to SQS.
//
// Address validation: an SQS Sender is bound to a single queue URL
// (resolved lazily via ensureClient from cfg.QueueURL or cfg.QueueName).
// When msg.Address is empty, the configured queue URL is used. A
// non-empty msg.Address must match the resolved s.queueURL exactly;
// any other value is rejected with shared.ErrInvalidTopic without
// contacting the SDK or emitting metrics. Per-message dynamic
// addressing for SQS is explicitly out of scope (see ARCHITECTURE_PLAN
// "Non-Goals"). The logical Envelope.Subject is mapped to the "Subject"
// SQS message attribute by buildSendInput and never selects the queue.
func (s *Sender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	if env == nil {
		return shared.ErrInvalidPayload.WithMessage("sqs: nil envelope")
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	if msg.Address != "" && msg.Address != s.queueURL {
		return shared.ErrInvalidTopic.WithMessage(fmt.Sprintf(
			"sqs: address %q does not match configured queue URL %q",
			msg.Address, s.queueURL))
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqs: sending",
			"queue_url", s.queueURL,
			"envelope_id", env.ID(),
			"payload_len", len(env.Payload()),
		)
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	return s.sendOne(sendCtx, env)
}

// SendBatch sends each envelope in chunks of up to BatchSize via the SQS
// SendMessageBatch API. The whole slice is pre-validated before any SDK
// dispatch: a nil envelope yields shared.ErrInvalidPayload and a
// non-empty address that does not match the resolved queue URL yields
// shared.ErrInvalidTopic; either rejects the entire batch with
// (nil, joined-errs) — fail-fast, no chunk is dispatched. A client setup
// failure likewise returns (nil, err).
//
// Once dispatched, SendBatch returns (results, nil): one BatchResult per
// input message, index-aligned with msgs. SQS reports per-entry success
// and failure, so each result carries nil Err (Successful) or the
// classified per-entry error (Failed); a whole-chunk API error marks
// every entry in that chunk. Chunks continue independently after a
// partial or chunk-level failure. See ports.BatchSender for the contract.
func (s *Sender) SendBatch(ctx context.Context, msgs []ports.OutboundMessage) ([]ports.BatchResult, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}

	var preErrs []error
	for i, m := range msgs {
		if m.Envelope == nil {
			preErrs = append(preErrs, shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
				"sqs: nil envelope at index %d", i)))
			continue
		}
		if m.Address != "" && m.Address != s.queueURL {
			preErrs = append(preErrs, shared.ErrInvalidTopic.WithMessage(fmt.Sprintf(
				"sqs: address %q at index %d does not match configured queue URL %q",
				m.Address, i, s.queueURL)))
		}
	}
	if len(preErrs) > 0 {
		return nil, errors.Join(preErrs...)
	}

	envs := make([]*messaging.Envelope, len(msgs))
	for i, m := range msgs {
		envs[i] = m.Envelope
	}

	results := make([]ports.BatchResult, len(envs))

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

		for _, cr := range s.sendBatchChunk(ctx, batch) {
			results[i+cr.Index] = ports.BatchResult{Index: i + cr.Index, Err: cr.Err}
		}
	}

	return results, nil
}
