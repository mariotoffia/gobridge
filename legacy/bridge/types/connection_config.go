package types

import (
	"strings"
	"time"
)

// ConnectionSettingsConfig contains transport-level settings (no topics).
// This is what gets updated when connection params change.
type ConnectionSettingsConfig interface {
	Config
	// GetBrokerURLs returns the broker/server URLs.
	// Multiple URLs support high-availability and failover scenarios.
	// For MQTT, the client will try URLs in order until one connects.
	// Changes to this list require a reconnect.
	GetBrokerURLs() []string
	// GetCredentials returns credentials for this connection.
	// The Credentials object may contain:
	//   - Inline credentials (actual username/password, certificates)
	//   - URI references (e.g., pms://tenantA/app1/prod/mqtt-creds) that need resolution
	//
	// When URIs are present (detected by IsServerURI), use credentials.Resolver to fetch actual values.
	// Returns nil if no credentials are needed.
	GetCredentials() *Credentials
	// GetKeepAlive returns keep-alive settings.
	GetKeepAlive() KeepAliveConfig
	// RequiresReconnect compares with another config and returns true
	// if the changes require a full reconnect (vs just a settings update).
	//
	// Changes that typically require reconnect:
	//   - BrokerURLs changed (added, removed, or reordered)
	//   - Credentials changed (username, password, or TLS certs)
	//   - TLS settings changed (e.g., InsecureSkipVerify)
	//   - Client ID changed (for MQTT)
	//
	// Changes that typically don't require reconnect:
	//   - Keep-alive interval/timeout adjustments (depends on transport)
	RequiresReconnect(other ConnectionSettingsConfig) bool
}

// KeepAliveConfig for connection keep-alive settings.
type KeepAliveConfig interface {
	// GetInterval returns the keep-alive interval.
	GetInterval() time.Duration
	// GetTimeout returns the keep-alive timeout.
	GetTimeout() time.Duration
}

// IsServerURI detects if a string is a serverURI that needs resolution.
// Returns true if string matches pattern: [a-z]+://.*
//
// ServerURI examples (needs resolution):
//   - pms://tenantA/app1/creds
//   - file://path/to/creds
//
// Inline examples (no resolution needed):
//   - "admin" (username)
//   - "-----BEGIN CERTIFICATE-----..." (certificate)
func IsServerURI(s string) bool {
	idx := strings.Index(s, "://")
	if idx <= 0 {
		return false
	}
	scheme := s[:idx]
	// Scheme must be single lowercase word: [a-z]+
	for _, c := range scheme {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// DefaultKeepAliveConfig provides a default implementation of KeepAliveConfig.
type DefaultKeepAliveConfig struct {
	Interval time.Duration `json:"interval,omitempty"`
	Timeout  time.Duration `json:"timeout,omitempty"`
}

// GetInterval returns the keep-alive interval.
func (c *DefaultKeepAliveConfig) GetInterval() time.Duration {
	return c.Interval
}

// GetTimeout returns the keep-alive timeout.
func (c *DefaultKeepAliveConfig) GetTimeout() time.Duration {
	return c.Timeout
}
