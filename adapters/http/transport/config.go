package transport

import (
	"fmt"
	"time"
)

// Kind is the registry discriminator for the HTTP transport.
const Kind = "http"

// Config is the typed plugin config for the HTTP transport. It covers
// both the receiver (POST endpoint) and the SSE sender. All fields are
// optional except Mode, which the sender rejects when it is anything
// other than "sse".
type Config struct {
	// Path overrides the auto-generated mount path. Empty means
	// "<path_prefix>/receivers/<id>/messages" or
	// "<path_prefix>/senders/<id>/events" depending on role.
	Path string `yaml:"path,omitempty" json:"path,omitempty" mapstructure:"path"`
	// APIKey, when non-empty, requires inbound requests to present
	// the matching key in the Authorization or X-API-Key header.
	APIKey string `yaml:"api_key,omitempty" json:"api_key,omitempty" mapstructure:"api_key"`
	// MaxBodySize is the receiver's request-body cap in bytes.
	// Zero falls through to the 1 MiB default.
	MaxBodySize int64 `yaml:"max_body_size,omitempty" json:"max_body_size,omitempty" mapstructure:"max_body_size"`

	// Mode selects the sender mode. Only "sse" is supported. Empty
	// defaults to "sse".
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty" mapstructure:"mode"`
	// HeartbeatInterval controls the SSE keepalive cadence. Zero
	// falls through to 30s.
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval,omitempty" json:"heartbeat_interval,omitempty" mapstructure:"heartbeat_interval"`
	// MaxClients caps concurrent SSE connections per sender. Zero
	// means no cap.
	MaxClients int `yaml:"max_clients,omitempty" json:"max_clients,omitempty" mapstructure:"max_clients"`

	// CredentialsURIRef is the optional URI consulted by the bridge's
	// credential store at build time. Resolved material populates
	// APIKey via ApplyCredentials when APIKey is empty.
	CredentialsURIRef string `yaml:"credentials_uri,omitempty" json:"credentials_uri,omitempty" mapstructure:"credentials_uri"`
}

// Kind returns the registry discriminator.
func (Config) Kind() string { return Kind }

// Validate rejects clearly invalid values.
func (c Config) Validate() error {
	if c.MaxBodySize < 0 {
		return fmt.Errorf("http: max_body_size must be >= 0")
	}
	if c.HeartbeatInterval < 0 {
		return fmt.Errorf("http: heartbeat_interval must be >= 0")
	}
	if c.MaxClients < 0 {
		return fmt.Errorf("http: max_clients must be >= 0")
	}
	if c.Mode != "" && c.Mode != "sse" {
		return fmt.Errorf("http: unsupported sender mode %q (only \"sse\" supported)", c.Mode)
	}
	return nil
}

func (c Config) effectiveMaxBody() int64 {
	if c.MaxBodySize <= 0 {
		return 1 << 20
	}
	return c.MaxBodySize
}

func (c Config) effectiveHeartbeat() time.Duration {
	if c.HeartbeatInterval <= 0 {
		return 30 * time.Second
	}
	return c.HeartbeatInterval
}

func (c Config) effectiveMode() string {
	if c.Mode == "" {
		return "sse"
	}
	return c.Mode
}
