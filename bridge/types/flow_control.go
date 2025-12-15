package types

import "time"

// FlowControlConfig configures backpressure and message defaults at pipeline level.
//
// This is configured hierarchically: Bridge (default) -> Pipeline (override).
//
// Example:
//
//	bridge := core.NewBridge("my-bridge",
//	    core.WithFlowControl(types.FlowControlConfig{
//	        MaxInFlight:       200,
//	        DefaultMessageTTL: 5 * time.Minute,
//	    }),
//	)
type FlowControlConfig struct {
	// MaxInFlight limits concurrent messages being processed (default: 100).
	// When reached, source reading is paused (backpressure).
	// Set to 0 for unlimited (not recommended for production).
	MaxInFlight int `json:"maxInFlight,omitempty"`

	// DefaultMessageTTL applies to messages without explicit TTL (default: 120s).
	// This is the TTL used by Transport Retry to bound retry attempts.
	// Messages are dropped (not retried) once TTL expires.
	DefaultMessageTTL time.Duration `json:"defaultMessageTTL,omitempty"`
}

// DefaultFlowControlConfig returns sensible defaults.
func DefaultFlowControlConfig() FlowControlConfig {
	return FlowControlConfig{
		MaxInFlight:       100,
		DefaultMessageTTL: 2 * time.Minute, // 120 seconds
	}
}

// Merge returns a new config with non-zero values from override.
func (c FlowControlConfig) Merge(override FlowControlConfig) FlowControlConfig {
	result := c
	if override.MaxInFlight > 0 {
		result.MaxInFlight = override.MaxInFlight
	}
	if override.DefaultMessageTTL > 0 {
		result.DefaultMessageTTL = override.DefaultMessageTTL
	}
	return result
}

// IsZero returns true if the config has no values set.
func (c FlowControlConfig) IsZero() bool {
	return c.MaxInFlight == 0 && c.DefaultMessageTTL == 0
}

// WithDefaults returns the config with default values applied for zero fields.
func (c FlowControlConfig) WithDefaults() FlowControlConfig {
	defaults := DefaultFlowControlConfig()
	if c.MaxInFlight == 0 {
		c.MaxInFlight = defaults.MaxInFlight
	}
	if c.DefaultMessageTTL == 0 {
		c.DefaultMessageTTL = defaults.DefaultMessageTTL
	}
	return c
}
