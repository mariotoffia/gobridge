package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// PipelineImpl implements types.Pipeline.
// It connects a Source to a Target through a middleware chain.
type PipelineImpl struct {
	id          string
	mode        types.PipelineMode
	source      types.Source
	target      types.Target
	middlewares *types.MiddlewareChain

	// stats tracks pipeline statistics
	stats PipelineStats

	// retryManager handles message retries (optional)
	retryManager types.RetryManager

	// dlq is the dead letter queue for permanent failures (optional)
	dlq types.DeadLetterQueue

	// running indicates if the pipeline is currently running
	running atomic.Bool

	// cancel cancels the pipeline context
	cancel context.CancelFunc

	// wg tracks running goroutines
	wg sync.WaitGroup

	// mu protects pipeline state
	mu sync.RWMutex
}

// PipelineStats tracks runtime statistics for a pipeline.
type PipelineStats struct {
	MessagesReceived atomic.Int64
	MessagesSent     atomic.Int64
	MessagesFailed   atomic.Int64
	MessagesRetried  atomic.Int64
	MessagesDropped  atomic.Int64
	InFlight         atomic.Int64
}

// Ensure PipelineImpl implements types.Pipeline and types.StatsProvider
var (
	_ types.Pipeline      = (*PipelineImpl)(nil)
	_ types.StatsProvider = (*PipelineImpl)(nil)
)

// PipelineOption configures a Pipeline.
type PipelineOption func(*PipelineImpl)

// WithRetryManager sets the retry manager for the pipeline.
func WithRetryManager(manager types.RetryManager) PipelineOption {
	return func(p *PipelineImpl) {
		p.retryManager = manager
	}
}

// WithDeadLetterQueue sets the dead letter queue for the pipeline.
func WithDeadLetterQueue(dlq types.DeadLetterQueue) PipelineOption {
	return func(p *PipelineImpl) {
		p.dlq = dlq
	}
}

// NewPipeline creates a new Pipeline.
func NewPipeline(
	id string,
	mode types.PipelineMode,
	source types.Source,
	target types.Target,
	middlewares *types.MiddlewareChain,
	opts ...PipelineOption,
) *PipelineImpl {
	if middlewares == nil {
		middlewares = types.NewMiddlewareChain()
	}

	p := &PipelineImpl{
		id:          id,
		mode:        mode,
		source:      source,
		target:      target,
		middlewares: middlewares,
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// GetID returns the unique identifier of the pipeline.
func (p *PipelineImpl) GetID() string {
	return p.id
}

// GetMode returns whether this pipeline is simplex or duplex.
func (p *PipelineImpl) GetMode() types.PipelineMode {
	return p.mode
}

// Source returns the message source for this pipeline.
func (p *PipelineImpl) Source() types.Source {
	return p.source
}

// Target returns the message target for this pipeline.
func (p *PipelineImpl) Target() types.Target {
	return p.target
}

// Middlewares returns the middleware chain for this pipeline.
func (p *PipelineImpl) Middlewares() *types.MiddlewareChain {
	return p.middlewares
}

// Stats returns current pipeline statistics.
func (p *PipelineImpl) Stats() types.PipelineStats {
	return types.PipelineStats{
		MessagesReceived: p.stats.MessagesReceived.Load(),
		MessagesSent:     p.stats.MessagesSent.Load(),
		MessagesFailed:   p.stats.MessagesFailed.Load(),
		MessagesRetried:  p.stats.MessagesRetried.Load(),
		MessagesDropped:  p.stats.MessagesDropped.Load(),
		InFlight:         p.stats.InFlight.Load(),
	}
}

// Start begins processing messages from source to target.
// The pipeline runs until the context is cancelled or Close is called.
func (p *PipelineImpl) Start(ctx context.Context) error {
	if !p.running.CompareAndSwap(false, true) {
		return errors.New("pipeline already running")
	}

	// Create cancellable context
	ctx, p.cancel = context.WithCancel(ctx)

	// Start the source
	if err := p.source.Start(ctx); err != nil {
		p.running.Store(false)
		return fmt.Errorf("failed to start source: %w", err)
	}

	// Start message processing goroutine
	p.wg.Add(1)
	go p.processMessages(ctx)

	return nil
}

// processMessages reads from the source and processes each message.
func (p *PipelineImpl) processMessages(ctx context.Context) {
	defer p.wg.Done()

	messages := p.source.Messages()
	for {
		select {
		case <-ctx.Done():
			return
		case srcMsg, ok := <-messages:
			if !ok {
				// Source channel closed
				return
			}
			p.handleMessage(ctx, srcMsg)
		}
	}
}

// handleMessage processes a single message through the middleware chain and target.
func (p *PipelineImpl) handleMessage(ctx context.Context, srcMsg *types.SourceMessage) {
	p.stats.MessagesReceived.Add(1)
	p.stats.InFlight.Add(1)
	defer p.stats.InFlight.Add(-1)

	msg := &srcMsg.Message

	// Define the final handler that sends to the target
	finalHandler := func(ctx context.Context, msg *types.Message) error {
		return p.target.Send(ctx, *msg)
	}

	// Process through middleware chain
	err := p.middlewares.Process(ctx, msg, finalHandler)

	if err == nil {
		// Success - acknowledge the message
		p.stats.MessagesSent.Add(1)
		if ackErr := srcMsg.Ack(); ackErr != nil {
			// Log ack error but don't fail - message was processed
			_ = ackErr // TODO: Add logging
		}
		return
	}

	// Handle error
	p.handleError(ctx, srcMsg, msg, err)
}

// handleError processes errors according to classification.
func (p *PipelineImpl) handleError(ctx context.Context, srcMsg *types.SourceMessage, msg *types.Message, err error) {
	p.stats.MessagesFailed.Add(1)

	// Check if error is recoverable
	if types.IsRecoverableError(err) {
		// Try to enqueue for retry if manager is available
		if p.retryManager != nil {
			if enqErr := p.retryManager.Enqueue(ctx, *msg, err); enqErr == nil {
				// Message queued for retry - ack the source
				p.stats.MessagesRetried.Add(1)
				_ = srcMsg.Ack()
				return
			}
		}

		// No retry manager or enqueue failed - nack to source
		_ = srcMsg.Nack(err)
		return
	}

	// Permanent error - send to DLQ if available
	if p.dlq != nil {
		if dlqErr := p.dlq.Send(ctx, *msg, err); dlqErr == nil {
			// Message archived - ack the source
			p.stats.MessagesDropped.Add(1)
			_ = srcMsg.Ack()
			return
		}
	}

	// No DLQ or DLQ failed - drop the message
	p.stats.MessagesDropped.Add(1)
	_ = srcMsg.Ack() // Ack to prevent infinite redelivery
}

// Close stops the pipeline and releases resources.
func (p *PipelineImpl) Close() error {
	if !p.running.CompareAndSwap(true, false) {
		return nil // Already stopped
	}

	// Cancel context to stop processing
	if p.cancel != nil {
		p.cancel()
	}

	// Wait for goroutines to finish
	p.wg.Wait()

	// Close source and target
	var errs []error

	if err := p.source.Close(); err != nil {
		errs = append(errs, fmt.Errorf("source close: %w", err))
	}

	if err := p.target.Close(); err != nil {
		errs = append(errs, fmt.Errorf("target close: %w", err))
	}

	return errors.Join(errs...)
}

// IsRunning returns true if the pipeline is currently running.
func (p *PipelineImpl) IsRunning() bool {
	return p.running.Load()
}
