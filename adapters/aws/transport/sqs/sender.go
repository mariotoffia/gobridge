package sqs

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
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
func (s *Sender) Send(ctx context.Context, env *messaging.Envelope) error {
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

	return s.sendOne(sendCtx, env)
}

// SendBatch sends multiple envelopes in batches of up to BatchSize.
// Returns the number of successfully sent messages. When some batches
// fail (partial failures or API errors), the method continues sending
// the remaining batches and returns a combined error with the total
// successful count.
func (s *Sender) SendBatch(ctx context.Context, envs []*messaging.Envelope) (int, error) {
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

		n, batchErrs := s.sendBatchChunk(ctx, batch)
		sent += n
		errs = append(errs, batchErrs...)
	}

	if len(errs) > 0 {
		return sent, errors.Join(errs...)
	}

	return sent, nil
}
