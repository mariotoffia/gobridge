package servicebus

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

var (
	_ ports.Sender      = (*Sender)(nil)
	_ ports.BatchSender = (*Sender)(nil)
)

// Sender implements ports.Sender and ports.BatchSender for Azure
// Service Bus. SDK access is concentrated in acl_*.go: this file
// references only the unexported asbSenderAPI seam and the
// *asbClientHandle wrapper.
//
// client / asbClient / cfg.Connection are guarded by initMu: a live
// credential rotation (ApplyCredentials) swaps them while Send /
// SendBatch may be in flight. Callers snapshot via currentClient();
// an in-flight send finishes against the old link.
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

// currentClient returns the live sender seam under the swap lock so a
// concurrent ApplyCredentials rotation is never observed half-applied.
//
//nolint:ireturn // asbSenderAPI is an adapter-internal mock seam (category 5).
func (s *Sender) currentClient() asbSenderAPI {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	return s.client
}

// swapClient installs a new sender seam + client handle + connection
// under the swap lock and returns the previous pair so the caller can
// close it OUTSIDE the lock.
//
//nolint:ireturn // asbSenderAPI is an adapter-internal mock seam (category 5).
func (s *Sender) swapClient(next asbSenderAPI, nextHandle *asbClientHandle, nextConn ConnectionConfig) (asbSenderAPI, *asbClientHandle) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	oldClient, oldHandle := s.client, s.asbClient
	s.client = next
	s.asbClient = nextHandle
	s.cfg.Connection = nextConn
	return oldClient, oldHandle
}

// Send submits a single envelope to Service Bus.
//
// Address validation: a Service Bus Sender is bound to a single
// queue or topic (resolved via cfg.QueueName / cfg.TopicName). When
// msg.Address is empty, the configured entity is used. A non-empty
// msg.Address must match s.entityName() exactly; any other value is
// rejected with shared.ErrInvalidTopic without contacting the SDK or
// emitting metrics. Per-message dynamic addressing for Service Bus is
// explicitly out of scope. The logical Envelope.Subject is mapped to
// the native Service Bus Subject by envelopeToMessage and never
// selects the entity.
func (s *Sender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	if env == nil {
		return shared.ErrInvalidPayload.WithMessage("servicebus: nil envelope")
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	entity := s.entityName()
	if msg.Address != "" && msg.Address != entity {
		return shared.ErrInvalidTopic.WithMessage(fmt.Sprintf(
			"servicebus: address %q does not match configured entity %q",
			msg.Address, entity))
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "servicebus: sending",
			"entity", s.entityName(),
			"envelope_id", env.ID(),
			"payload_len", len(env.Payload()),
		)
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	start := s.clock().Now()
	if err := sendOne(sendCtx, s.currentClient(), env, s.cfg.DefaultSessionID, s.clock()); err != nil {
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "servicebus: send failed",
				"entity", s.entityName(), "error", err)
		}
		return err
	}

	s.metrics.Timer(MetricASBSendLatency, s.clock().Since(start),
		shared.Tag{Key: shared.TagKeyEntity, Value: s.entityName()})

	return nil
}

// SendBatch sends each envelope in chunks of up to cfg.BatchSize. ASB
// batches are size-limited; when a message overflows the batch, the
// current batch is flushed and the oversized message is sent
// individually.
//
// The whole slice is pre-validated before any SDK dispatch: a nil
// envelope yields shared.ErrInvalidPayload and a non-empty address that
// does not match the configured entity yields shared.ErrInvalidTopic;
// either rejects the entire batch with (nil, joined-errs) — fail-fast,
// no chunk is dispatched. A client setup failure likewise returns
// (nil, err).
//
// Once dispatched, SendBatch returns (results, nil): one BatchResult per
// input message, index-aligned with msgs. Service Bus does not report
// per-message results inside a batch, so each result carries nil Err
// when its flush/send succeeded or the classified error otherwise.
// Chunks continue independently after a chunk-level failure. See
// ports.BatchSender for the contract.
func (s *Sender) SendBatch(ctx context.Context, msgs []ports.OutboundMessage) ([]ports.BatchResult, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}
	entity := s.entityName()

	var preErrs []error
	for i, m := range msgs {
		if m.Envelope == nil {
			preErrs = append(preErrs, shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
				"servicebus: nil envelope at index %d", i)))
			continue
		}
		if m.Address != "" && m.Address != entity {
			preErrs = append(preErrs, shared.ErrInvalidTopic.WithMessage(fmt.Sprintf(
				"servicebus: address %q at index %d does not match configured entity %q",
				m.Address, i, entity)))
		}
	}
	if len(preErrs) > 0 {
		return nil, errors.Join(preErrs...)
	}

	envs := make([]*messaging.Envelope, len(msgs))
	for i, m := range msgs {
		envs[i] = m.Envelope
	}

	// Snapshot once for the whole batch: a mid-batch credential rotation
	// must not split the batch across two links.
	client := s.currentClient()

	results := make([]ports.BatchResult, len(envs))

	for i := 0; i < len(envs); i += s.cfg.BatchSize {
		end := i + s.cfg.BatchSize
		if end > len(envs) {
			end = len(envs)
		}
		chunk := envs[i:end]

		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "servicebus: sending batch",
				"entity", entity,
				"chunk_size", len(chunk),
			)
		}

		// cfg.Timeout is a PER-CALL bound (see SenderConfig.Timeout): each
		// chunk gets a fresh deadline. A single deadline across all chunks
		// would starve the tail of a large batch.
		chunkCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)

		start := s.clock().Now()
		for _, cr := range sendChunk(chunkCtx, client, chunk, s.cfg.DefaultSessionID, s.clock(), s.logger, entity) {
			results[i+cr.Index] = ports.BatchResult{Index: i + cr.Index, Err: cr.Err}
		}
		cancel()
		s.metrics.Timer(MetricASBSendBatchLatency, s.clock().Since(start),
			shared.Tag{Key: shared.TagKeyEntity, Value: entity})
	}

	return results, nil
}

// Close tears down the Service Bus sender and the underlying AMQP connection.
func (s *Sender) Close(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "servicebus: sender closing",
			"entity", s.entityName())
	}

	oldClient, oldHandle := s.swapClient(nil, nil, s.connectionSnapshot())

	var firstErr error
	if oldClient != nil {
		firstErr = oldClient.Close(ctx)
	}
	if oldHandle != nil {
		if err := oldHandle.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// connectionSnapshot reads cfg.Connection under the swap lock.
func (s *Sender) connectionSnapshot() ConnectionConfig {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	return s.cfg.Connection
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
