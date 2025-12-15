package types

import (
	"context"
	"time"
)

// ============================================================================
// Draining
// ============================================================================

// Drainable is implemented by components that can be gracefully drained.
// Draining stops accepting new work and waits for in-flight work to complete.
type Drainable interface {
	// Drain initiates graceful draining of the component.
	// It blocks until draining is complete or the context is cancelled.
	// Options control the drain behavior (timeout, specific topics, etc.).
	Drain(ctx context.Context, opts DrainOptions) error

	// DrainProgress returns the current drain progress.
	// Returns nil if not currently draining.
	DrainProgress() *DrainProgress
}

// DrainOptions configures drain behavior.
type DrainOptions struct {
	// Timeout is the maximum time to wait for draining.
	// If zero, uses a default timeout.
	Timeout time.Duration `json:"timeout,omitempty"`
	// Topics limits draining to specific topics.
	// If nil or empty, drains all topics.
	Topics []string `json:"topics,omitempty"`
	// WaitForInFlight waits for in-flight messages to complete.
	// If false, in-flight messages may be abandoned.
	WaitForInFlight bool `json:"waitForInFlight,omitempty"`
	// ForceAfter is the duration after which to force-close connections
	// even if not all messages are processed.
	ForceAfter time.Duration `json:"forceAfter,omitempty"`
}

// DrainProgress reports the progress of a drain operation.
type DrainProgress struct {
	// StartedAt is when the drain started.
	StartedAt time.Time `json:"startedAt"`
	// Total is the total number of items to drain.
	Total int64 `json:"total"`
	// Drained is the number of items successfully drained.
	Drained int64 `json:"drained"`
	// InFlight is the number of items currently being processed.
	InFlight int64 `json:"inFlight"`
	// Failed is the number of items that failed to drain.
	Failed int64 `json:"failed"`
	// Complete indicates if draining is complete.
	Complete bool `json:"complete"`
	// Error contains any error that occurred during draining.
	Error error `json:"error,omitempty"`
}

// PercentComplete returns the percentage of draining complete.
func (d *DrainProgress) PercentComplete() float64 {
	if d.Total == 0 {
		return 100.0
	}
	return float64(d.Drained) / float64(d.Total) * 100.0
}

// Remaining returns the number of items remaining to drain.
func (d *DrainProgress) Remaining() int64 {
	return d.Total - d.Drained
}

// ============================================================================
// Health Checking
// ============================================================================

// HealthStatus represents the health status of a component.
type HealthStatus string

const (
	// HealthStatusHealthy indicates the component is functioning normally.
	HealthStatusHealthy HealthStatus = "healthy"
	// HealthStatusDegraded indicates the component is functioning but with issues.
	HealthStatusDegraded HealthStatus = "degraded"
	// HealthStatusUnhealthy indicates the component is not functioning.
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	// HealthStatusUnknown indicates the health status cannot be determined.
	HealthStatusUnknown HealthStatus = "unknown"
)

// HealthCheck represents the result of a health check.
type HealthCheck struct {
	// Status is the overall health status.
	Status HealthStatus `json:"status"`
	// Message provides additional context about the status.
	Message string `json:"message,omitempty"`
	// LastCheck is when this check was performed.
	LastCheck time.Time `json:"lastCheck"`
	// Duration is how long the check took.
	Duration time.Duration `json:"duration"`
	// Details contains component-specific health details.
	Details map[string]any `json:"details,omitempty"`
	// Children contains health checks for sub-components.
	Children map[string]*HealthCheck `json:"children,omitempty"`
}

// HealthChecker is implemented by components that can report their health.
type HealthChecker interface {
	// Health performs a health check and returns the result.
	Health(ctx context.Context) *HealthCheck
}

// ============================================================================
// Lifecycle Hooks
// ============================================================================

// LifecycleHook is a callback that runs at specific lifecycle events.
type LifecycleHook func(ctx context.Context) error

// LifecycleHooks contains hooks for various lifecycle events.
type LifecycleHooks struct {
	// OnStart is called when the component starts.
	OnStart []LifecycleHook
	// OnStop is called when the component stops.
	OnStop []LifecycleHook
	// OnDrainStart is called when draining begins.
	OnDrainStart []LifecycleHook
	// OnDrainComplete is called when draining completes.
	OnDrainComplete []LifecycleHook
	// OnError is called when an error occurs.
	OnError []func(ctx context.Context, err error)
}

// RunHooks executes all hooks in order, stopping on first error.
func RunHooks(ctx context.Context, hooks []LifecycleHook) error {
	for _, hook := range hooks {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ============================================================================
// Ready State
// ============================================================================

// ReadyChecker is implemented by components that have a ready state.
type ReadyChecker interface {
	// IsReady returns true if the component is ready to process work.
	IsReady() bool
	// WaitForReady blocks until the component is ready or context is cancelled.
	WaitForReady(ctx context.Context) error
}

// ============================================================================
// Lifecycle Coordination for Shared Connections
// ============================================================================

// LifecycleCoordinator manages atomic lifecycle changes for shared connections.
// This is needed for transports like MQTT where Source/Target share a client.
//
// Use BeginTransaction() to start an atomic change operation. All Source/Target
// changes within the transaction are applied atomically when Commit() is called.
type LifecycleCoordinator interface {
	// BeginTransaction starts an atomic change operation.
	// All Source/Target changes within the transaction are applied atomically.
	BeginTransaction(ctx context.Context) (LifecycleTransaction, error)
}

// LifecycleTransaction represents an atomic set of changes to Sources and Targets.
// Changes are staged and applied atomically when Commit() is called.
//
// For MQTT: unsubscribes removed topics and subscribes new topics in one operation.
type LifecycleTransaction interface {
	// AddSource schedules a source to be added.
	AddSource(config SourceConfig) error
	// RemoveSource schedules a source to be removed by ID.
	RemoveSource(sourceID string) error
	// UpdateSource schedules a source to be updated (remove + add).
	UpdateSource(sourceID string, config SourceConfig) error

	// AddTarget schedules a target to be added.
	AddTarget(config TargetConfig) error
	// RemoveTarget schedules a target to be removed by ID.
	RemoveTarget(targetID string) error
	// UpdateTarget schedules a target to be updated (remove + add).
	UpdateTarget(targetID string, config TargetConfig) error

	// Commit applies all scheduled changes atomically.
	// Returns the result of the operation, which includes created instances
	// and any errors that occurred.
	Commit(ctx context.Context) (*LifecycleChangeResult, error)

	// Rollback cancels the transaction without applying changes.
	Rollback() error
}

// LifecycleChangeResult contains the results of a committed transaction.
type LifecycleChangeResult struct {
	// AddedSources contains the Source instances that were created.
	AddedSources []Source
	// RemovedSources contains the IDs of Sources that were removed.
	RemovedSources []string
	// AddedTargets contains the Target instances that were created.
	AddedTargets []Target
	// RemovedTargets contains the IDs of Targets that were removed.
	RemovedTargets []string
	// Errors contains any non-fatal errors that occurred during the transaction.
	Errors []error
}
