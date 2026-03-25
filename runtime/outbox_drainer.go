package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// OutboxDrainer claims pending outbox records for a partition and sends
// them through the target sender. It validates fencing tokens to prevent
// stale owners from sending after a lease transfer.
type OutboxDrainer struct {
	outboxStore   ports.OutboxStore
	leaseStore    ports.LeaseStore
	sender        ports.Sender
	dlq           *DLQRouter
	routeID       string
	partitionKey  string
	leaseID       string
	ownerID       string
	policy        domain.RoutePolicy
	drainInterval time.Duration
	batchSize     int
	metrics       ports.MetricsExporter
	logger        *slog.Logger

	// tokenFn returns the current lease token and whether the caller
	// still holds the lease. The OutboxDrainer only processes when
	// the second return value is true.
	tokenFn func() (domain.LeaseToken, bool)
}

// OutboxDrainerConfig holds the configuration for an OutboxDrainer.
type OutboxDrainerConfig struct {
	OutboxStore   ports.OutboxStore
	LeaseStore    ports.LeaseStore
	Sender        ports.Sender
	DLQ           *DLQRouter
	RouteID       string
	PartitionKey  string
	LeaseID       string
	OwnerID       string
	Policy        domain.RoutePolicy
	DrainInterval time.Duration
	BatchSize     int
	Metrics       ports.MetricsExporter
	Logger        *slog.Logger
	TokenFn       func() (domain.LeaseToken, bool)
}

// NewOutboxDrainerFromConfig creates an OutboxDrainer from a config struct.
func NewOutboxDrainerFromConfig(cfg OutboxDrainerConfig) *OutboxDrainer {
	return newOutboxDrainer(cfg)
}

func newOutboxDrainer(cfg OutboxDrainerConfig) *OutboxDrainer {
	if cfg.DrainInterval == 0 {
		cfg.DrainInterval = time.Second
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.DLQ == nil {
		cfg.DLQ = NewDLQRouter(nil)
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
	return &OutboxDrainer{
		outboxStore:   cfg.OutboxStore,
		leaseStore:    cfg.LeaseStore,
		sender:        cfg.Sender,
		dlq:           cfg.DLQ,
		routeID:       cfg.RouteID,
		partitionKey:  cfg.PartitionKey,
		leaseID:       leaseID,
		ownerID:       cfg.OwnerID,
		policy:        cfg.Policy,
		drainInterval: cfg.DrainInterval,
		batchSize:     cfg.BatchSize,
		metrics:       m,
		logger:        cfg.Logger,
		tokenFn:       cfg.TokenFn,
	}
}

// Run polls the outbox for pending records and sends them. It blocks
// until the context is cancelled or a fencing error occurs.
func (d *OutboxDrainer) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.drainInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			token, hasLease := d.tokenFn()
			if !hasLease {
				continue
			}
			if err := d.drainBatch(ctx, token); err != nil {
				if errors.Is(err, domain.ErrStaleFencingToken) {
					d.log(ctx, slog.LevelError, "stale fencing token, stopping drain")
					return err
				}
				d.log(ctx, slog.LevelWarn, "drain batch error", "error", err)
			}
		}
	}
}

func (d *OutboxDrainer) drainBatch(ctx context.Context, token domain.LeaseToken) error {
	start := time.Now()
	sessionTag := domain.Tag{Key: domain.TagKeySessionID, Value: d.partitionKey}
	routeTag := domain.Tag{Key: domain.TagKeyRouteID, Value: d.routeID}

	if d.leaseStore != nil {
		info, err := d.leaseStore.Current(ctx, d.leaseID)
		if err != nil {
			return err
		}
		if info.Version != token.Version || info.Owner != token.Owner {
			return domain.ErrStaleFencingToken
		}
	}

	records, err := d.outboxStore.Claim(ctx, d.partitionKey, d.ownerID, token, d.batchSize)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}

	for i := range records {
		rec := &records[i]
		if rec.ReplayCount > 1 {
			d.metrics.Counter(domain.MetricOutboxReplayCount, 1, routeTag)
		}
		if err := d.processRecord(ctx, rec, token); err != nil {
			d.log(ctx, slog.LevelWarn, "record processing failed",
				"record_id", rec.ID, "error", err)
		}
	}

	d.metrics.Timer(domain.MetricOutboxDrainLatency, time.Since(start), sessionTag)
	return nil
}

func (d *OutboxDrainer) processRecord(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	routeTag := domain.Tag{Key: domain.TagKeyRouteID, Value: d.routeID}

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

	sendErr := d.sender.Send(ctx, env)
	if sendErr == nil {
		d.metrics.Counter(domain.MetricOutboxCompletions, 1, routeTag)
		return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
	}

	be, ok := domain.AsBridgeError(sendErr)
	if ok && be.Class != domain.ErrorTransient {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", sendErr, rec.ReplayCount); dlqErr != nil {
			d.log(ctx, slog.LevelError, "DLQ write failed, will not complete record",
				"record_id", rec.ID, "dlq_error", dlqErr)
			return dlqErr
		}
		return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
	}

	d.log(ctx, slog.LevelWarn, "transient send failure, will retry on next drain",
		"record_id", rec.ID, "error", sendErr)
	return nil
}

func (d *OutboxDrainer) handleExpired(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	if d.policy.OnExpired == domain.ExpiredDLQ {
		if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", domain.ErrMessageExpired, rec.ReplayCount); dlqErr != nil {
			return dlqErr
		}
	}
	return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
}

func (d *OutboxDrainer) handlePoison(ctx context.Context, rec *domain.OutboxRecord, token domain.LeaseToken) error {
	env := &rec.Envelope
	poisonErr := domain.NewBridgeError("POISON_MESSAGE", domain.ErrorPermanent, "replay count exceeded")
	if dlqErr := d.dlq.Route(ctx, env, d.routeID, rec.BindingID, rec.SessionID, "", poisonErr, rec.ReplayCount); dlqErr != nil {
		return dlqErr
	}
	return d.outboxStore.Complete(ctx, []string{rec.ID}, token)
}

func (d *OutboxDrainer) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if d.logger == nil {
		return
	}
	allArgs := append([]any{"partition", d.partitionKey, "route", d.routeID}, args...)
	d.logger.Log(ctx, level, msg, allArgs...)
}
