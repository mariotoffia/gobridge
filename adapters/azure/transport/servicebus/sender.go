package servicebus

import (
	"context"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.Sender      = (*Sender)(nil)
	_ ports.BatchSender = (*Sender)(nil)
)

// Sender implements ports.Sender and ports.BatchSender for Azure
// Service Bus. SDK access is concentrated in acl_*.go: this file
// references only the unexported asbSenderAPI seam and the
// *asbClientHandle wrapper.
type Sender struct {
	cfg       SenderConfig
	client    asbSenderAPI
	asbClient *asbClientHandle
	initMu    sync.Mutex
	logger    *slog.Logger
	metrics   ports.MetricsExporter
	clk       clock.Clock
}

// NewSender creates a Service Bus Sender. The underlying AMQP connection
// is established lazily on the first Send call unless cfg.Client is
// injected for testing.
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
	return &Sender{cfg: cfg, client: cfg.Client, logger: cfg.Logger, metrics: m, clk: clk}, nil
}

func (s *Sender) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

func (s *Sender) entityName() string {
	if s.cfg.QueueName != "" {
		return s.cfg.QueueName
	}
	return s.cfg.TopicName
}

// Send submits a single envelope to Service Bus.
func (s *Sender) Send(ctx context.Context, env *messaging.Envelope) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "servicebus: sending",
			"entity", s.entityName(),
			"envelope_id", env.ID,
			"payload_len", len(env.Payload),
		)
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	start := s.clock().Now()
	if err := sendOne(sendCtx, s.client, env, s.cfg.DefaultSessionID, s.clock()); err != nil {
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "servicebus: send failed",
				"entity", s.entityName(), "error", err)
		}
		return err
	}

	s.metrics.Timer(shared.MetricASBSendLatency, s.clock().Since(start),
		shared.Tag{Key: shared.TagKeyEntity, Value: s.entityName()})

	return nil
}

// SendBatch sends multiple envelopes in batches of up to cfg.BatchSize.
// ASB batches are size-limited; when a message overflows the batch, the
// current batch is flushed and the oversized message is sent individually.
// Returns the number of successfully sent messages.
func (s *Sender) SendBatch(ctx context.Context, envs []*messaging.Envelope) (int, error) {
	if err := s.ensureClient(ctx); err != nil {
		return 0, err
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	var sent int

	for i := 0; i < len(envs); i += s.cfg.BatchSize {
		end := i + s.cfg.BatchSize
		if end > len(envs) {
			end = len(envs)
		}
		chunk := envs[i:end]

		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "servicebus: sending batch",
				"entity", s.entityName(),
				"chunk_size", len(chunk),
			)
		}

		start := s.clock().Now()
		chunkSent, err := sendChunk(sendCtx, s.client, chunk, s.cfg.DefaultSessionID, s.clock(), s.logger, s.entityName())
		sent += chunkSent
		if err != nil {
			return sent, err
		}

		s.metrics.Timer(shared.MetricASBSendBatchLatency, s.clock().Since(start),
			shared.Tag{Key: shared.TagKeyEntity, Value: s.entityName()})
	}

	return sent, nil
}

// Close tears down the Service Bus sender and the underlying AMQP connection.
func (s *Sender) Close(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "servicebus: sender closing",
			"entity", s.entityName())
	}

	var firstErr error
	if s.client != nil {
		firstErr = s.client.Close(ctx)
	}
	if s.asbClient != nil {
		if err := s.asbClient.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Sender) ensureClient(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()

	if s.client != nil {
		return nil
	}

	asbClient, err := buildClient(s.cfg.Connection)
	if err != nil {
		return err
	}

	entityName := s.entityName()

	sender, err := asbClient.NewSender(entityName)
	if err != nil {
		_ = asbClient.Close(ctx)
		return MapError(err)
	}

	s.client = sender
	s.asbClient = asbClient

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "servicebus: sender initialized",
			"entity", entityName)
	}

	return nil
}
