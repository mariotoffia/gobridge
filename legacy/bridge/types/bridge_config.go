package types

import (
	"os"
	"strconv"
	"time"
)

// ============================================================================
// Bridge Configuration
// ============================================================================

// BridgeConfig holds configuration for creating a Bridge instance.
// It supports loading from environment variables.
type BridgeConfig struct {
	// ID is the unique identifier for this bridge instance.
	// Env: BRIDGE_ID (default: "bridge-1")
	ID string `json:"id"`

	// ClusterID is the cluster this bridge belongs to.
	// Env: CLUSTER_ID (default: "default")
	ClusterID string `json:"clusterId"`

	// ShutdownTimeout is how long to wait for graceful shutdown.
	// Env: SHUTDOWN_TIMEOUT (default: "30s")
	ShutdownTimeout time.Duration `json:"shutdownTimeout"`

	// DrainTimeout is how long to wait when draining pipelines.
	// Env: DRAIN_TIMEOUT (default: "30s")
	DrainTimeout time.Duration `json:"drainTimeout"`

	// TransportRetry is the default transport retry configuration.
	TransportRetry TransportRetryConfig `json:"transportRetry,omitempty"`

	// FlowControl is the default flow control configuration.
	FlowControl FlowControlConfig `json:"flowControl,omitempty"`

	// HealthCheckInterval is how often health checks run.
	// Env: HEALTH_CHECK_INTERVAL (default: "10s")
	HealthCheckInterval time.Duration `json:"healthCheckInterval"`

	// MetricsEnabled enables metrics collection.
	// Env: METRICS_ENABLED (default: "true")
	MetricsEnabled bool `json:"metricsEnabled"`

	// LogLevel is the logging level.
	// Env: LOG_LEVEL (default: "info")
	LogLevel string `json:"logLevel"`
}

// DefaultBridgeConfig returns a BridgeConfig with sensible defaults.
func DefaultBridgeConfig() BridgeConfig {
	return BridgeConfig{
		ID:                  "bridge-1",
		ClusterID:           "default",
		ShutdownTimeout:     30 * time.Second,
		DrainTimeout:        30 * time.Second,
		TransportRetry:      DefaultTransportRetryConfig(),
		FlowControl:         DefaultFlowControlConfig(),
		HealthCheckInterval: 10 * time.Second,
		MetricsEnabled:      true,
		LogLevel:            "info",
	}
}

// LoadBridgeConfigFromEnv loads configuration from environment variables.
// Missing variables use default values.
func LoadBridgeConfigFromEnv() BridgeConfig {
	config := DefaultBridgeConfig()

	if v := os.Getenv("BRIDGE_ID"); v != "" {
		config.ID = v
	}
	if v := os.Getenv("CLUSTER_ID"); v != "" {
		config.ClusterID = v
	}
	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.ShutdownTimeout = d
		}
	}
	if v := os.Getenv("DRAIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.DrainTimeout = d
		}
	}
	if v := os.Getenv("HEALTH_CHECK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.HealthCheckInterval = d
		}
	}
	if v := os.Getenv("METRICS_ENABLED"); v != "" {
		config.MetricsEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		config.LogLevel = v
	}

	// Transport retry from env
	if v := os.Getenv("TRANSPORT_RETRY_INITIAL_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.TransportRetry.InitialBackoff = d
		}
	}
	if v := os.Getenv("TRANSPORT_RETRY_MAX_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.TransportRetry.MaxBackoff = d
		}
	}

	// Flow control from env
	if v := os.Getenv("MAX_IN_FLIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			config.FlowControl.MaxInFlight = n
		}
	}
	if v := os.Getenv("DEFAULT_MESSAGE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.FlowControl.DefaultMessageTTL = d
		}
	}

	return config
}

// Validate validates the configuration.
func (c *BridgeConfig) Validate() error {
	var errs []error

	if c.ID == "" {
		errs = append(errs, &ValidationError{Field: "ID", Message: "bridge ID is required"})
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, &ValidationError{Field: "ShutdownTimeout", Message: "shutdown timeout must be positive"})
	}
	if c.DrainTimeout <= 0 {
		errs = append(errs, &ValidationError{Field: "DrainTimeout", Message: "drain timeout must be positive"})
	}
	if c.FlowControl.MaxInFlight < 0 {
		errs = append(errs, &ValidationError{Field: "FlowControl.MaxInFlight", Message: "max in flight cannot be negative"})
	}

	if len(errs) > 0 {
		return &ConfigValidationError{Errors: errs}
	}
	return nil
}

// ============================================================================
// Validation Errors
// ============================================================================

// ValidationError represents a single validation error.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

// ConfigValidationError contains multiple validation errors.
type ConfigValidationError struct {
	Errors []error `json:"errors"`
}

func (e *ConfigValidationError) Error() string {
	if len(e.Errors) == 0 {
		return "no validation errors"
	}
	if len(e.Errors) == 1 {
		return e.Errors[0].Error()
	}
	msg := "multiple validation errors: "
	for i, err := range e.Errors {
		if i > 0 {
			msg += "; "
		}
		msg += err.Error()
	}
	return msg
}

func (e *ConfigValidationError) Unwrap() []error {
	return e.Errors
}
