package transport

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Kind is the registry discriminator for the HTTP transport.
const Kind = "http"

// minAPIKeyLength is the enforced floor for an inline api_key. The
// documented examples (scenario 15, transport-configuration.md) already
// use >=16-character keys; this turns that convention into a decode-time
// guard so a too-short key cannot silently weaken endpoint protection.
// The check applies only to inline keys: credential-resolved material is
// validated at the credential layer, not re-validated post-resolution.
const minAPIKeyLength = 16

// defaultSSEWriteTimeout bounds each individual SSE frame write when the
// operator does not set WriteTimeout. See Config.WriteTimeout.
const defaultSSEWriteTimeout = 15 * time.Second

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
	APIKey shared.Secret `yaml:"api_key,omitempty" json:"api_key,omitempty" mapstructure:"api_key"`
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

	// WriteTimeout bounds each individual SSE frame write. The SSE
	// handler re-arms this per-write deadline before every frame via
	// http.ResponseController, which (a) overrides a fronting server's
	// global WriteTimeout so a healthy long-lived stream is not killed
	// and (b) evicts a stalled client instead of pinning a goroutine on
	// a blocked write. Zero falls through to the 15s default.
	WriteTimeout time.Duration `yaml:"write_timeout,omitempty" json:"write_timeout,omitempty" mapstructure:"write_timeout"`

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
	if c.WriteTimeout < 0 {
		return fmt.Errorf("http: write_timeout must be >= 0")
	}
	if c.MaxClients < 0 {
		return fmt.Errorf("http: max_clients must be >= 0")
	}
	if key := c.APIKey.Reveal(); key != "" && len(key) < minAPIKeyLength {
		// Name the 16-char minimum as the cause. A short inline key that
		// an earlier build accepted now fails this floor (HTTP-N3); an
		// error that did not spell out the minimum left operators
		// guessing why. Reporting the too-short length of an
		// already-rejected, unusable key is a config-time aid, not a
		// secret leak.
		return fmt.Errorf(
			"http: inline api_key is too short: %d characters, minimum is %d",
			len(key), minAPIKeyLength)
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

func (c Config) effectiveWriteTimeout() time.Duration {
	if c.WriteTimeout <= 0 {
		return defaultSSEWriteTimeout
	}
	return c.WriteTimeout
}

func (c Config) effectiveMode() string {
	if c.Mode == "" {
		return "sse"
	}
	return c.Mode
}
