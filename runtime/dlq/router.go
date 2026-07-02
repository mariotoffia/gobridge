package dlq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Router classifies errors and writes entries to a [ports.DLQStore].
// [Router.Route] writes synchronously and returns only once the entry is
// durably confirmed (or permanently failed after bounded retries). Callers
// MUST treat a non-nil return as "DLQ evidence is not durable" and refuse to
// settle (ACK/Complete) the source delivery or outbox record. This keeps the
// failure evidence at least as durable as the message it describes.
type Router struct {
	store             ports.DLQStore
	writeTimeout      time.Duration
	logger            *slog.Logger
	metrics           ports.MetricsExporter
	clk               clock.Clock
	mu                sync.Mutex // guards tokenFn
	tokenFn           func() (persistence.LeaseToken, bool)
	writeMaxAttempts  int
	writeRetryBackoff routing.BackoffPolicy
}

// Config configures the DLQ router.
type Config struct {
	Store        ports.DLQStore
	WriteTimeout time.Duration // per-write deadline, default 30s
	Logger       *slog.Logger
	Metrics      ports.MetricsExporter
	Clock        clock.Clock

	WriteMaxAttempts  int
	WriteRetryBackoff routing.BackoffPolicy
}

const (
	defaultWriteTimeout = 30 * time.Second
)

// New creates a DLQ router. If store is nil, [Router.Route] is a no-op.
func New(store ports.DLQStore) *Router {
	return NewFromConfig(Config{Store: store})
}

// NewFromConfig creates a DLQ router with explicit configuration.
func NewFromConfig(cfg Config) *Router {
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System
	}
	if cfg.WriteMaxAttempts <= 0 {
		cfg.WriteMaxAttempts = 3
	}
	if cfg.WriteRetryBackoff.InitialInterval <= 0 {
		cfg.WriteRetryBackoff.InitialInterval = 500 * time.Millisecond
	}
	if cfg.WriteRetryBackoff.MaxInterval <= 0 {
		cfg.WriteRetryBackoff.MaxInterval = 5 * time.Second
	}
	if cfg.WriteRetryBackoff.Multiplier <= 0 {
		cfg.WriteRetryBackoff.Multiplier = 2.0
	}
	return &Router{
		store:             cfg.Store,
		writeTimeout:      cfg.WriteTimeout,
		logger:            cfg.Logger,
		metrics:           m,
		clk:               clk,
		writeMaxAttempts:  cfg.WriteMaxAttempts,
		writeRetryBackoff: cfg.WriteRetryBackoff,
	}
}

// HasStore returns true if a DLQ store is configured.
func (r *Router) HasStore() bool {
	return r.store != nil
}

// SetTokenFn sets the function used to check lease validity before DLQ writes.
func (r *Router) SetTokenFn(fn func() (persistence.LeaseToken, bool)) {
	r.mu.Lock()
	r.tokenFn = fn
	r.mu.Unlock()
}

// Route classifies err and writes a DLQ entry for env synchronously,
// returning only once the write is durably confirmed (nil) or has
// permanently failed after bounded retries (non-nil). A non-nil return
// means the evidence is NOT durable; the caller must not settle the source
// delivery or outbox record so the message is redelivered or reclaimed.
//
// The address parameter is the transport destination address that was
// the target of the failed delivery (e.g. MQTT topic, SQS queue URL,
// AMQP routing key) on egress, or the source address on ingress. It
// is recorded on the DLQ entry so consumers can route or analyze
// failures by transport address without inspecting Envelope.Subject.
func (r *Router) Route(
	ctx context.Context,
	env *messaging.Envelope,
	routeID, bindingID, address, sessionID, sourceID string,
	err error,
	attempts int,
) error {
	if r.store == nil {
		return nil
	}

	entry := r.buildEntry(env, routeID, bindingID, address, sessionID, sourceID, err, attempts)
	return r.writeConfirmed(ctx, entry)
}

func (r *Router) buildEntry(
	env *messaging.Envelope,
	routeID, bindingID, address, sessionID, sourceID string,
	err error,
	attempts int,
) routing.DLQEntry {
	category, errorCode := classifyError(err)
	correlationID, _ := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationID)
	reason := safeErrorReason(err)

	return routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:            generateID(),
		Envelope:      *env,
		RouteID:       routeID,
		BindingID:     bindingID,
		Address:       address,
		SessionID:     sessionID,
		SourceID:      sourceID,
		CorrelationID: correlationID,
		Reason:        reason,
		Category:      category,
		ErrorCode:     errorCode,
		LastError:     reason,
		FailedAt:      r.clk.Now(),
		Attempts:      attempts,
	})
}

// writeConfirmed writes entry to the store and returns only once the write
// is durably confirmed (nil) or has permanently failed after
// writeMaxAttempts (non-nil). Backoff between attempts is driven by the
// injected clock. The configured lease check is a best-effort gate: it
// refuses the write with a transient error when the lease is absent at
// entry, so a cleanly-demoted instance leaves the message for the rightful
// owner instead of settling it. It is NOT store-level fencing — the token is
// not passed to Write, so a lease lost mid-write can still produce a
// duplicate entry (never loss, since duplicates are reconcilable downstream).
func (r *Router) writeConfirmed(ctx context.Context, entry routing.DLQEntry) error {
	r.mu.Lock()
	tokenFn := r.tokenFn
	r.mu.Unlock()

	if tokenFn != nil {
		if _, hasLease := tokenFn(); !hasLease {
			if r.logger != nil {
				r.logger.Warn("DLQ write skipped, lease not held",
					"entry_id", entry.ID(),
					"route_id", entry.RouteID(),
				)
			}
			r.metrics.Counter(shared.MetricDLQWriteFailures, 1)
			return shared.ErrUnavailable.WithMessage("DLQ write skipped: lease not held")
		}
	}

	delay := r.writeRetryBackoff.InitialInterval
	var writeErr error
	for attempt := 0; attempt < r.writeMaxAttempts; attempt++ {
		// Honor cancellation before each attempt, including the first, so a
		// shutdown does not pay one WriteTimeout against a store that ignores
		// context before returning and unblocking Run's wg.Wait.
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-r.clk.After(delay):
			}
			delay = time.Duration(float64(delay) * r.writeRetryBackoff.Multiplier)
			if delay > r.writeRetryBackoff.MaxInterval {
				delay = r.writeRetryBackoff.MaxInterval
			}
		}
		writeCtx, cancel := context.WithTimeout(ctx, r.writeTimeout)
		writeErr = r.store.Write(writeCtx, entry)
		cancel()
		if writeErr == nil {
			return nil
		}
	}

	r.metrics.Counter(shared.MetricDLQWriteFailures, 1)
	if r.logger != nil {
		r.logger.Error("DLQ write failed after retries",
			"entry_id", entry.ID(),
			"route_id", entry.RouteID(),
			"error", writeErr,
			"attempts", r.writeMaxAttempts,
		)
	}
	return fmt.Errorf("runtime: dlq: write failed after %d attempts: %w", r.writeMaxAttempts, writeErr)
}

// safeErrorReason returns a sanitized error reason suitable for persistence.
func safeErrorReason(err error) string {
	be, ok := shared.AsBridgeError(err)
	if ok {
		return be.Message
	}
	return "internal error"
}

func classifyError(err error) (category string, code string) {
	be, ok := shared.AsBridgeError(err)
	if !ok {
		return "unknown", ""
	}
	return string(be.Class), string(be.Code)
}

// idFallbackCounter feeds the deterministic-but-unique fallback path in
// [generateID] when crypto/rand fails after composition (effectively
// impossible on supported OSes but kept as defence-in-depth so the DLQ
// router never panics on ID generation).
//
//nolint:gochecknoglobals // counter must outlive every call
var idFallbackCounter atomic.Uint64

// generateID returns a 32-hex-character DLQ entry identifier. It mirrors
// the runtime-wide convention used by other components but is duplicated
// here because the dlq package is a leaf and must not depend on its
// parent (which would create an import cycle once the parent imports
// this package).
func generateID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fallbackID()
	}
	return hex.EncodeToString(b)
}

func fallbackID() string {
	n := idFallbackCounter.Add(1)
	ts := uint64(clock.System.Now().UnixNano())
	return strconv.FormatUint(ts, 16) + "-" + strconv.FormatUint(n, 16)
}
