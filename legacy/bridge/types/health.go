package types

import (
	"context"
	"time"
)

// ============================================================================
// Health Check Types
// ============================================================================

// HealthStatus indicates the health status of a component.
type HealthStatus string

const (
	// HealthStatusHealthy indicates the component is healthy.
	HealthStatusHealthy HealthStatus = "healthy"
	// HealthStatusDegraded indicates the component is partially healthy.
	HealthStatusDegraded HealthStatus = "degraded"
	// HealthStatusUnhealthy indicates the component is unhealthy.
	HealthStatusUnhealthy HealthStatus = "unhealthy"
)

// HealthCheck represents the health status of a component.
type HealthCheck struct {
	// Status is the overall health status.
	Status HealthStatus `json:"status"`
	// Message provides additional context about the status.
	Message string `json:"message,omitempty"`
	// Timestamp is when the health check was performed.
	Timestamp time.Time `json:"timestamp"`
	// Components contains health checks for sub-components.
	Components map[string]*HealthCheck `json:"components,omitempty"`
	// Details contains additional health details.
	Details map[string]any `json:"details,omitempty"`
}

// NewHealthCheck creates a new healthy HealthCheck.
func NewHealthCheck() *HealthCheck {
	return &HealthCheck{
		Status:     HealthStatusHealthy,
		Timestamp:  time.Now(),
		Components: make(map[string]*HealthCheck),
		Details:    make(map[string]any),
	}
}

// AddComponent adds a sub-component health check.
func (h *HealthCheck) AddComponent(name string, check *HealthCheck) {
	if h.Components == nil {
		h.Components = make(map[string]*HealthCheck)
	}
	h.Components[name] = check

	// Update overall status based on component status
	if check.Status == HealthStatusUnhealthy {
		h.Status = HealthStatusUnhealthy
	} else if check.Status == HealthStatusDegraded && h.Status == HealthStatusHealthy {
		h.Status = HealthStatusDegraded
	}
}

// SetUnhealthy marks the health check as unhealthy.
func (h *HealthCheck) SetUnhealthy(message string) {
	h.Status = HealthStatusUnhealthy
	h.Message = message
}

// SetDegraded marks the health check as degraded.
func (h *HealthCheck) SetDegraded(message string) {
	h.Status = HealthStatusDegraded
	h.Message = message
}

// ============================================================================
// Health Provider Interface
// ============================================================================

// HealthProvider is implemented by components that can report their health.
type HealthProvider interface {
	// Health returns the current health status.
	Health(ctx context.Context) *HealthCheck
}

// ReadinessProvider is implemented by components that can report readiness.
type ReadinessProvider interface {
	// IsReady returns true if the component is ready to serve traffic.
	IsReady() bool
	// WaitForReady blocks until the component is ready or context is cancelled.
	WaitForReady(ctx context.Context) error
}

// LivenessProvider is implemented by components that can report liveness.
type LivenessProvider interface {
	// IsLive returns true if the component is alive (not in a fatal state).
	IsLive() bool
}

// HealthChecker combines health, readiness, and liveness checks.
type HealthChecker interface {
	HealthProvider
	ReadinessProvider
	LivenessProvider
}
