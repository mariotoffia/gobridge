package tenant

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// DefaultTenantHeader is the app-visible header the tenant processor reads
// the tenant identifier from when Config.TenantHeader is unset. It is
// deliberately NOT the reserved x-bridge.tenant-id header: the runtime
// strips all reserved x-bridge.* headers at ingress (anti-spoofing), so a
// reserved source header would always be empty by the time the chain runs.
const DefaultTenantHeader = "x-tenant-id"

// Tenant observability metric names. Kept package-local — deliberately not
// added to the shared registry (domain/shared/metrics.go, which this module
// already imports) — because they are emitted only by this optional tenant
// processor. The shared registry holds the runtime's cross-cutting metric
// vocabulary; a single plugin's internal metric names do not belong there.
const (
	metricTenantTrackerErrors = "TenantTrackerErrors"
	metricTenantRejects       = "TenantRejects"
)

// ErrTenantHeaderReserved signals that the configured TenantHeader is a
// reserved x-bridge.* header. Such a header is stripped at ingress, so
// reading it would silently resolve no tenant. Rejected at construction
// as a permanent setup error so the misconfiguration fails fast.
var ErrTenantHeaderReserved = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "tenant: TenantHeader must not be a reserved x-bridge.* header",
}

// Processor resolves and validates tenant identity and tracks per-tenant
// usage for messages flowing through the processing chain. Enforcement
// covers tenant-active, MaxMessageSizeBytes, and — when the configured
// usage tracker also implements ports.TenantUsageReader and the tenant's
// MaxInFlight > 0 — a per-tenant in-flight ceiling (transient reject,
// fail-open on read error). Message-count usage tracking remains
// observational (increment-only — no message-count quota ceiling).
type Processor struct {
	config    Config
	validator ports.TenantValidator
	tracker   ports.TenantUsageTracker
	reader    ports.TenantUsageReader
	metrics   ports.MetricsExporter
	logger    *slog.Logger
}

var _ ports.Processor = (*Processor)(nil)

// New creates a tenant processor with the given configuration and options.
// The tenant source header defaults to DefaultTenantHeader (non-reserved)
// and a reserved x-bridge.* TenantHeader is rejected, so tenant resolution
// cannot silently fail against an ingress-stripped header.
func New(cfg Config, opts ...Option) (*Processor, error) {
	if cfg.TenantHeader == "" {
		cfg.TenantHeader = DefaultTenantHeader
	}
	if messaging.IsReservedHeader(cfg.TenantHeader) {
		return nil, ErrTenantHeaderReserved.With("header", cfg.TenantHeader)
	}
	if cfg.InFlightDecrementTimeout <= 0 {
		cfg.InFlightDecrementTimeout = 2 * time.Second
	}

	p := &Processor{config: cfg, metrics: &ports.NoopExporter{}}
	for _, opt := range opts {
		opt(p)
	}
	if p.metrics == nil {
		p.metrics = &ports.NoopExporter{}
	}
	// Type-assert the optional read-back capability once at construction,
	// not per message. A tracker that also implements TenantUsageReader
	// unlocks in-flight quota enforcement; increment-only trackers leave
	// p.reader nil and enforcement is skipped.
	if p.tracker != nil {
		p.reader, _ = p.tracker.(ports.TenantUsageReader)
	}
	return p, nil
}

func (p *Processor) Name() string {
	if p.config.Name != "" {
		return p.config.Name
	}
	return "tenant"
}

// resolveTenantID reads the tenant identity from headers[key], distinguishing
// an absent header from one that is present but not a usable string:
//
//   - key absent            -> ("", nil): no tenant; caller applies the
//     fail-open / RequireTenant policy.
//   - present, string       -> (value, nil); an empty string falls through to
//     the same "no tenant" path as absent.
//   - present, int/int64/uint32 -> (decimal string, nil): a tenant stamped as a
//     typed numeric property by a transport (AMQP/MQTTv5) is coerced, not lost.
//   - present, float64 that is integral and exactly representable ->
//     (decimal string, nil): encoding/json rehydrates EVERY numeric header as
//     float64, so a numeric tenant survives a DLQ/outbox JSON round-trip and
//     coerces to the SAME decimal string int64 would produce.
//   - present, any other type (incl. a fractional, non-finite, or >2^53 float64)
//     -> ("", ErrInvalidPayload): a MALFORMED identity, rejected as ambiguous
//     rather than silently coerced, so a present-but-non-string tenant is never
//     mistaken for "no tenant" (which would fail-open the message untenanted).
//
// ponytail / TRUST BOUNDARY: the tenant header is CALLER-SUPPLIED and is NOT
// authenticated here — this resolver only closes the present-but-non-string type
// trap. Any downstream consumer of tenant identity MUST treat it as untrusted
// unless a preceding validator authenticates it (e.g. against a signed claim).
//
// maxExactTenantInt is the largest integer float64 represents exactly (2^53); a
// numeric tenant beyond it is ambiguous after a JSON round-trip, so it is
// rejected rather than coerced to a possibly-wrong value.
const maxExactTenantInt float64 = 1 << 53

func resolveTenantID(headers map[string]any, key string) (string, error) {
	if headers == nil {
		return "", nil
	}
	v, ok := headers[key]
	if !ok {
		return "", nil // absent: no tenant
	}
	switch n := v.(type) {
	case string:
		return n, nil // empty string -> treated as absent by the caller
	case int:
		return strconv.FormatInt(int64(n), 10), nil
	case int64:
		return strconv.FormatInt(n, 10), nil
	case uint32:
		return strconv.FormatUint(uint64(n), 10), nil
	case float64:
		// encoding/json rehydrates EVERY numeric header as float64 (see
		// messaging.Envelope.UnmarshalJSON): a tenant stamped int64(42) on first
		// delivery is float64(42) after any DLQ/outbox JSON round-trip. Coerce an
		// integral, exactly-representable value to the SAME decimal string int64
		// would produce so a numeric tenant survives redrive; reject a fractional,
		// non-finite, or out-of-safe-range value as malformed (never fail-open
		// untenanted).
		// ponytail: 2^53 ceiling — a larger numeric tenant id cannot be
		// distinguished from its neighbours over JSON; use a string id for those.
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) ||
			n < -maxExactTenantInt || n > maxExactTenantInt {
			return "", shared.ErrInvalidPayload.WithMessage(
				fmt.Sprintf("tenant header %q has a non-integral or out-of-range numeric value (%v)", key, n))
		}
		return strconv.FormatInt(int64(n), 10), nil
	default:
		return "", shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("tenant header %q present but not a string (got %T)", key, v))
	}
}

func (p *Processor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	// Resolve the tenant identity, distinguishing an ABSENT header (no tenant,
	// fail-open/RequireTenant applies) from a header that is PRESENT but not a
	// usable string (a MALFORMED identity — reject, never fail-open untenanted).
	tenantID, err := resolveTenantID(env.Headers(), p.config.TenantHeader)
	if err != nil {
		p.observeReject(ctx, "malformed", "")
		return err
	}

	if tenantID == "" {
		if p.config.RequireTenant {
			p.observeReject(ctx, "missing_required", tenantID)
			return shared.ErrInvalidPayload.WithMessage("tenant ID required")
		}
		return next(ctx, env)
	}

	var (
		info      ports.TenantInfo
		validated bool
	)

	if p.validator != nil {
		var err error
		info, err = p.validator.Validate(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("tenant validation failed for %q: %w", tenantID, err)
		}
		validated = true

		if !info.Active {
			p.observeReject(ctx, "disabled", tenantID)
			return shared.ErrInvalidPayload.WithMessage(
				fmt.Sprintf("tenant disabled: %s", tenantID),
			)
		}

		if info.MaxMessageSizeBytes > 0 && int64(len(env.Payload())) > info.MaxMessageSizeBytes {
			p.observeReject(ctx, "oversize", tenantID)
			return shared.ErrInvalidPayload.WithMessage(
				fmt.Sprintf("message size %d exceeds tenant limit %d",
					len(env.Payload()), info.MaxMessageSizeBytes),
			)
		}
	}

	if p.tracker != nil {
		// Enforce the per-tenant in-flight ceiling before incrementing.
		// MaxInFlight lives on TenantInfo, so enforcement is gated on a
		// validator having produced info (validated); the read-back requires
		// the tracker to also implement ports.TenantUsageReader.
		//
		// ponytail: check-then-increment races under concurrency; overshoot is
		// bounded by the tenant's total concurrent in-flight admissions across
		// all routes and instances sharing the usage store, not a single
		// route's parallelism. Upgrade path if exact ceilings are ever
		// required: optional conditional-increment extension
		// (IncrementInFlightIfBelow) on the tracker.
		if p.reader != nil && validated && info.MaxInFlight > 0 {
			usage, err := p.reader.Usage(ctx, tenantID)
			if err != nil {
				// Fail open: quota is a fairness control, not a security
				// boundary. Failing closed would turn a usage-store blip into
				// a full-tenant outage. Surface for observability, then proceed.
				p.observeTrackerError(ctx, "usage_read", tenantID, err)
			} else if usage.InFlight >= info.MaxInFlight {
				p.observeThrottle(ctx, "quota_inflight", tenantID)
				return shared.ErrTenantQuotaExceeded.WithMessage(
					fmt.Sprintf("tenant %s in-flight quota exceeded: %d >= %d",
						tenantID, usage.InFlight, info.MaxInFlight))
			}
		}

		if err := p.tracker.IncrementInFlight(ctx, tenantID, 1); err != nil {
			p.observeTrackerError(ctx, "increment", tenantID, err)
			// Transient dependency failure: classify so the runtime
			// retries rather than DLQ-ing on a tracker hiccup.
			return shared.ErrUnavailable.Wrap(
				fmt.Errorf("tenant in-flight tracking failed: %w", err))
		}
		release := func() {
			decrementCtx := ctx
			if ctx.Err() != nil {
				var cancel context.CancelFunc
				decrementCtx, cancel = context.WithTimeout(context.Background(), p.config.InFlightDecrementTimeout)
				defer cancel()
			}
			if err := p.tracker.IncrementInFlight(decrementCtx, tenantID, -1); err != nil {
				// Best-effort cleanup: cannot alter control flow, but the
				// leaked in-flight count must be visible.
				p.observeTrackerError(decrementCtx, "decrement", tenantID, err)
			}
		}
		if scope, ok := ports.DeliveryScopeFrom(ctx); ok {
			// Decrement when the WHOLE delivery settles (after the send), not
			// when Process returns mid-chain. This is the fix: the tenant is
			// the last processor, so the send happens in the RouteRunner AFTER
			// the chain returns — a defer here would fire before the send even
			// starts, making MaxInFlight a no-op. In route-runner use ctx is
			// already cancelled by the time Release runs, so the decrement
			// deterministically takes the Background+timeout branch above —
			// intentional: the cleanup must not be cancelled by the delivery it
			// is cleaning up after.
			scope.OnRelease(release)
		} else {
			defer release() // standalone use (no runtime scope): today's behaviour
		}
	}

	err = next(ctx, env)

	if p.tracker != nil && err == nil {
		if mErr := p.tracker.IncrementMessages(ctx, tenantID, 1); mErr != nil {
			// Message-count is advisory: swallowed for control flow
			// (Process still succeeds) but surfaced for observability.
			p.observeTrackerError(ctx, "message_count", tenantID, mErr)
		}
	}

	return err
}

// observeTrackerError emits a metric and a structured warning when a
// per-tenant usage-tracker call fails. op is a low-cardinality dimension
// (increment / decrement / message_count / usage_read); the tenant ID is
// kept out of the metric tags (unbounded cardinality) and logged instead.
func (p *Processor) observeTrackerError(ctx context.Context, op, tenantID string, err error) {
	p.metrics.Counter(metricTenantTrackerErrors, 1, shared.Tag{Key: "op", Value: op})
	if p.logger != nil {
		p.logger.WarnContext(ctx, "tenant usage tracker error",
			"processor", p.Name(), "op", op, "tenant", tenantID, "error", err)
	}
}

// observeReject emits a metric and a structured warning when the
// processor rejects or skips a message for a tenancy-policy reason. The
// reason is low-cardinality; the tenant ID is logged, not tagged.
func (p *Processor) observeReject(ctx context.Context, reason, tenantID string) {
	p.metrics.Counter(metricTenantRejects, 1, shared.Tag{Key: "reason", Value: reason})
	if p.logger != nil {
		p.logger.WarnContext(ctx, "tenant message rejected",
			"processor", p.Name(), "reason", reason, "tenant", tenantID)
	}
}

// observeThrottle emits the same reject counter as observeReject but logs at
// Debug rather than Warn. It is used for the quota-ceiling path, which — unlike
// disabled/oversize/missing_required (rare anomalies) — is an EXPECTED
// steady-state outcome under a runaway tenant and is amplified by redelivery
// (each retry re-rejects). Warn-per-event there is a log-ingestion DoS caused
// by exactly the condition the quota targets; the metricTenantRejects counter
// (tagged reason) remains the signal.
func (p *Processor) observeThrottle(ctx context.Context, reason, tenantID string) {
	p.metrics.Counter(metricTenantRejects, 1, shared.Tag{Key: "reason", Value: reason})
	if p.logger != nil {
		p.logger.DebugContext(ctx, "tenant message throttled",
			"processor", p.Name(), "reason", reason, "tenant", tenantID)
	}
}
