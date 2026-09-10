package transport

import (
	"fmt"
	"strings"
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

// defaultMaxDispatchDuration bounds the DETACHED ingress dispatch when the
// operator does not set MaxDispatchDuration. It is the backstop that
// cancels the context.WithoutCancel dispatch copy even when the fronting
// server installs no request-context deadline. See Config.MaxDispatchDuration.
const defaultMaxDispatchDuration = 5 * time.Minute

// defaultSSEClientBuffer is the fallback per-connection SSE event-queue
// depth when the operator does not set ClientBufferSize. See
// Config.ClientBufferSize.
const defaultSSEClientBuffer = 256

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
	// falls through to the 10000 default; there is no uncapped mode.
	MaxClients int `yaml:"max_clients,omitempty" json:"max_clients,omitempty" mapstructure:"max_clients"`

	// ClientBufferSize sizes each connected SSE client's per-connection
	// event queue. A broadcast that finds a client's queue full drops
	// the event for that client (counted on MetricSSEDroppedEvents)
	// instead of blocking the fan-out to healthy clients. Zero falls
	// through to the 256 default. Raising it trades memory for tolerance
	// of bursty producers / briefly slow consumers. There is
	// deliberately NO slow-consumer disconnect policy keyed on this
	// depth: a persistently slow client is evicted by the per-write
	// deadline (WriteTimeout), not by queue occupancy.
	ClientBufferSize int `yaml:"client_buffer_size,omitempty" json:"client_buffer_size,omitempty" mapstructure:"client_buffer_size"`

	// FailOnZeroDelivery is the LEGACY opt-in that made a zero-delivery
	// broadcast return a transient error. Zero delivery now FAILS BY
	// DEFAULT (see AtMostOnceAcceptLoss), so this flag is retained only for
	// backward compatibility: setting it true merely reaffirms the default
	// and has no additional effect. It is MUTUALLY EXCLUSIVE with
	// AtMostOnceAcceptLoss (one demands failure, the other demands
	// accepting loss) and that contradiction is rejected by Validate.
	FailOnZeroDelivery bool `yaml:"fail_on_zero_delivery,omitempty" json:"fail_on_zero_delivery,omitempty" mapstructure:"fail_on_zero_delivery"`

	// AtMostOnceAcceptLoss OPTS AN SSE SENDER OUT of the safe default and
	// into explicit at-most-once loss. By default (false) a broadcast that
	// reaches ZERO subscribers — none connected, or every subscriber's
	// buffer full — makes Send return a TRANSIENT (Unavailable-class)
	// error, so the route runner does NOT ack the source: a durable source
	// redelivers with backoff (giving a briefly-disconnected subscriber
	// time to reconnect) and dead-letters per policy, and an HTTP-ingress
	// source surfaces the failure to the POSTing producer as HTTP 500.
	// Setting this true restores the pre-hardening behaviour where Send
	// reports SUCCESS on zero delivery and the source is acked even though
	// the event reached nobody — accept ONLY for a best-effort live tap
	// whose loss is tolerable. Either way the zero-delivery outcome is
	// counted (MetricSSENoSubscribers / MetricSSEAllDropped) and logged at
	// ERROR level. Mutually exclusive with FailOnZeroDelivery.
	//
	// BREAKING CHANGE: earlier builds defaulted to at-most-once (Send
	// succeeded on zero delivery). Deployments that relied on that must set
	// at_most_once_accept_loss: true to keep it; otherwise SSE routes now
	// fail-and-retry instead of silently dropping events with no subscriber.
	AtMostOnceAcceptLoss bool `yaml:"at_most_once_accept_loss,omitempty" json:"at_most_once_accept_loss,omitempty" mapstructure:"at_most_once_accept_loss"`

	// RedirectEndpoint names the PeerInfo.Endpoints key an SSE sender
	// uses to build the 307 redirect target when the bound route is
	// owned by a remote cluster node. Empty (the default) DISABLES the
	// redirect: requests for a remote route are refused with 503 so the
	// internal peer endpoint (the one the cluster forwarder uses) is
	// never leaked to a possibly-external SSE client. Operators who
	// publish an externally reachable endpoint per node (e.g.
	// "http_public") opt in by naming that key here; setting it to
	// "http" restores the pre-hardening behaviour of redirecting to the
	// internal endpoint.
	RedirectEndpoint string `yaml:"redirect_endpoint,omitempty" json:"redirect_endpoint,omitempty" mapstructure:"redirect_endpoint"`

	// DedupWindow sizes the receiver's bounded in-memory idempotency
	// window: the number of most-recently processed Idempotency-Key /
	// X-Dedup-Id values remembered per receiver. A request presenting a
	// remembered key is acknowledged without re-emitting the delivery.
	// Zero falls through to the 4096 default. The window is node-local
	// and best-effort — see doc.go "Ingress idempotency window".
	DedupWindow int `yaml:"dedup_window,omitempty" json:"dedup_window,omitempty" mapstructure:"dedup_window"`

	// MaxDispatchDuration bounds the DETACHED ingress dispatch. Once a
	// request body is accepted the delivery is emitted on a
	// context.WithoutCancel copy of the request context so a client
	// disconnect cannot abort the pipeline mid-flight. This cap guarantees
	// that detached context is ALWAYS cancelled — the SSE/ingress handler
	// arms it unconditionally, so a wedged downstream cannot leak one
	// goroutine + in-memory delivery per stuck request even when the
	// fronting http.Server installs no request-context deadline (a bare
	// Read/WriteTimeout does not set one, so the previous conditional
	// re-arm was a no-op). Zero falls through to the 5m default. The
	// deadline is released early when the delivery settles (Ack/Retry).
	MaxDispatchDuration time.Duration `yaml:"max_dispatch_duration,omitempty" json:"max_dispatch_duration,omitempty" mapstructure:"max_dispatch_duration"`

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
	if c.Path != "" {
		if err := validateMountPath(c.Path); err != nil {
			return err
		}
	}
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
	if c.ClientBufferSize < 0 {
		return fmt.Errorf("http: client_buffer_size must be >= 0")
	}
	if c.MaxDispatchDuration < 0 {
		return fmt.Errorf("http: max_dispatch_duration must be >= 0")
	}
	if c.DedupWindow < 0 {
		return fmt.Errorf("http: dedup_window must be >= 0")
	}
	if key := c.APIKey.Reveal(); key != "" && len(key) < minAPIKeyLength {
		// Name the 16-char minimum as the cause. A short inline key that
		// an earlier build accepted now fails this floor (HTTP); an
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
	if c.FailOnZeroDelivery && c.AtMostOnceAcceptLoss {
		// One demands a transient error on zero delivery, the other demands
		// accepting the loss and acking. Refuse the ambiguous config at
		// build time rather than silently picking one.
		return fmt.Errorf("http: fail_on_zero_delivery and at_most_once_accept_loss are mutually exclusive")
	}
	return nil
}

// effectiveAcceptZeroDeliveryLoss reports whether an SSE broadcast that
// reached nobody must be treated as an at-most-once SUCCESS (true) rather
// than the safe default TRANSIENT error (false). Loss is accepted ONLY
// when the operator explicitly opts in via AtMostOnceAcceptLoss; the
// legacy FailOnZeroDelivery flag can only reaffirm the default and is
// rejected by Validate when it contradicts an accept-loss opt-in.
func (c Config) effectiveAcceptZeroDeliveryLoss() bool {
	return c.AtMostOnceAcceptLoss
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

// effectiveMaxDispatchDuration returns the detached-dispatch cap,
// defaulting to defaultMaxDispatchDuration. Mirrors effectiveWriteTimeout.
func (c Config) effectiveMaxDispatchDuration() time.Duration {
	if c.MaxDispatchDuration <= 0 {
		return defaultMaxDispatchDuration
	}
	return c.MaxDispatchDuration
}

// effectiveClientBufferSize returns the per-client SSE queue depth,
// defaulting to defaultSSEClientBuffer. Mirrors effectiveHeartbeat.
func (c Config) effectiveClientBufferSize() int {
	if c.ClientBufferSize <= 0 {
		return defaultSSEClientBuffer
	}
	return c.ClientBufferSize
}

func (c Config) effectiveMode() string {
	if c.Mode == "" {
		return "sse"
	}
	return c.Mode
}

// defaultDedupWindow is the fallback size of the receiver's bounded
// ingress idempotency window (number of remembered keys).
const defaultDedupWindow = 4096

func (c Config) effectiveDedupWindow() int {
	if c.DedupWindow <= 0 {
		return defaultDedupWindow
	}
	return c.DedupWindow
}

// validateMountPath rejects operator-supplied paths that would panic
// http.ServeMux registration at bridge build time (a mux pattern like
// "POST <path>" panics on malformed input — HTTP). A valid mount
// path starts with '/', carries no whitespace or control characters,
// and no ServeMux pattern metacharacters ('{', '}' wildcards): the
// configured path is a LITERAL mount point, never a pattern.
func validateMountPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("http: path %q must start with '/'", path)
	}
	for _, r := range path {
		switch {
		case r == '{' || r == '}':
			return fmt.Errorf(
				"http: path %q must not contain ServeMux pattern metacharacters '{' or '}'", path)
		case r == ' ' || r == '\t' || r < 0x20 || r == 0x7f:
			return fmt.Errorf("http: path %q must not contain whitespace or control characters", path)
		}
	}
	return nil
}
