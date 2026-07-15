// Package ports — bridge blueprint.
//
// BridgeConfig and its sub-types describe the parsed input that a
// gobridge runtime is built from. They live in `ports/` (not in the
// config parser package) so the bridge composition layer and the
// admin HTTP layer can consume them without depending on any
// particular configuration format. The yaml/json struct tags are
// kept on the types because adapters that read from disk parse them
// in-place — but the ports package itself has no yaml/json runtime
// dependency, so the inner ring stays dependency-neutral, not
// tag-free (schema-tagged DTOs by design, M-11).
//
// The config sub-package (in `config/`) implements the YAML/JSON
// parser, validator, merger, and on-disk manager that produce
// *ports.BridgeConfig values.
package ports

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// BridgeConfig is the root configuration for a GoBridge instance.
type BridgeConfig struct {
	// Version is an optimistic-concurrency counter incremented on each
	// config commit. When multiple instances share a config file (e.g.
	// on AWS EFS), the config transaction API uses this field for
	// check-and-set: a commit succeeds only when the on-disk version
	// matches the version the transaction was started against.
	// A zero value means the config has never been committed via the API.
	Version int `yaml:"version,omitempty" json:"version,omitempty"`

	Bridge      BridgeSettings  `yaml:"bridge" json:"bridge"`
	ConfigWatch *ConfigWatchDef `yaml:"config_watch,omitempty" json:"config_watch,omitempty"`
	Stores      StoresConfig    `yaml:"stores,omitempty" json:"stores,omitempty"`
	Sessions    []SessionDef    `yaml:"sessions,omitempty" json:"sessions,omitempty"`
	Receivers   []ReceiverDef   `yaml:"receivers,omitempty" json:"receivers,omitempty"`
	Senders     []SenderDef     `yaml:"senders,omitempty" json:"senders,omitempty"`
	Bindings    []BindingDef    `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	Routes      []RouteDef      `yaml:"routes,omitempty" json:"routes,omitempty"`
	HTTP        *HTTPConfig     `yaml:"http,omitempty" json:"http,omitempty"`
}

// ConfigWatchDef configures how the configuration source is watched
// for changes. Mode selects the detection mechanism:
//   - "notify" (default): filesystem event notifications, debounced by Debounce
//   - "poll": periodic file reads with content comparison, at PollInterval
type ConfigWatchDef struct {
	Mode         string `yaml:"mode,omitempty" json:"mode,omitempty"`
	PollInterval string `yaml:"poll_interval,omitempty" json:"poll_interval,omitempty"`
	Debounce     string `yaml:"debounce,omitempty" json:"debounce,omitempty"`
}

// BridgeSettings holds bridge-level operational settings.
type BridgeSettings struct {
	ID              string `yaml:"id" json:"id"`
	InstanceID      string `yaml:"instance_id,omitempty" json:"instance_id,omitempty"`
	DeploymentMode  string `yaml:"deployment_mode,omitempty" json:"deployment_mode,omitempty"`
	ShutdownTimeout string `yaml:"shutdown_timeout,omitempty" json:"shutdown_timeout,omitempty"`
	// DrainTimeout is the legacy fixed ceiling applied to a whole drain
	// batch. Retained for backward compatibility; prefer
	// PerRecordDrainTimeout + MaxDrainTimeout for production workloads.
	DrainTimeout string `yaml:"drain_timeout,omitempty" json:"drain_timeout,omitempty"`
	// PerRecordDrainTimeout is the per-record allowance used by the
	// scaled formula: ceiling = min(batchCount * PerRecordDrainTimeout,
	// MaxDrainTimeout). When non-zero together with MaxDrainTimeout this
	// supersedes DrainTimeout.
	PerRecordDrainTimeout string `yaml:"per_record_drain_timeout,omitempty" json:"per_record_drain_timeout,omitempty"`
	// MaxDrainTimeout is the upper bound for the scaled drain formula.
	MaxDrainTimeout string         `yaml:"max_drain_timeout,omitempty" json:"max_drain_timeout,omitempty"`
	LogLevel        string         `yaml:"log_level,omitempty" json:"log_level,omitempty"`
	Cluster         *ClusterConfig `yaml:"cluster,omitempty" json:"cluster,omitempty"`
}

// ClusterConfig configures cluster endpoint advertisement for this instance.
//
// Endpoints are normally auto-discovered via EndpointResolver at startup
// (e.g. the ECS resolver derives them from task metadata) and written into the
// lease row on acquire/renew. The Endpoints field is an optional STATIC OVERRIDE
// for special cases.
//
// IMPORTANT — shape: Endpoints is THIS instance's advertised CAPABILITY
// endpoints, keyed by CAPABILITY name (currently "http"), with a full URL value
// (scheme://host:port). It is NOT a peer/instance membership map. The HTTP
// forwarder locates the owning instance's endpoint via Endpoints["http"] to
// forward a remote exclusive request, so a static override MUST look like:
//
//	endpoints:
//	  http: "http://10.0.1.10:8080"   # this instance's reachable transport URL
//
// A peer-membership map (instance-01: "10.0.1.10:8080", ...) has no "http" key
// and would make every remote exclusive HTTP forward fail with "target has no
// HTTP endpoint" (502). The config validator rejects that shape in clustered
// mode (config/validate.go: validateClusterEndpoints).
type ClusterConfig struct {
	Endpoints map[string]string `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
}

// ShutdownTimeoutDuration parses the shutdown timeout string, falling back to
// 30s when the string is empty OR malformed. The duration strings on
// BridgeSettings are validated (malformed or non-positive values rejected) by
// the config package's blueprint validator on the load path; these accessors
// stay deliberately forgiving, so a consumer that builds BridgeSettings by
// hand and skips that validator inherits the 30s fallback by design.
func (b BridgeSettings) ShutdownTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(b.ShutdownTimeout)
	if d == 0 {
		return 30 * time.Second
	}
	return d
}

// DrainTimeoutDuration parses the drain timeout string.
func (b BridgeSettings) DrainTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(b.DrainTimeout)
	if d == 0 {
		return 30 * time.Second
	}
	return d
}

// PerRecordDrainTimeoutDuration parses the per-record drain timeout.
// Returns 0 when unset so the caller can fall back to DrainTimeout.
func (b BridgeSettings) PerRecordDrainTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(b.PerRecordDrainTimeout)
	return d
}

// MaxDrainTimeoutDuration parses the max drain timeout.
// Returns 0 when unset so the caller can fall back to DrainTimeout.
func (b BridgeSettings) MaxDrainTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(b.MaxDrainTimeout)
	return d
}

// StoresConfig configures the backing stores for lease, outbox, and DLQ.
type StoresConfig struct {
	Lease                *StoreConfig `yaml:"lease,omitempty" json:"lease,omitempty"`
	Outbox               *StoreConfig `yaml:"outbox,omitempty" json:"outbox,omitempty"`
	DLQ                  *StoreConfig `yaml:"dlq,omitempty" json:"dlq,omitempty"`
	ManagedSubscriptions *StoreConfig `yaml:"managed_subscriptions,omitempty" json:"managed_subscriptions,omitempty"`
}

// StoreConfig describes a single store backend.
//
// The discriminator is Type. After the two-stage parser in `config/`
// has resolved the registered decoder, the typed plugin config is
// stored in Config and the originating raw payload is retained in the
// unexported `raw` field for diagnostics and round-trip.
type StoreConfig struct {
	Type   string       `yaml:"type" json:"type"`
	Config PluginConfig `yaml:"-" json:"-"`
	raw    RawConfig
}

// Raw returns the stage-1 raw options payload that produced Config.
// It is nil when no stage-2 decode has run (e.g. for hand-built
// configs in tests). Intended for diagnostics and round-trip; not
// part of the typed contract callers should depend on.
func (s *StoreConfig) Raw() RawConfig { return s.raw }

// SetDecoded attaches the stage-2 decoded PluginConfig and the
// originating RawConfig in a single shot. It is the only way the
// `config/` parser (or hand-written tests) can populate the
// unexported raw field across the package boundary.
func (s *StoreConfig) SetDecoded(cfg PluginConfig, raw RawConfig) {
	s.Config = cfg
	s.raw = raw
}

// SessionDef describes a transport session (e.g. an MQTT connection).
type SessionDef struct {
	ID          string       `yaml:"id" json:"id"`
	Transport   string       `yaml:"transport" json:"transport"`
	SessionMode string       `yaml:"session_mode,omitempty" json:"session_mode,omitempty"`
	Config      PluginConfig `yaml:"-" json:"-"`
	raw         RawConfig
}

// Raw returns the stage-1 raw options payload. See StoreConfig.Raw.
func (s *SessionDef) Raw() RawConfig { return s.raw }

// SetDecoded attaches the stage-2 decoded PluginConfig and raw payload.
func (s *SessionDef) SetDecoded(cfg PluginConfig, raw RawConfig) {
	s.Config = cfg
	s.raw = raw
}

// ReceiverDef describes a message ingress endpoint.
type ReceiverDef struct {
	ID        string            `yaml:"id" json:"id"`
	Transport string            `yaml:"transport" json:"transport"`
	SessionID string            `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Topics    []SubscriptionDef `yaml:"topics,omitempty" json:"topics,omitempty"`
	Config    PluginConfig      `yaml:"-" json:"-"`
	raw       RawConfig
}

// Raw returns the stage-1 raw options payload. See StoreConfig.Raw.
func (r *ReceiverDef) Raw() RawConfig { return r.raw }

// SetDecoded attaches the stage-2 decoded PluginConfig and raw payload.
func (r *ReceiverDef) SetDecoded(cfg PluginConfig, raw RawConfig) {
	r.Config = cfg
	r.raw = raw
}

// SubscriptionDef describes a topic subscription for a receiver.
//
// SubscriptionDef has no own discriminator: its plugin kind is
// inherited from the parent ReceiverDef.Transport at parse time.
type SubscriptionDef struct {
	Topic  string       `yaml:"topic" json:"topic"`
	QoS    int          `yaml:"qos,omitempty" json:"qos,omitempty"`
	Config PluginConfig `yaml:"-" json:"-"`
	raw    RawConfig
}

// Raw returns the stage-1 raw options payload. See StoreConfig.Raw.
func (s *SubscriptionDef) Raw() RawConfig { return s.raw }

// SetDecoded attaches the stage-2 decoded PluginConfig and raw payload.
func (s *SubscriptionDef) SetDecoded(cfg PluginConfig, raw RawConfig) {
	s.Config = cfg
	s.raw = raw
}

// SenderDef describes a message egress endpoint.
type SenderDef struct {
	ID        string       `yaml:"id" json:"id"`
	Transport string       `yaml:"transport" json:"transport"`
	SessionID string       `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Config    PluginConfig `yaml:"-" json:"-"`
	raw       RawConfig
}

// Raw returns the stage-1 raw options payload. See StoreConfig.Raw.
func (s *SenderDef) Raw() RawConfig { return s.raw }

// SetDecoded attaches the stage-2 decoded PluginConfig and raw payload.
func (s *SenderDef) SetDecoded(cfg PluginConfig, raw RawConfig) {
	s.Config = cfg
	s.raw = raw
}

// BindingDef describes a concrete destination that a route may send to.
//
// BindingDef has no own discriminator: its plugin kind is inherited
// from the referenced SenderDef.Transport at parse time. If the
// sender does not exist the parser surfaces an error rather than
// silently skipping the binding.
type BindingDef struct {
	ID        string       `yaml:"id" json:"id"`
	SenderID  string       `yaml:"sender_id" json:"sender_id"`
	SessionID string       `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Address   string       `yaml:"address" json:"address"`
	Config    PluginConfig `yaml:"-" json:"-"`
	raw       RawConfig
}

// Raw returns the stage-1 raw options payload. See StoreConfig.Raw.
func (b *BindingDef) Raw() RawConfig { return b.raw }

// SetDecoded attaches the stage-2 decoded PluginConfig and raw payload.
func (b *BindingDef) SetDecoded(cfg PluginConfig, raw RawConfig) {
	b.Config = cfg
	b.raw = raw
}

// RouteDef describes a message route from a receiver through an optional
// processor chain to one or more sender bindings.
type RouteDef struct {
	ID           string `yaml:"id" json:"id"`
	ReceiverID   string `yaml:"receiver_id" json:"receiver_id"`
	DeliveryMode string `yaml:"delivery_mode,omitempty" json:"delivery_mode,omitempty"`
	DispatchMode string `yaml:"dispatch_mode,omitempty" json:"dispatch_mode,omitempty"`
	// TrustBridgeHeaders preserves the BRIDGE-TO-BRIDGE PROPAGATED reserved
	// headers (correlation-id, causation-id, idempotency-key, dedup-id,
	// ordering-key, tenant-id, forwarded-from/hop) on inbound deliveries
	// instead of stripping every x-bridge.* header at ingress. Enable ONLY on
	// receivers fed exclusively by a trusted upstream bridge — otherwise an
	// external producer could spoof bridge metadata. INTERNAL-ONLY headers
	// (route-id, route-override, source-id, content-type) are stripped
	// regardless, so routing can never be steered by an inbound header. (W3C
	// trace context — traceparent/tracestate — is not x-bridge.*-prefixed,
	// is never stripped, and survives in BOTH modes.)
	TrustBridgeHeaders bool             `yaml:"trust_bridge_headers,omitempty" json:"trust_bridge_headers,omitempty"`
	Policy             PolicyDef        `yaml:"policy,omitempty" json:"policy,omitempty"`
	Bindings           []string         `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	Processors         []string         `yaml:"processors,omitempty" json:"processors,omitempty"`
	Resolver           *ResolverDef     `yaml:"resolver,omitempty" json:"resolver,omitempty"`
	Session            *RouteSessionDef `yaml:"session,omitempty" json:"session,omitempty"`
}

// ResolverDef configures content-based binding resolution for a route.
// Supported types: "rules" (ordered rule evaluation), "header_map"
// (header-value to binding-ID mapping), "all" (fan-out to all bindings),
// "static" (first binding only).
type ResolverDef struct {
	Type           string            `yaml:"type" json:"type"`
	DefaultBinding string            `yaml:"default_binding,omitempty" json:"default_binding,omitempty"`
	HeaderKey      string            `yaml:"header_key,omitempty" json:"header_key,omitempty"`
	HeaderMap      map[string]string `yaml:"header_map,omitempty" json:"header_map,omitempty"`
	Rules          []RuleDef         `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// RuleDef defines a single content-based routing rule: a binding ID and
// the conditions that must all match (AND logic) for the rule to select.
type RuleDef struct {
	BindingID string         `yaml:"binding_id" json:"binding_id"`
	Match     []ConditionDef `yaml:"match,omitempty" json:"match,omitempty"`
}

// ConditionDef defines a field-level predicate for routing decisions.
// Field patterns: "subject", "header.<key>", "$.<json.path>", or bare name
// (header fallback). Operators: eq, ne, prefix, contains, regex, gt, lt,
// gte, lte, exists, in.
type ConditionDef struct {
	Field    string `yaml:"field" json:"field"`
	Operator string `yaml:"operator" json:"operator"`
	Value    any    `yaml:"value" json:"value"`
}

// RouteSessionDef configures session management for a route that targets
// an exclusive MQTT session or similar stateful transport.
type RouteSessionDef struct {
	SessionID string `yaml:"session_id" json:"session_id"`
	SenderID  string `yaml:"sender_id" json:"sender_id"`

	// LeaseTTL is how long the lease is valid (duration string, e.g. "360s").
	// Empty means use the runtime default.
	LeaseTTL string `yaml:"lease_ttl,omitempty" json:"lease_ttl,omitempty"`

	// RenewInterval is how often the lease is renewed (e.g. "120s").
	// Empty means derive as LeaseTTL / MaxRenewFails.
	RenewInterval string `yaml:"renew_interval,omitempty" json:"renew_interval,omitempty"`

	// RenewJitter adds bounded random jitter to each renewal timer to avoid a
	// thundering herd of renewals across a cluster (e.g. "2s"). Empty means
	// the session manager derives it from RenewInterval. When BOTH
	// renew_interval and lease_ttl are set explicitly, cross-field validation
	// requires renewInterval*maxRenewFails + jitter < leaseTTL so a renewal
	// storm can never outlast the lease (contract C3).
	RenewJitter string `yaml:"lease_renew_jitter,omitempty" json:"lease_renew_jitter,omitempty"`

	// MaxRenewFails is consecutive renewal failures before step-down.
	// Zero means use the runtime default (3).
	MaxRenewFails int `yaml:"max_renew_fails,omitempty" json:"max_renew_fails,omitempty"`

	// StepDownGrace is how long to wait for in-flight operations before
	// releasing the lease (e.g. "15s"). Empty means use the runtime default.
	StepDownGrace string `yaml:"step_down_grace,omitempty" json:"step_down_grace,omitempty"`

	// AcquirePollInterval is how often a standby retries acquiring the lease
	// while another instance owns it (e.g. "5s"). Empty means use the runtime
	// default.
	AcquirePollInterval string `yaml:"acquire_poll_interval,omitempty" json:"acquire_poll_interval,omitempty"`

	// RenewCallTimeout bounds a single lease-renew store call (e.g. "3s"). It is
	// part of the failover-safety invariant: because renewLoop resets its timer
	// AFTER the renew call returns, real attempt spacing is
	// renewInterval + jitter/2 + renewCallTimeout, so the worst-case detection
	// span folds this in and must stay below lease_ttl (finding C3-HIGH). Empty
	// means the session manager derives it from RenewInterval.
	RenewCallTimeout string `yaml:"renew_call_timeout,omitempty" json:"renew_call_timeout,omitempty"`

	// FailoverSLO is an optional failure-detection to ServiceLevelFull objective.
	// It is a duration string; empty means no SLO is declared.
	FailoverSLO string `yaml:"failover_slo,omitempty" json:"failover_slo,omitempty"`

	// StartupAllowance is explicit bounded time reserved for process-side work
	// outside lease, broker-connect, and reconcile calls. Empty means zero.
	StartupAllowance string `yaml:"startup_allowance,omitempty" json:"startup_allowance,omitempty"`

	DrainInterval       string            `yaml:"drain_interval,omitempty" json:"drain_interval,omitempty"`
	DrainBatchSize      int               `yaml:"drain_batch_size,omitempty" json:"drain_batch_size,omitempty"`
	DrainMaxBatchSize   int               `yaml:"drain_max_batch_size,omitempty" json:"drain_max_batch_size,omitempty"`
	DrainMaxConcurrency int               `yaml:"drain_max_concurrency,omitempty" json:"drain_max_concurrency,omitempty"`
	DrainStrategy       *DrainStrategyDef `yaml:"drain_strategy,omitempty" json:"drain_strategy,omitempty"`
	// ConnectAfterLease defers the source session's broker connect until this
	// instance wins the lease. For an exclusive single-use transport (MQTT/AMQP)
	// this stops a booting standby from resuming a broker-persisted subscription
	// (clean_start=false) and consuming WITHOUT the lease until reconcile
	// converges. A RouteSessionDef source is always exclusive, so the safe
	// default is ON: nil (omitted) resolves to true; set it explicitly to false
	// only to opt out (finding F6). It is a pointer so an omitted flag is
	// distinguishable from an explicit false.
	ConnectAfterLease *bool `yaml:"connect_after_lease,omitempty" json:"connect_after_lease,omitempty"`
}

// DrainStrategyDef configures the outbox drain polling strategy.
// Type must be "fixed_poll" or "adaptive_backoff".
type DrainStrategyDef struct {
	Type        string  `yaml:"type" json:"type"`
	Interval    string  `yaml:"interval,omitempty" json:"interval,omitempty"`
	MinInterval string  `yaml:"min_interval,omitempty" json:"min_interval,omitempty"`
	MaxInterval string  `yaml:"max_interval,omitempty" json:"max_interval,omitempty"`
	Multiplier  float64 `yaml:"multiplier,omitempty" json:"multiplier,omitempty"`
}

// PolicyDef defines per-route delivery, retry, and backpressure
// configuration as YAML-friendly strings.
type PolicyDef struct {
	MaxInFlight        int    `yaml:"max_in_flight,omitempty" json:"max_in_flight,omitempty"`
	AckAfter           string `yaml:"ack_after,omitempty" json:"ack_after,omitempty"`
	MaxReplayAttempts  int    `yaml:"max_replay_attempts,omitempty" json:"max_replay_attempts,omitempty"`
	ReplayBudget       string `yaml:"replay_budget,omitempty" json:"replay_budget,omitempty"`
	MaxOutboxDepth     int    `yaml:"max_outbox_depth,omitempty" json:"max_outbox_depth,omitempty"`
	OnExpired          string `yaml:"on_expired,omitempty" json:"on_expired,omitempty"`
	OnPermanentFailure string `yaml:"on_permanent_failure,omitempty" json:"on_permanent_failure,omitempty"`
	// OnFiltered governs a message a processor intentionally drops
	// (shared.ErrMessageFiltered). Values: "drop" (default) or "dlq". It is
	// separate from on_permanent_failure so a high-volume filter drop does not
	// inherit the permanent-failure DLQ default.
	OnFiltered     string     `yaml:"on_filtered,omitempty" json:"on_filtered,omitempty"`
	Backoff        BackoffDef `yaml:"backoff,omitempty" json:"backoff,omitempty"`
	SendTimeout    string     `yaml:"send_timeout,omitempty" json:"send_timeout,omitempty"`
	DepthCacheTTL  string     `yaml:"depth_cache_ttl,omitempty" json:"depth_cache_ttl,omitempty"`
	AllowUnfenced  bool       `yaml:"allow_unfenced,omitempty" json:"allow_unfenced,omitempty"`
	AllowRetryDrop bool       `yaml:"allow_retry_drop,omitempty" json:"allow_retry_drop,omitempty"`
}

// BackoffDef defines retry backoff as YAML-friendly strings.
type BackoffDef struct {
	InitialInterval string  `yaml:"initial_interval,omitempty" json:"initial_interval,omitempty"`
	MaxInterval     string  `yaml:"max_interval,omitempty" json:"max_interval,omitempty"`
	Multiplier      float64 `yaml:"multiplier,omitempty" json:"multiplier,omitempty"`
	// Jitter is the equal-jitter fraction in [0,1] applied to each
	// computed backoff delay (0 = deterministic). Maps to
	// routing.BackoffPolicy.JitterFactor.
	Jitter float64 `yaml:"jitter,omitempty" json:"jitter,omitempty"`
}

// BlueprintValidator is the contract a config validator implements so
// the bridge composition layer can verify a *BridgeConfig without
// importing the parser/validator package directly. config.Validate
// satisfies this signature; the composition root supplies it (or any
// other implementation) to bridge.WithBlueprintValidator.
type BlueprintValidator func(*BridgeConfig) error

// HTTPConfig configures the optional HTTP admin and monitor servers.
// AdminAPIKey is mandatory when the HTTP block is present; the server
// refuses to start without it (checked via IsZero). MonitorAPIKey is
// optional; when empty the admin key is used for authenticated monitor
// endpoints. Both keys are shared.Secret value objects: they decode
// from a plain YAML/JSON scalar via UnmarshalText and REDACT on every
// generic marshal surface ("[REDACTED]"); the authoritative config
// serializer reveals them explicitly at the save boundary (so config-as-
// code round-trips), and the admin config-read endpoint also redacts.
// CORS is disabled by default and wildcard '*' is rejected.
//
// TLS is opt-in: when both TLSCertFile and TLSKeyFile are set the admin and
// monitor servers serve HTTPS using that certificate pair; when either is
// empty the servers stay plaintext (the historical default) on the assumption
// an external terminator (LB/ingress/mesh) provides TLS. Supplying only one of
// the pair is a configuration error the server rejects at startup.
type HTTPConfig struct {
	AdminAddr     string        `yaml:"admin_addr,omitempty" json:"admin_addr,omitempty"`
	MonitorAddr   string        `yaml:"monitor_addr,omitempty" json:"monitor_addr,omitempty"`
	AdminAPIKey   shared.Secret `yaml:"admin_api_key,omitempty" json:"admin_api_key,omitempty"`
	MonitorAPIKey shared.Secret `yaml:"monitor_api_key,omitempty" json:"monitor_api_key,omitempty"`
	CORSOrigins   string        `yaml:"cors_origins,omitempty" json:"cors_origins,omitempty"`

	// TLSCertFile and TLSKeyFile are filesystem paths to the PEM-encoded
	// server certificate (with any intermediate chain) and its private key.
	// Both must be set together to enable in-process TLS termination on both
	// the admin and monitor listeners. Empty (the default) keeps plaintext.
	TLSCertFile string `yaml:"tls_cert_file,omitempty" json:"tls_cert_file,omitempty"`
	TLSKeyFile  string `yaml:"tls_key_file,omitempty" json:"tls_key_file,omitempty"`
}
