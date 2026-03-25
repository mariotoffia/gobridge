package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// RouteImpl implements types.Route.
// A Route represents a chain of Pipelines where messages flow through
// multiple stages of processing.
type RouteImpl struct {
	id        string
	pipelines []types.Pipeline

	// running indicates if the route is currently running
	running atomic.Bool

	// mu protects route state
	mu sync.RWMutex
}

// Ensure RouteImpl implements types.Route
var _ types.Route = (*RouteImpl)(nil)

// NewRoute creates a new Route with the given pipelines.
// Pipelines are executed in order - the output of one pipeline
// should be connected as the input of the next (via shared topics/queues).
func NewRoute(id string, pipelines ...types.Pipeline) *RouteImpl {
	return &RouteImpl{
		id:        id,
		pipelines: pipelines,
	}
}

// GetID returns the unique identifier of the route.
func (r *RouteImpl) GetID() string {
	return r.id
}

// Pipelines returns all pipelines in this route, in order.
func (r *RouteImpl) Pipelines() []types.Pipeline {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Return a copy to prevent external modification
	result := make([]types.Pipeline, len(r.pipelines))
	copy(result, r.pipelines)
	return result
}

// Start starts all pipelines in the route.
// Pipelines are started in order. If any pipeline fails to start,
// previously started pipelines are stopped and the error is returned.
func (r *RouteImpl) Start(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return errors.New("route already running")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Start pipelines in order
	startedPipelines := make([]types.Pipeline, 0, len(r.pipelines))

	for i, pipeline := range r.pipelines {
		if err := pipeline.Start(ctx); err != nil {
			// Stop previously started pipelines
			for _, started := range startedPipelines {
				_ = started.Close()
			}
			r.running.Store(false)
			return fmt.Errorf("failed to start pipeline %d (%s): %w", i, pipeline.GetID(), err)
		}
		startedPipelines = append(startedPipelines, pipeline)
	}

	return nil
}

// Close stops all pipelines in the route.
// Pipelines are stopped in reverse order to ensure clean shutdown.
func (r *RouteImpl) Close() error {
	if !r.running.CompareAndSwap(true, false) {
		return nil // Already stopped
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error

	// Stop pipelines in reverse order
	for i := len(r.pipelines) - 1; i >= 0; i-- {
		if err := r.pipelines[i].Close(); err != nil {
			errs = append(errs, fmt.Errorf("pipeline %d (%s): %w", i, r.pipelines[i].GetID(), err))
		}
	}

	return errors.Join(errs...)
}

// IsRunning returns true if the route is currently running.
func (r *RouteImpl) IsRunning() bool {
	return r.running.Load()
}

// AddPipeline adds a pipeline to the route.
// This should only be called before Start().
func (r *RouteImpl) AddPipeline(pipeline types.Pipeline) error {
	if r.running.Load() {
		return errors.New("cannot add pipeline to running route")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.pipelines = append(r.pipelines, pipeline)
	return nil
}

// Stats returns aggregated statistics for all pipelines in the route.
func (r *RouteImpl) Stats() types.PipelineStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var stats types.PipelineStats

	for _, pipeline := range r.pipelines {
		if sp, ok := pipeline.(types.StatsProvider); ok {
			ps := sp.Stats()
			stats.MessagesReceived += ps.MessagesReceived
			stats.MessagesSent += ps.MessagesSent
			stats.MessagesFailed += ps.MessagesFailed
			stats.MessagesRetried += ps.MessagesRetried
			stats.MessagesDropped += ps.MessagesDropped
			stats.InFlight += ps.InFlight
		}
	}

	return stats
}
