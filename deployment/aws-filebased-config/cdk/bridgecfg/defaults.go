package bridgecfg

import "time"

// Default values shared by builder methods. Kept in one place so the
// canonical "what the builder produces when the operator stays out of
// its way" is visible at a glance. Operators that need to deviate
// pass options to the relevant With* method.
const (
	// DefaultAdminAddr is the listen address used by AdminAPIDefaults.
	// Matches the design contract (admin on :8080).
	DefaultAdminAddr = ":8080"

	// DefaultMonitorAddr is unset by default — the monitor server
	// is opt-in and operators must pick the addr explicitly when
	// they want it.
	DefaultMonitorAddr = ""

	// DefaultAdminAPIPathPrefix is the documented prefix for admin
	// endpoints; the bridge HTTP server mounts /healthz, /readyz
	// and /api/v1/* under this. Carried as a constant so docs and
	// tests share a single source of truth.
	DefaultAdminAPIPathPrefix = "/api/v1"

	// DefaultMQTTKeepAlive is the keep-alive (in seconds) the MQTT
	// builder method seeds when the operator omits a value. Aligned
	// with paho.DefaultSessionOptions to avoid surprise drift.
	DefaultMQTTKeepAlive uint16 = 30
)

// DefaultSQSAutoExtend is the canonical value the SQS builder uses
// for the auto-extend toggle when the operator omits one. SQS
// receivers default to true at the adapter layer; the builder mirrors
// that default explicitly so the produced bridge.yaml is
// self-describing rather than relying on an implicit fallback.
func DefaultSQSAutoExtend() *bool {
	v := true
	return &v
}

// DefaultMQTTConnectTimeout matches paho.DefaultSessionOptions.
const DefaultMQTTConnectTimeout = 30 * time.Second

// HTTPAdminAPIOptions configures the HTTP admin and monitor servers
// emitted onto BridgeConfig.HTTP. The split between admin and monitor
// addresses, and between API keys, follows the runtime contract
// (ports.HTTPConfig): the admin key is mandatory, the monitor key
// optional (admin key reused when empty).
//
// Either AdminAPIKey or AdminAPIKeyURI must be set when the resulting
// HTTP block is emitted; the secret scanner enforces that an inline
// AdminAPIKey is a credential URI and rejects literal values.
type HTTPAdminAPIOptions struct {
	// AdminAddr is the listen address for the admin server.
	AdminAddr string

	// MonitorAddr is the listen address for the monitor server.
	// Empty disables the monitor endpoint.
	MonitorAddr string

	// AdminAPIKey is written verbatim to BridgeConfig.HTTP.AdminAPIKey.
	// MUST be a credential URI (e.g. pms://<path>) — plaintext
	// values are rejected by the secret scanner.
	AdminAPIKey string

	// MonitorAPIKey is written verbatim to BridgeConfig.HTTP.MonitorAPIKey
	// when non-empty. Same credential-URI rule applies.
	MonitorAPIKey string

	// CORSOrigins is written verbatim to BridgeConfig.HTTP.CORSOrigins.
	// Empty leaves CORS disabled.
	CORSOrigins string
}

// AdminAPIDefaults returns an HTTPAdminAPIOptions seeded with the
// builder's recommended defaults: admin on :8080, monitor disabled,
// no API key, no CORS.
//
// The returned value is the canonical starting point for
// builder.WithHTTPAdminAPI(bridgecfg.AdminAPIDefaults().With...).
// Operators MUST set AdminAPIKey to a credential URI before Build —
// the runtime HTTP server refuses to start without an API key.
func AdminAPIDefaults() HTTPAdminAPIOptions {
	return HTTPAdminAPIOptions{
		AdminAddr:   DefaultAdminAddr,
		MonitorAddr: DefaultMonitorAddr,
	}
}
