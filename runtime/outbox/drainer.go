package outbox

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// Drainer claims pending outbox records for a partition and sends them
// through the target sender. It validates fencing tokens to prevent
// stale owners from sending after a lease transfer.
type Drainer struct {
	outboxStore    ports.OutboxStore
	leaseStore     ports.LeaseStore
	sender         ports.Sender
	dlq            *dlq.Router
	routeID        string
	partitionKey   string
	leaseID        string
	ownerID        string
	policy         routing.RoutePolicy
	strategy       persistence.DrainStrategy
	batchSize      int
	maxBatchSize   int
	maxConcurrency int
	metrics        ports.MetricsExporter
	hook           ports.DeliveryHook
	logger         *slog.Logger
	clk            clock.Clock

	drainTimeout          time.Duration
	perRecordDrainTimeout time.Duration
	maxDrainTimeout       time.Duration
	useScaledTimeout      bool
	batchTimeoutFloor     time.Duration
	currentBatchSize      int
	hasDrained            bool
	// hadPending tracks whether the most recent Claim returned records.
	// Used to ensure OnDrained fires only on the transition from
	// "pending records" to "caught up", not on every empty cycle.
	hadPending bool

	idleMu    sync.Mutex
	idleCh    chan struct{}
	idleSince time.Time

	// onBatchComplete is invoked after each drain batch with the number
	// of records successfully sent+completed. Optional.
	onBatchComplete func(n int)
	// onDrained is invoked on the transition to an empty pending set.
	// Optional.
	onDrained func()

	// tokenFn returns the current lease token and whether the caller
	// still holds the lease. The Drainer only processes when the second
	// return value is true.
	tokenFn func() (persistence.LeaseToken, bool)

	// readyFn returns true when the egress transport is connected and
	// ready to send. When set and returning false, the drainer skips the
	// drain cycle entirely, preventing unnecessary Claim calls that would
	// increment replay_count on records that cannot be sent anyway.
	readyFn func(ctx context.Context) bool
}

// Config holds the configuration for a Drainer.
type Config struct {
	OutboxStore         ports.OutboxStore
	LeaseStore          ports.LeaseStore
	Sender              ports.Sender
	DLQ                 *dlq.Router
	RouteID             string
	PartitionKey        string
	LeaseID             string
	OwnerID             string
	Policy              routing.RoutePolicy
	Strategy            persistence.DrainStrategy
	DrainBatchSize      int
	DrainMaxBatchSize   int
	DrainMaxConcurrency int
	// DrainTimeout is the legacy fixed timeout that bounded the entire
	// batch (claim + all sends). It is retained for backward
	// compatibility: when both PerRecordDrainTimeout and MaxDrainTimeout
	// are zero, the drainer falls back to using this value as a fixed
	// batch ceiling. When either of the new fields is set, DrainTimeout
	// no longer participates in the scaled computation; prefer the new
	// fields for new code.
	DrainTimeout time.Duration
	// PerRecordDrainTimeout is the time budget allocated per record in
	// the batch under the scaled timeout formula
	// (batchCount * PerRecordDrainTimeout, capped by MaxDrainTimeout).
	// Default: 3s. Leaving this zero along with MaxDrainTimeout
	// preserves legacy DrainTimeout semantics.
	PerRecordDrainTimeout time.Duration
	// MaxDrainTimeout is the absolute ceiling for the per-batch
	// timeout, regardless of batch size. Default: 10s (matches the
	// previous DrainTimeout default so the worst-case is unchanged).
	MaxDrainTimeout   time.Duration
	BatchTimeoutFloor time.Duration
	Metrics           ports.MetricsExporter
	Hook              ports.DeliveryHook
	Logger            *slog.Logger
	Clock             clock.Clock
	TokenFn           func() (persistence.LeaseToken, bool)

	// ReadyFn optionally gates drain cycles on egress transport
	// readiness. When the MQTT session is disconnected, skipping
	// drains prevents replay_count from being exhausted by
	// repeated failed Claim+Send cycles during broker downtime.
	ReadyFn func(ctx context.Context) bool

	// OnBatchComplete is invoked after each drain batch with the number
	// of records successfully sent+completed. Non-blocking; callers must
	// not block in the callback. Optional.
	OnBatchComplete func(n int)

	// OnDrained is invoked when the drain loop observes an empty pending
	// set (the queue is caught up), after previously having seen pending
	// records. Non-blocking. Optional.
	OnDrained func()
}

const (
	absoluteMaxBatchSize = 10000

	// defaultPerRecordDrainTimeout is the per-record time budget used
	// when MaxDrainTimeout is set but PerRecordDrainTimeout is not. It
	// reflects the old 10s/~3-record assumption that motivated the
	// scaling formula.
	defaultPerRecordDrainTimeout = 3 * time.Second
	// defaultMaxDrainTimeout is the absolute ceiling used when
	// PerRecordDrainTimeout is set but MaxDrainTimeout is not. Matches
	// the legacy DrainTimeout default so the worst-case ceiling is
	// unchanged.
	defaultMaxDrainTimeout = 10 * time.Second
)

// New creates a Drainer from a Config.
func New(cfg Config) *Drainer {
	if cfg.Strategy == nil {
		cfg.Strategy = persistence.NewAdaptiveBackoff(0, 0, 0)
	}
	if cfg.DrainBatchSize <= 0 {
		cfg.DrainBatchSize = 100
	}
	cfg.DrainBatchSize = min(cfg.DrainBatchSize, absoluteMaxBatchSize)
	if cfg.DrainMaxBatchSize <= 0 {
		cfg.DrainMaxBatchSize = 500
	}
	cfg.DrainMaxBatchSize = min(cfg.DrainMaxBatchSize, absoluteMaxBatchSize)
	cfg.DrainMaxBatchSize = max(cfg.DrainMaxBatchSize, cfg.DrainBatchSize)
	if cfg.DrainMaxConcurrency <= 0 {
		cfg.DrainMaxConcurrency = 10
	}
	if cfg.DLQ == nil {
		cfg.DLQ = dlq.New(nil)
	}
	// useScaledTimeout captures whether the caller opted into the new
	// scaled formula. When either new field is non-zero we use
	// ComputeBatchDeadline; otherwise we preserve legacy behavior by
	// bounding the batch with a fixed DrainTimeout.
	useScaledTimeout := cfg.PerRecordDrainTimeout > 0 || cfg.MaxDrainTimeout > 0
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 10 * time.Second
	}
	if cfg.BatchTimeoutFloor <= 0 {
		cfg.BatchTimeoutFloor = 2 * time.Second
	}
	if cfg.TokenFn == nil {
		cfg.TokenFn = func() (persistence.LeaseToken, bool) { return persistence.LeaseToken{}, false }
	}
	leaseID := cfg.LeaseID
	if leaseID == "" {
		leaseID = cfg.PartitionKey
	}
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	hk := cfg.Hook
	if hk == nil {
		hk = ports.NoopDeliveryHook{}
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System
	}
	return &Drainer{
		outboxStore:           cfg.OutboxStore,
		leaseStore:            cfg.LeaseStore,
		sender:                cfg.Sender,
		dlq:                   cfg.DLQ,
		routeID:               cfg.RouteID,
		partitionKey:          cfg.PartitionKey,
		leaseID:               leaseID,
		ownerID:               cfg.OwnerID,
		policy:                cfg.Policy,
		strategy:              cfg.Strategy,
		batchSize:             cfg.DrainBatchSize,
		maxBatchSize:          cfg.DrainMaxBatchSize,
		maxConcurrency:        cfg.DrainMaxConcurrency,
		drainTimeout:          cfg.DrainTimeout,
		perRecordDrainTimeout: cfg.PerRecordDrainTimeout,
		maxDrainTimeout:       cfg.MaxDrainTimeout,
		useScaledTimeout:      useScaledTimeout,
		batchTimeoutFloor:     cfg.BatchTimeoutFloor,
		currentBatchSize:      cfg.DrainBatchSize,
		metrics:               m,
		hook:                  hk,
		logger:                cfg.Logger,
		clk:                   clk,
		tokenFn:               cfg.TokenFn,
		readyFn:               cfg.ReadyFn,
		onBatchComplete:       cfg.OnBatchComplete,
		onDrained:             cfg.OnDrained,
		idleCh:                make(chan struct{}),
	}
}

// RouteID returns the route identifier the drainer was configured with.
func (d *Drainer) RouteID() string { return d.routeID }

// PartitionKey returns the outbox partition key the drainer claims from.
func (d *Drainer) PartitionKey() string { return d.partitionKey }

// fireBatchComplete invokes the OnBatchComplete callback with a panic
// guard so a faulty callback cannot kill the drain loop.
func (d *Drainer) fireBatchComplete(n int) {
	if d.onBatchComplete == nil {
		return
	}
	defer func() { _ = recover() }()
	d.onBatchComplete(n)
}

// fireDrained invokes the OnDrained callback with a panic guard.
func (d *Drainer) fireDrained() {
	if d.onDrained == nil {
		return
	}
	defer func() { _ = recover() }()
	d.onDrained()
}

// IdleSince returns the time at which the drainer last transitioned to
// idle (no pending outbox records) and true, or a zero time and false
// if the drainer is not currently idle.
func (d *Drainer) IdleSince() (time.Time, bool) {
	d.idleMu.Lock()
	defer d.idleMu.Unlock()
	if d.idleSince.IsZero() {
		return time.Time{}, false
	}
	return d.idleSince, true
}

// WaitIdle blocks until the drainer has been idle (no pending outbox
// records) for at least minQuiet continuous time. It returns nil once
// the condition is met, or ctx.Err() if the context is cancelled.
func (d *Drainer) WaitIdle(ctx context.Context, minQuiet time.Duration) error {
	for {
		d.idleMu.Lock()
		ch := d.idleCh
		since := d.idleSince
		d.idleMu.Unlock()

		if !since.IsZero() && d.clk.Since(since) >= minQuiet {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		case <-d.clk.After(minQuiet):
		}
	}
}

func (d *Drainer) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if d.logger == nil || !d.logger.Enabled(ctx, level) {
		return
	}
	allArgs := append([]any{"partition", d.partitionKey, "route", d.routeID}, args...)
	d.logger.Log(ctx, level, msg, allArgs...)
}
