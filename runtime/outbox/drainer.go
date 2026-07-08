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
	// poisonMinAge is the minimum wall-clock age a record must reach BEFORE
	// replay-count exhaustion is allowed to poison it to the DLQ (finding 8).
	// Replay count alone poisons healthy records during an egress outage that
	// outlasts the (small) replay budget; requiring a generous minimum age as
	// well decouples poisoning from a transient outage that merely burned the
	// count quickly. It is also the guard against a subtler over-count: a
	// record's ReplayCount increments on EVERY claim — including batch-deadline
	// deferrals and stale-claim reclaims where no send ever failed — so replay
	// exhaustion is not by itself proof of poison. The age gate is therefore a
	// hard AND-condition, never an OR. Default: max(5×SendTimeout, 2m).
	poisonMinAge time.Duration
	// replayBudget bounds the TOTAL wall-clock time, measured from a record's
	// FirstAttemptedAt, that redelivery may span before the record is poisoned
	// to the DLQ (WP-REPLAY-BUDGET). It is the age half of the poison AND-gate:
	// a transient egress outage that merely burns the replay COUNT quickly can
	// no longer poison a healthy record until real time — replayBudget — has
	// elapsed since the first attempt. poisonMinAge remains the LEGACY fallback,
	// used only for records whose FirstAttemptedAt is zero (pre-budget schema).
	// Default: cfg.Policy.ReplayBudget, itself defaulting to
	// routing.DefaultReplayBudget (15m).
	replayBudget     time.Duration
	currentBatchSize int
	hasDrained       bool
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

	// lastNoLeaseWarn rate-limits the "drain skipped: no lease" warning so a
	// permanently lease-less drainer (e.g. a non-exclusive session paired with
	// shared_outbox) is observable without flooding the log every poll cycle.
	// Accessed only from the Run goroutine — no lock required.
	lastNoLeaseWarn time.Time

	// expireInterval bounds how often maybeExpire sweeps expired-but-unclaimed
	// pending records (finding 19). lastExpire tracks the last sweep. Both are
	// touched only from the Run goroutine — no lock required.
	expireInterval time.Duration
	lastExpire     time.Time
}

// noLeaseWarnInterval bounds how often the drain-skipped-no-lease warning is
// emitted per drainer (finding 11).
const noLeaseWarnInterval = 30 * time.Second

// Config holds the configuration for a Drainer.
type Config struct {
	OutboxStore         ports.OutboxStore
	LeaseStore          ports.LeaseStore
	Sender              ports.Sender
	DLQ                 *dlq.Router
	RouteID             string
	PartitionKey        string
	LeaseID             string
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
	// PoisonMinAge is the minimum record age required, IN ADDITION to
	// replay-count exhaustion, before a record is poisoned to the DLQ
	// (finding 8). Zero selects the default max(5×SendTimeout, 2m).
	PoisonMinAge time.Duration
	// ReplayBudget bounds the total wall-clock time from a record's
	// FirstAttemptedAt before it is poisoned to the DLQ (WP-REPLAY-BUDGET).
	// Zero derives it from Policy.ReplayBudget, which itself defaults to
	// routing.DefaultReplayBudget (15m). This is the primary poison age gate;
	// PoisonMinAge is retained as the fallback for legacy records that carry a
	// zero FirstAttemptedAt.
	ReplayBudget time.Duration
	// ExpireInterval is how often the drainer sweeps expired-but-unclaimed
	// pending records to the expired terminal state (finding 19). Zero
	// selects defaultExpireInterval. The sweep runs only under lease
	// ownership so exactly one instance expires per partition.
	ExpireInterval time.Duration
	Metrics        ports.MetricsExporter
	Hook           ports.DeliveryHook
	Logger         *slog.Logger
	Clock          clock.Clock
	TokenFn        func() (persistence.LeaseToken, bool)

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

	// defaultExpireInterval bounds how often the drainer sweeps
	// expired-but-unclaimed pending records to the expired terminal state
	// (finding 19). The sweep is a cheap indexed status+time scan; a slow
	// cadence keeps expired records from lingering forever without adding
	// per-cycle cost.
	defaultExpireInterval = time.Minute
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
	if cfg.PoisonMinAge <= 0 {
		// Generous default: a record must be at least this old before
		// replay-count exhaustion may poison it. max(5×SendTimeout, 2m)
		// comfortably clears a transient egress outage that slipped past
		// ReadyFn and burned the replay budget in seconds.
		poisonAge := 5 * cfg.Policy.SendTimeout
		if poisonAge < 2*time.Minute {
			poisonAge = 2 * time.Minute
		}
		cfg.PoisonMinAge = poisonAge
	}
	if cfg.ReplayBudget <= 0 {
		// Derive from the route policy (WithDefaults sets 15m); the second
		// guard falls back to the package default so a bare Config still
		// bounds total replay burn.
		cfg.ReplayBudget = cfg.Policy.ReplayBudget
	}
	if cfg.ReplayBudget <= 0 {
		cfg.ReplayBudget = routing.DefaultReplayBudget
	}
	if cfg.ExpireInterval <= 0 {
		cfg.ExpireInterval = defaultExpireInterval
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
		poisonMinAge:          cfg.PoisonMinAge,
		replayBudget:          cfg.ReplayBudget,
		expireInterval:        cfg.ExpireInterval,
		// Seed lastExpire to now so the first Expire sweep waits a full
		// interval (finding 19). This gives the active drain path priority:
		// freshly-claimable expired records are handled per-record by
		// processRecord (which emits OutboxExpiredBeforeSend and honors the
		// OnExpired policy — DLQ/drop) instead of being blanket-swept to the
		// "expired" terminal state by the bulk Expire backstop.
		lastExpire:       clk.Now(),
		currentBatchSize: cfg.DrainBatchSize,
		metrics:          m,
		hook:             hk,
		logger:           cfg.Logger,
		clk:              clk,
		tokenFn:          cfg.TokenFn,
		readyFn:          cfg.ReadyFn,
		onBatchComplete:  cfg.OnBatchComplete,
		onDrained:        cfg.OnDrained,
		idleCh:           make(chan struct{}),
		// Min1: seed idleSince at construction so a drainer that NEVER sees a
		// pending record still reports idle after minQuiet. idleSince was
		// previously set only on a pending->empty transition, so an always-empty
		// drainer never satisfied WaitIdle. A later Claim that returns records
		// clears this (drainBatch sets idleSince = zero) and the normal
		// transition machinery takes over.
		idleSince: clk.Now(),
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

// warnDrainSkippedNoLease emits a rate-limited warning when a drain cycle is
// skipped because the drainer holds no lease (finding 11). It fires at most
// once per noLeaseWarnInterval per drainer so a permanently lease-less
// configuration is visible without flooding the log.
func (d *Drainer) warnDrainSkippedNoLease(ctx context.Context) {
	now := d.clk.Now()
	if !d.lastNoLeaseWarn.IsZero() && now.Sub(d.lastNoLeaseWarn) < noLeaseWarnInterval {
		return
	}
	d.lastNoLeaseWarn = now
	d.log(ctx, slog.LevelWarn,
		"outbox drain skipped: no lease held for partition; a non-exclusive session never acquires a lease, so shared_outbox records for this partition will not drain until an exclusive owner exists",
		"partition_key", d.partitionKey)
}
