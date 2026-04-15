package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// OutboxDrainer claims pending outbox records for a partition and sends
// them through the target sender. It validates fencing tokens to prevent
// stale owners from sending after a lease transfer.
type OutboxDrainer struct {
	outboxStore    ports.OutboxStore
	leaseStore     ports.LeaseStore
	sender         ports.Sender
	dlq            *DLQRouter
	routeID        string
	partitionKey   string
	leaseID        string
	ownerID        string
	policy         domain.RoutePolicy
	strategy       domain.DrainStrategy
	batchSize      int
	maxBatchSize   int
	maxConcurrency int
	metrics        ports.MetricsExporter
	hook           ports.DeliveryHook
	logger         *slog.Logger

	drainTimeout     time.Duration
	currentBatchSize int
	hasDrained       bool

	// tokenFn returns the current lease token and whether the caller
	// still holds the lease. The OutboxDrainer only processes when
	// the second return value is true.
	tokenFn func() (domain.LeaseToken, bool)

	// readyFn returns true when the egress transport is connected and
	// ready to send. When set and returning false, the drainer skips the
	// drain cycle entirely, preventing unnecessary Claim calls that would
	// increment replay_count on records that cannot be sent anyway.
	readyFn func() bool
}

// OutboxDrainerConfig holds the configuration for an OutboxDrainer.
type OutboxDrainerConfig struct {
	OutboxStore    ports.OutboxStore
	LeaseStore     ports.LeaseStore
	Sender         ports.Sender
	DLQ            *DLQRouter
	RouteID        string
	PartitionKey   string
	LeaseID        string
	OwnerID        string
	Policy         domain.RoutePolicy
	Strategy            domain.DrainStrategy
	DrainBatchSize      int
	DrainMaxBatchSize   int
	DrainMaxConcurrency int
	DrainTimeout        time.Duration
	Metrics        ports.MetricsExporter
	Hook           ports.DeliveryHook
	Logger         *slog.Logger
	TokenFn        func() (domain.LeaseToken, bool)

	// ReadyFn optionally gates drain cycles on egress transport
	// readiness. When the MQTT session is disconnected, skipping
	// drains prevents replay_count from being exhausted by
	// repeated failed Claim+Send cycles during broker downtime.
	ReadyFn func() bool
}

// NewOutboxDrainerFromConfig creates an OutboxDrainer from a config struct.
func NewOutboxDrainerFromConfig(cfg OutboxDrainerConfig) *OutboxDrainer {
	return newOutboxDrainer(cfg)
}

const absoluteMaxBatchSize = 10000

func newOutboxDrainer(cfg OutboxDrainerConfig) *OutboxDrainer {
	if cfg.Strategy == nil {
		cfg.Strategy = domain.NewAdaptiveBackoff(0, 0, 0)
	}
	if cfg.DrainBatchSize <= 0 {
		cfg.DrainBatchSize = 100
	}
	if cfg.DrainBatchSize > absoluteMaxBatchSize {
		cfg.DrainBatchSize = absoluteMaxBatchSize
	}
	if cfg.DrainMaxBatchSize <= 0 {
		cfg.DrainMaxBatchSize = 500
	}
	if cfg.DrainMaxBatchSize > absoluteMaxBatchSize {
		cfg.DrainMaxBatchSize = absoluteMaxBatchSize
	}
	if cfg.DrainMaxBatchSize < cfg.DrainBatchSize {
		cfg.DrainMaxBatchSize = cfg.DrainBatchSize
	}
	if cfg.DrainMaxConcurrency <= 0 {
		cfg.DrainMaxConcurrency = 10
	}
	if cfg.DLQ == nil {
		cfg.DLQ = NewDLQRouter(nil)
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 10 * time.Second
	}
	if cfg.TokenFn == nil {
		cfg.TokenFn = func() (domain.LeaseToken, bool) { return domain.LeaseToken{}, false }
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
	return &OutboxDrainer{
		outboxStore:      cfg.OutboxStore,
		leaseStore:       cfg.LeaseStore,
		sender:           cfg.Sender,
		dlq:              cfg.DLQ,
		routeID:          cfg.RouteID,
		partitionKey:     cfg.PartitionKey,
		leaseID:          leaseID,
		ownerID:          cfg.OwnerID,
		policy:           cfg.Policy,
		strategy:         cfg.Strategy,
		batchSize:        cfg.DrainBatchSize,
		maxBatchSize:     cfg.DrainMaxBatchSize,
		maxConcurrency:   cfg.DrainMaxConcurrency,
		drainTimeout:     cfg.DrainTimeout,
		currentBatchSize: cfg.DrainBatchSize,
		metrics:          m,
		hook:             hk,
		logger:           cfg.Logger,
		tokenFn:          cfg.TokenFn,
		readyFn:          cfg.ReadyFn,
	}
}

// Run polls the outbox for pending records and sends them. It blocks
// until the context is cancelled or a fencing error occurs. The polling
// interval is determined by the configured DrainStrategy.
func (d *OutboxDrainer) Run(ctx context.Context) error {
	timer := time.NewTimer(d.strategy.NextInterval(0))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			if drainErr := d.finalDrain(); drainErr != nil {
				d.log(ctx, slog.LevelWarn, "final drain error during shutdown", "error", drainErr)
			}
			return ctx.Err()
		case <-timer.C:
			token, hasLease := d.tokenFn()
			if !hasLease {
				timer.Reset(d.strategy.NextInterval(0))
				continue
			}
			if d.readyFn != nil && !d.readyFn() {
				if logging.TraceEnabled(d.logger) {
					d.logger.Log(ctx, logging.LevelTrace, "egress not ready, skipping drain cycle",
						"partition_key", d.partitionKey)
				}
				timer.Reset(d.strategy.NextInterval(0))
				continue
			}
			d.hasDrained = true
			n, err := d.drainBatch(ctx, token)
			if err != nil {
				if errors.Is(err, domain.ErrStaleFencingToken) {
					d.log(ctx, slog.LevelWarn, "stale fencing token, waiting for new lease")
					staleBackoff := d.strategy.NextInterval(0)
					if staleBackoff < 5*time.Second {
						staleBackoff = 5 * time.Second
					}
					timer.Reset(staleBackoff)
					continue
				}
				d.log(ctx, slog.LevelWarn, "drain batch error", "error", err)
			}
			d.adaptBatchSize(n)
			timer.Reset(d.strategy.NextInterval(n))
		}
	}
}

// finalDrain performs one last drain after Run's context is cancelled.
// Uses context.Background() so the drain can complete during shutdown.
// Claim is bounded by DrainTimeout; in-flight sends may exceed it.
func (d *OutboxDrainer) finalDrain() error {
	if !d.hasDrained {
		return nil
	}
	token, hasLease := d.tokenFn()
	if !hasLease {
		return nil
	}
	// Skip final drain if the egress transport is not connected.
	// Draining into a disconnected sender wastes replay_count budget
	// and the records will be re-claimed on the next healthy drain cycle.
	if d.readyFn != nil && !d.readyFn() {
		return nil
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), d.drainTimeout)
	defer cancel()
	_, err := d.drainBatch(drainCtx, token)
	return err
}

func (d *OutboxDrainer) drainBatch(ctx context.Context, token domain.LeaseToken) (int, error) {
	start := time.Now()
	sessionTag := domain.Tag{Key: domain.TagKeySessionID, Value: d.partitionKey}
	routeTag := domain.Tag{Key: domain.TagKeyRouteID, Value: d.routeID}

	if logging.DebugEnabled(d.logger) {
		d.logger.Log(context.Background(), logging.LevelDebug, "drain batch starting",
			"partition_key", d.partitionKey,
			"batch_size", d.currentBatchSize,
		)
	}

	records, err := d.outboxStore.Claim(ctx, d.partitionKey, d.ownerID, token, d.currentBatchSize)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}

	if logging.DebugEnabled(d.logger) {
		d.logger.Log(context.Background(), logging.LevelDebug, "claimed records",
			"count", len(records),
			"partition_key", d.partitionKey,
		)
	}

	sem := make(chan struct{}, d.maxConcurrency)
	var wg sync.WaitGroup
	var successCount int64
	var staleDetected atomic.Bool

	// workCtx gives in-flight goroutines a bounded deadline to finish
	// their send+complete operations even after the parent ctx is cancelled.
	// The timeout is derived from SendTimeout (plus a buffer for Complete)
	// so that the configured SendTimeout is not silently capped.
	batchTimeout := d.policy.SendTimeout + 5*time.Second
	if batchTimeout < d.drainTimeout {
		batchTimeout = d.drainTimeout
	}
	workCtx, workCancel := context.WithTimeout(context.Background(), batchTimeout)

	// batchCtx is cancelled when a stale fencing token is detected so
	// that sibling goroutines abort quickly instead of continuing to
	// send with an invalid token (preventing duplicate deliveries).
	batchCtx, batchCancel := context.WithCancel(workCtx)

loop:
	for i := range records {
		if ctx.Err() != nil || batchCtx.Err() != nil {
			break
		}
		rec := &records[i]
		if rec.ReplayCount > 1 {
			d.metrics.Counter(domain.MetricOutboxReplayCount, 1, routeTag)
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		case <-batchCtx.Done():
			break loop
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					d.log(batchCtx, slog.LevelError, "panic in drain goroutine",
						"record_id", rec.ID, "panic", r)
					d.metrics.Counter(domain.MetricOutboxRecordFailures, 1, routeTag)
				}
			}()
			if err := d.processRecord(batchCtx, rec, token); err != nil {
				if errors.Is(err, domain.ErrStaleFencingToken) {
					staleDetected.Store(true)
					batchCancel()
				} else {
					d.metrics.Counter(domain.MetricOutboxRecordFailures, 1, routeTag)
				}
				d.log(batchCtx, slog.LevelWarn, "record processing failed",
					"record_id", rec.ID, "error", err)
			} else {
				atomic.AddInt64(&successCount, 1)
			}
		}()
	}
	wg.Wait()
	batchCancel()
	workCancel()

	d.metrics.Timer(domain.MetricOutboxDrainLatency, time.Since(start), sessionTag)

	if staleDetected.Load() {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "stale fencing token detected",
				"partition_key", d.partitionKey,
				"token_version", token.Version,
				"owner", d.ownerID,
			)
		}
		return int(atomic.LoadInt64(&successCount)), domain.ErrStaleFencingToken
	}

	if logging.DebugEnabled(d.logger) {
		d.logger.Log(context.Background(), logging.LevelDebug, "drain batch complete",
			"partition_key", d.partitionKey,
			"success_count", atomic.LoadInt64(&successCount),
			"duration", time.Since(start),
		)
	}

	return int(atomic.LoadInt64(&successCount)), nil
}

// adaptBatchSize adjusts currentBatchSize based on throughput.
// Called only from the Run loop goroutine — not concurrent.
//
// Scale-up: when a full batch is drained, double (capped at maxBatchSize).
// Scale-down: on each zero-success cycle, halve currentBatchSize
// (floored at the initial batchSize). This progressively shrinks the
// batch from any previously scaled-up value down to the floor.
func (d *OutboxDrainer) adaptBatchSize(drained int) {
	if drained >= d.currentBatchSize {
		next := d.currentBatchSize * 2
		if next > d.maxBatchSize {
			next = d.maxBatchSize
		}
		d.currentBatchSize = next
	} else if drained == 0 {
		next := d.currentBatchSize / 2
		if next < d.batchSize {
			next = d.batchSize
		}
		d.currentBatchSize = next
	}
}

func (d *OutboxDrainer) processRecord(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	routeTag := domain.Tag{Key: domain.TagKeyRouteID, Value: d.routeID}
	attempt := rec.ReplayCount + 1

	if env.HasExpiry() && env.IsExpired() {
		d.metrics.Counter(domain.MetricOutboxExpiredBeforeSend, 1, routeTag)
		return d.handleExpired(ctx, rec, token)
	}

	if rec.ReplayCount > d.policy.MaxReplayAttempts {
		return d.handlePoison(ctx, rec, token)
	}

	if rec.Address != "" {
		env.Subject = rec.Address
	}
	if rec.DispatchHeaders != nil {
		env.Headers = domain.MergeHeaders(env.Headers, rec.DispatchHeaders, true)
	}

	// Re-check lease before sending to minimize duplicate delivery window.
	if _, hasLease := d.tokenFn(); !hasLease {
		return domain.ErrStaleFencingToken
	}

	sendCtx, sendCancel := context.WithTimeout(ctx, d.policy.SendTimeout)
	defer sendCancel()

	sendErr := d.sender.Send(sendCtx, env)

	d.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Envelope:    env,
		Attempt:     attempt,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         sendErr,
	})

	if sendErr == nil {
		if completeErr := d.outboxStore.Complete(ctx, []string{rec.ID}, token); completeErr != nil {
			d.metrics.Counter(domain.MetricOutboxDuplicateRisk, 1, routeTag)
			d.log(ctx, slog.LevelError, "complete failed after successful send, message may be re-delivered",
				"record_id", rec.ID, "error", completeErr)
			return completeErr
		}
		d.metrics.Counter(domain.MetricOutboxCompletions, 1, routeTag)
		d.metrics.Counter(domain.MetricMessagesSent, 1, routeTag)
		d.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID,
			Envelope:    env,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Terminal:    true,
		})
		return nil
	}

	be, ok := domain.AsBridgeError(sendErr)
	if ok && be.Class != domain.ErrorTransient {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", sendErr, rec.ReplayCount); dlqErr != nil {
			d.log(ctx, slog.LevelError, "DLQ write failed, will not complete record",
				"record_id", rec.ID, "dlq_error", dlqErr)
			return dlqErr
		}
		d.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     d.routeID,
			BindingID:   rec.BindingID,
			Envelope:    env,
			Attempt:     attempt,
			MaxAttempts: d.policy.MaxReplayAttempts,
			Err:         sendErr,
			Terminal:    true,
		})
		return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
	}

	if ctx.Err() != nil {
		d.log(ctx, slog.LevelDebug, "send aborted due to context cancellation",
			"record_id", rec.ID, "error", sendErr)
	} else {
		d.log(ctx, slog.LevelWarn, "transient send failure, will retry on next drain",
			"record_id", rec.ID, "error", sendErr)
	}
	return nil
}

func (d *OutboxDrainer) handleExpired(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	if d.policy.OnExpired == domain.ExpiredDLQ {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", domain.ErrMessageExpired, rec.ReplayCount); dlqErr != nil {
			return dlqErr
		}
	}
	d.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Envelope:    env,
		Attempt:     rec.ReplayCount + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         domain.ErrMessageExpired,
		Terminal:    true,
	})
	return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
}

func (d *OutboxDrainer) handlePoison(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	poisonErr := domain.NewBridgeError(domain.ErrCodePoisonMessage, domain.ErrorPermanent, "replay count exceeded")
	if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", poisonErr, rec.ReplayCount); dlqErr != nil {
		return dlqErr
	}
	d.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     d.routeID,
		BindingID:   rec.BindingID,
		Envelope:    env,
		Attempt:     rec.ReplayCount + 1,
		MaxAttempts: d.policy.MaxReplayAttempts,
		Err:         poisonErr,
		Terminal:    true,
	})
	return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
}

func (d *OutboxDrainer) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if d.logger == nil || !d.logger.Enabled(ctx, level) {
		return
	}
	allArgs := append([]any{"partition", d.partitionKey, "route", d.routeID}, args...)
	d.logger.Log(ctx, level, msg, allArgs...)
}
