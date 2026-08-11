package servicebus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

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
// SendBatch may be in flight. Send / SendBatch resolve the live seam
// via ensureAndSnapshotClient — a single locked ensure+snapshot, so a
// concurrent closed-link teardown (invalidateClient) can never hand a
// caller a nil seam between "ensure" and "use". An in-flight send
// finishes against the seam it captured.
type Sender struct {
	cfg       SenderConfig
	client    asbSenderAPI
	asbClient *asbClientHandle
	initMu    sync.Mutex
	logger    *slog.Logger
	metrics   ports.MetricsExporter
	clk       clock.Clock

	// authFailureCB is the reactive-recovery hook. The
	// CredentialRefresher injects a URI-bound callback via
	// SetAuthFailureCallback; reportAuthFailure invokes it when a live Send maps
	// an SDK error to shared.ErrNotAuthorized (SAS/AAD revocation), forcing an
	// immediate re-resolve rather than failing sends on revoked material until
	// the next poll. atomic.Pointer gives safe publication across the builder
	// goroutine (setter) and the send goroutines (load).
	authFailureCB atomic.Pointer[func(error)]

	// buildSenderFn overrides the real sender build (buildSender) for
	// tests that need to drive a closed-link rebuild deterministically
	// without dialing Azure; nil in production. It returns a fresh sender
	// seam and its owning client handle, mirroring ensureAndSnapshotClient's build.
	buildSenderFn func(ctx context.Context, conn ConnectionConfig) (asbSenderAPI, *asbClientHandle, error)
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
	used, err := s.ensureAndSnapshotClient(ctx)
	if err != nil {
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
	if err = sendOne(sendCtx, used, env, s.cfg.DefaultSessionID, s.clock()); err != nil {
		// A terminally CLOSED sender link never recovers on its own; tear
		// it down so the NEXT Send rebuilds a fresh link (fenced against a
		// concurrent credential rotation). See invalidateOnClosedLink.
		s.invalidateOnClosedLink(ctx, used, err)
		// reactive-recovery chokepoint: sendOne already classified the
		// SDK error via MapError. When a SAS/AAD revocation makes it
		// shared.ErrNotAuthorized, force an immediate re-resolve instead of
		// failing every send on revoked material until the next poll.
		// reportAuthFailure filters non-auth send errors.
		s.reportAuthFailure(err)
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
	// SendBatch also participates in reactive recovery. sendChunk
	// aggregates a MapError-classified error per message into results[i].Err
	// rather than returning one error, so the report is fired from a scan of
	// the aggregated results below (see the ErrNotAuthorized scan after the
	// chunk loop). This matters for a batch-ONLY sender: with no single-Send
	// or receive-poll path, the batch results are the sole live-op signal of a
	// credential revocation.
	client, err := s.ensureAndSnapshotClient(ctx)
	if err != nil {
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

	// client was snapshotted atomically by ensureAndSnapshotClient above:
	// a mid-batch credential rotation must not split the batch across two
	// links, and the seam is guaranteed non-nil (no nil-deref window).
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

	// If the shared batch link went terminally CLOSED, tear it down so the
	// NEXT Send/SendBatch rebuilds a fresh one instead of reusing the dead
	// link forever (fenced against a concurrent rotation). See.
	for i := range results {
		if isClosedLinkError(results[i].Err) {
			s.invalidateOnClosedLink(ctx, client, results[i].Err)
			break
		}
	}

	// a batch-only sender has no single-Send/receive path to fire the
	// reactive-recovery report, so surface a revocation from the aggregated
	// per-message results here. Report once — NotifyAuthFailure is per-URI
	// rate-limited, so one call per batch is sufficient and cheapest. A SEPARATE
	// scan (not the closed-link loop above, which breaks on the first
	// closed-link result and could skip a later auth error) guarantees any
	// ErrNotAuthorized in the batch is seen.
	for i := range results {
		if errors.Is(results[i].Err, shared.ErrNotAuthorized) {
			s.reportAuthFailure(results[i].Err)
			break
		}
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

// ensureAndSnapshotClient lazily builds the sender link on first use (or
// after a closed-link teardown niled it) and returns the LIVE seam
// atomically under a single lock hold. Folding "ensure" and "snapshot"
// into one locked op closes the nil window that two separate locked ops
// (a former ensureClient followed by currentClient) left open: a
// concurrent invalidateClient could nil s.client BETWEEN them, handing a
// caller a nil seam that sendOne / sendChunk would dereference. The
// returned seam is guaranteed non-nil whenever err is nil.
//
//nolint:ireturn // asbSenderAPI is an adapter-internal mock seam (category 5).
func (s *Sender) ensureAndSnapshotClient(ctx context.Context) (asbSenderAPI, error) {
	s.initMu.Lock()
	defer s.initMu.Unlock()

	if s.client != nil {
		return s.client, nil
	}

	sender, asbClient, err := s.buildSender(ctx, s.cfg.Connection)
	if err != nil {
		return nil, err
	}

	s.client = sender
	s.asbClient = asbClient

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "servicebus: sender initialized",
			"entity", s.entityName())
	}

	return s.client, nil
}

// buildSender constructs a fresh sender seam and its owning client
// handle from conn. Shared by ensureAndSnapshotClient (cold init on first
// Send and after a closed-link teardown). The buildSenderFn seam
// overrides it in tests; production dials the SDK via buildClient +
// NewSender.
//
//nolint:ireturn // asbSenderAPI is an adapter-internal mock seam (category 5).
func (s *Sender) buildSender(ctx context.Context, conn ConnectionConfig) (asbSenderAPI, *asbClientHandle, error) {
	if s.buildSenderFn != nil {
		return s.buildSenderFn(ctx, conn)
	}

	asbClient, err := buildClient(conn)
	if err != nil {
		return nil, nil, err
	}

	entityName := s.entityName()
	sender, err := asbClient.NewSender(entityName)
	if err != nil {
		_ = asbClient.Close(ctx)
		return nil, nil, MapError(err)
	}
	return sender, asbClient, nil
}

// invalidateOnClosedLink tears down a terminally CLOSED sender link so
// the NEXT Send rebuilds a fresh one (ensureAndSnapshotClient). A closed
// AMQP sender never recovers on its own; without this every later send
// reuses the dead link and fails until the process restarts or a
// credential rotation happens to swap it.
//
// Only the SDK's TYPED CodeClosed condition triggers it
// (isClosedLinkError): CodeConnectionLost self-heals inside the SDK on
// the next send, and a misclassified auth/config fault must SURFACE, not
// silently rebuild-loop. An injected test client (cfg.Client != nil) has
// no connection to rebuild from and is left in place.
//
// The teardown is FENCED by pointer identity (invalidateClient): it
// clears the seam ONLY when s.client is STILL `used`. A concurrent
// ApplyCredentials rotation installs a fresh seam (a new object);
// observing s.client != used means the rotation already replaced the
// dead link, so tearing it down would destroy the healthy fresh link.
func (s *Sender) invalidateOnClosedLink(ctx context.Context, used asbSenderAPI, err error) {
	if s.cfg.Client != nil || used == nil || !isClosedLinkError(err) {
		return
	}
	old, oldHandle := s.invalidateClient(used)
	if old == nil && oldHandle == nil {
		// A rotation (or a racing Send) already swapped the dead link out.
		return
	}
	// Close the dead pair OUTSIDE the swap lock; the link is already
	// closed so any error here is best-effort.
	if old != nil {
		_ = old.Close(ctx)
	}
	if oldHandle != nil {
		_ = oldHandle.Close(ctx)
	}
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug,
			"servicebus: sender link closed; rebuilding on next send",
			"entity", s.entityName())
	}
}

// invalidateClient clears the live sender seam + client handle IFF the
// seam is still `used` (an identity fence against a concurrent rotation),
// returning the cleared pair for the caller to Close OUTSIDE the lock.
// Returns (nil, nil) when a rotation — or another racing invalidation —
// already replaced or cleared `used`, so a fresh link is never torn down
// and a dead link is never double-closed.
//
//nolint:ireturn // asbSenderAPI is an adapter-internal mock seam (category 5).
func (s *Sender) invalidateClient(used asbSenderAPI) (asbSenderAPI, *asbClientHandle) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.client == nil || s.client != used {
		return nil, nil
	}
	old, oldHandle := s.client, s.asbClient
	s.client = nil
	s.asbClient = nil
	return old, oldHandle
}
