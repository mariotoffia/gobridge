package sqs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time checks.
var (
	_ ports.Sender                  = (*Sender)(nil)
	_ ports.BatchSender             = (*Sender)(nil)
	_ ports.AddressValidatingSender = (*Sender)(nil)
)

// Sender implements ports.Sender and ports.BatchSender for Amazon SQS.
//
// Concurrency: the SQS client is held in an atomic.Pointer so the hot
// send path reads it lock-free while ApplyCredentials swaps a rotated
// client underneath in-flight calls. initMu serialises the lazy-init /
// queue-URL resolution sequence and credential swaps against each other;
// it never guards a hot-path read.
type Sender struct {
	cfg      SenderConfig
	client   atomic.Pointer[sqsAPI]
	queueURL string
	initMu   sync.Mutex
	logger   *slog.Logger
	metrics  ports.MetricsExporter
	clk      clock.Clock
}

// loadClient returns the current SQS client snapshot, or nil when unset.
func (s *Sender) loadClient() sqsAPI {
	if p := s.client.Load(); p != nil {
		return *p
	}
	return nil
}

// storeClient atomically installs the SQS client snapshot. A nil client
// clears it (lazy-init reset / tests).
func (s *Sender) storeClient(c sqsAPI) {
	if c == nil {
		s.client.Store(nil)
		return
	}
	s.client.Store(&c)
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

// addressMatchesQueue reports whether a rendered OutboundMessage.Address
// refers to this sender's bound queue. SQS senders are pinned to one
// queue, but a route binding address is frequently the logical queue
// NAME — scenario configs use `address: <queue-name>` — while the sender
// resolves a fully-qualified queue URL. Accept any unambiguous reference
// to the bound queue: empty (use the configured queue), the resolved
// queue URL, the configured QueueName, or the queue name embedded as the
// last path segment of the queue URL. Everything else is a mismatch.
func (s *Sender) addressMatchesQueue(addr string) bool {
	if addr == "" || addr == s.queueURL {
		return true
	}
	if s.cfg.QueueName != "" && addr == s.cfg.QueueName {
		return true
	}
	if name := queueNameFromURL(s.queueURL); name != "" && addr == name {
		return true
	}
	return false
}

// queueNameFromURL returns the trailing path segment of an SQS queue URL
// (the queue name, e.g. "orders.fifo" from
// "https://sqs.us-east-1.amazonaws.com/123456789012/orders.fifo"), or ""
// when the input has no path segment.
func queueNameFromURL(u string) string {
	if i := strings.LastIndexByte(u, '/'); i >= 0 && i+1 < len(u) {
		return u[i+1:]
	}
	return ""
}

// ValidateAddress reports whether a static binding address refers to this
// sender's bound queue, so a misconfigured address fails when the bridge is
// built rather than at first Send. It implements ports.AddressValidatingSender
// and mirrors the send-time check (addressMatchesQueue), but is deliberately
// more lenient about queue URLs: a QueueName-only sender resolves its URL
// lazily on the first Send, so s.queueURL is empty at build time. To avoid
// falsely rejecting a full-URL binding address before that resolution happens,
// also accept an address whose trailing path segment equals the configured
// QueueName. A definitively wrong address — neither the URL, the name, nor a
// URL ending in the name — is rejected with shared.ErrInvalidTopic.
//
// This check is offline and structural: it never calls GetQueueUrl, so for a
// name-only sender a structurally-plausible full URL (right name, wrong
// region/account) passes the build and is still caught at first Send once the
// canonical URL is resolved. That is the safe direction — build-time accepts a
// superset of send-time, so a valid config never fails the build.
func (s *Sender) ValidateAddress(address string) error {
	if address == "" || s.addressMatchesQueue(address) {
		return nil
	}
	if s.cfg.QueueName != "" && queueNameFromURL(address) == s.cfg.QueueName {
		return nil
	}
	return shared.ErrInvalidTopic.WithMessage(fmt.Sprintf(
		"sqs: binding address %q does not match configured queue (url=%q name=%q)",
		address, s.queueURL, s.cfg.QueueName))
}

// Send submits a single envelope to SQS.
//
// Address validation: an SQS Sender is bound to a single queue (resolved
// lazily via ensureClient from cfg.QueueURL or cfg.QueueName). When
// msg.Address is empty, the configured queue is used. A non-empty
// msg.Address is accepted when it refers to the bound queue — see
// addressMatchesQueue: the resolved queue URL, the configured queue
// name, or the queue name embedded as the last path segment of the URL
// (the form scenario configs use, e.g. `address: orders`). Anything else
// is rejected with shared.ErrInvalidTopic without contacting the SDK or
// emitting metrics. Per-message dynamic addressing for SQS is explicitly
// out of scope (see ARCHITECTURE_PLAN "Non-Goals"). The logical
// Envelope.Subject is mapped to the "Subject" SQS message attribute by
// buildSendInput and never selects the queue.
func (s *Sender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	if env == nil {
		return shared.ErrInvalidPayload.WithMessage("sqs: nil envelope")
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	if !s.addressMatchesQueue(msg.Address) {
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
		if m.Address != "" && !s.addressMatchesQueue(m.Address) {
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
