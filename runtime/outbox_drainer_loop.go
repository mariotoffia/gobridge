package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
)

// Run polls the outbox for pending records and sends them. It blocks
// until the context is cancelled or a fencing error occurs. The polling
// interval is determined by the configured DrainStrategy.
func (d *OutboxDrainer) Run(ctx context.Context) error {
	timer := d.clk.NewTimer(d.strategy.NextInterval(0))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			if drainErr := d.finalDrain(ctx); drainErr != nil {
				d.log(ctx, slog.LevelWarn, "final drain error during shutdown", "error", drainErr)
			}
			return ctx.Err()
		case <-timer.C():
			token, hasLease := d.tokenFn()
			if !hasLease {
				timer.Reset(d.strategy.NextInterval(0))
				continue
			}
			if d.readyFn != nil && !d.readyFn(ctx) {
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
					staleBackoff := max(d.strategy.NextInterval(0), 5*time.Second)
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
// Uses context.WithoutCancel(parent)+WithTimeout so the drain can complete
// during shutdown while retaining trace/correlation values from the parent.
// Claim is bounded by the batch ceiling; in-flight sends may exceed it.
func (d *OutboxDrainer) finalDrain(parent context.Context) error {
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
	// parent is typically cancelled at this point; detach cancellation but
	// preserve values so Health can still be queried.
	if d.readyFn != nil && !d.readyFn(context.WithoutCancel(parent)) {
		return nil
	}
	// Use the worst-case ceiling for the final drain because the batch
	// size is only known after Claim. When the caller opted into the
	// scaled formula this resolves to MaxDrainTimeout; otherwise it
	// resolves to the legacy fixed DrainTimeout.
	finalCeiling := d.drainTimeout
	if d.useScaledTimeout {
		finalCeiling = d.maxDrainTimeout
		if finalCeiling <= 0 {
			finalCeiling = defaultMaxDrainTimeout
		}
	}
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), finalCeiling)
	defer cancel()
	_, err := d.drainBatch(drainCtx, token)
	return err
}

func (d *OutboxDrainer) drainBatch(ctx context.Context, token domain.LeaseToken) (int, error) {
	start := time.Now()
	sessionTag := domain.Tag{Key: domain.TagKeySessionID, Value: d.partitionKey}
	routeTag := domain.Tag{Key: domain.TagKeyRouteID, Value: d.routeID}

	if logging.DebugEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelDebug, "drain batch starting",
			"partition_key", d.partitionKey,
			"batch_size", d.currentBatchSize,
		)
	}

	records, err := d.outboxStore.Claim(ctx, d.partitionKey, d.ownerID, token, d.currentBatchSize)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		if d.hadPending {
			d.hadPending = false
			d.fireDrained()

			d.idleMu.Lock()
			d.idleSince = d.clk.Now()
			old := d.idleCh
			d.idleCh = make(chan struct{})
			d.idleMu.Unlock()
			close(old)
		}
		return 0, nil
	}

	d.idleMu.Lock()
	d.idleSince = time.Time{}
	d.idleMu.Unlock()

	d.hadPending = true

	if logging.DebugEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelDebug, "claimed records",
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
	//
	// The outer ceiling is the scaled batch deadline
	// (batchCount * PerRecordDrainTimeout, capped by MaxDrainTimeout),
	// which falls back to the legacy fixed DrainTimeout when the caller
	// has not opted into the scaled formula.
	ceiling := d.batchDeadline(len(records))
	batchTimeout := max(time.Duration(float64(d.policy.SendTimeout)*1.5), d.batchTimeoutFloor)
	batchTimeout = min(batchTimeout, ceiling)
	// workCtx must survive caller ctx cancellation so in-flight sends can
	// settle cleanly, but retains trace/correlation values from parent.
	workCtx, workCancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)

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
		wg.Go(func() {
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
		})
	}
	wg.Wait()
	batchCancel()
	workCancel()

	d.metrics.Timer(domain.MetricOutboxDrainLatency, time.Since(start), sessionTag)

	successTotal := int(atomic.LoadInt64(&successCount))
	d.fireBatchComplete(successTotal)

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
		d.logger.Log(ctx, logging.LevelDebug, "drain batch complete",
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
		d.currentBatchSize = min(d.currentBatchSize*2, d.maxBatchSize)
	} else if drained == 0 {
		d.currentBatchSize = max(d.currentBatchSize/2, d.batchSize)
	}
}
