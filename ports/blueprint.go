// Package ports — bridge blueprint.
//
// BridgeConfig and its sub-types describe the parsed input that a
// gobridge runtime is built from. They live in `ports/` (not in the
// config parser package) so the bridge composition layer and the
// admin HTTP layer can consume them without depending on any
// particular configuration format. The yaml/json struct tags are
// kept on the types because adapters that read from disk parse them
// in-place — but the ports package itself has no yaml/json runtime
// dependency, so the inner ring stays format-neutral.
//
// The config sub-package (in `config/`) implements the YAML/JSON
// parser, validator, merger, and on-disk manager that produce
// *ports.BridgeConfig values.
package ports

import "time"

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

// ClusterConfig configures cluster membership and endpoint discovery.
// Endpoints are normally auto-discovered via EndpointResolver at startup.
// The Endpoints field is an optional static override for special cases.
type ClusterConfig struct {
	Endpoints map[string]string `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
}

// ShutdownTimeoutDuration parses the shutdown timeout string.
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
	Lease  *StoreConfig `yaml:"lease,omitempty" json:"lease,omitempty"`
	Outbox *StoreConfig `yaml:"outbox,omitempty" json:"outbox,omitempty"`
	DLQ    *StoreConfig `yaml:"dlq,omitempty" json:"dlq,omitempty"`
}

// StoreConfig describes a single store backend.
type StoreConfig struct {
	Type    string         `yaml:"type" json:"type"`
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

// SessionDef describes a transport session (e.g. an MQTT connection).
type SessionDef struct {
	ID          string         `yaml:"id" json:"id"`
	Transport   string         `yaml:"transport" json:"transport"`
	SessionMode string         `yaml:"session_mode,omitempty" json:"session_mode,omitempty"`
	Options     map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

// ReceiverDef describes a message ingress endpoint.
type ReceiverDef struct {
	ID        string            `yaml:"id" json:"id"`
	Transport string            `yaml:"transport" json:"transport"`
	SessionID string            `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Topics    []SubscriptionDef `yaml:"topics,omitempty" json:"topics,omitempty"`
	Options   map[string]any    `yaml:"options,omitempty" json:"options,omitempty"`
}

// SubscriptionDef describes a topic subscription for a receiver.
type SubscriptionDef struct {
	Topic   string         `yaml:"topic" json:"topic"`
	QoS     int            `yaml:"qos,omitempty" json:"qos,omitempty"`
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

// SenderDef describes a message egress endpoint.
type SenderDef struct {
	ID        string         `yaml:"id" json:"id"`
	Transport string         `yaml:"transport" json:"transport"`
	SessionID string         `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Options   map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

// BindingDef describes a concrete destination that a route may send to.
type BindingDef struct {
	ID        string         `yaml:"id" json:"id"`
	SenderID  string         `yaml:"sender_id" json:"sender_id"`
	SessionID string         `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Address   string         `yaml:"address" json:"address"`
	Options   map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

// RouteDef describes a message route from a receiver through an optional
// processor chain to one or more sender bindings.
type RouteDef struct {
	ID           string           `yaml:"id" json:"id"`
	ReceiverID   string           `yaml:"receiver_id" json:"receiver_id"`
	DeliveryMode string           `yaml:"delivery_mode,omitempty" json:"delivery_mode,omitempty"`
	DispatchMode string           `yaml:"dispatch_mode,omitempty" json:"dispatch_mode,omitempty"`
	Policy       PolicyDef        `yaml:"policy,omitempty" json:"policy,omitempty"`
	Bindings     []string         `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	Processors   []string         `yaml:"processors,omitempty" json:"processors,omitempty"`
	Resolver     *ResolverDef     `yaml:"resolver,omitempty" json:"resolver,omitempty"`
	Session      *RouteSessionDef `yaml:"session,omitempty" json:"session,omitempty"`
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

	// MaxRenewFails is consecutive renewal failures before step-down.
	// Zero means use the runtime default (3).
	MaxRenewFails int `yaml:"max_renew_fails,omitempty" json:"max_renew_fails,omitempty"`

	// StepDownGrace is how long to wait for in-flight operations before
	// releasing the lease (e.g. "15s"). Empty means use the runtime default.
	StepDownGrace string `yaml:"step_down_grace,omitempty" json:"step_down_grace,omitempty"`

	DrainInterval       string            `yaml:"drain_interval,omitempty" json:"drain_interval,omitempty"`
	DrainBatchSize      int               `yaml:"drain_batch_size,omitempty" json:"drain_batch_size,omitempty"`
	DrainMaxBatchSize   int               `yaml:"drain_max_batch_size,omitempty" json:"drain_max_batch_size,omitempty"`
	DrainMaxConcurrency int               `yaml:"drain_max_concurrency,omitempty" json:"drain_max_concurrency,omitempty"`
	DrainStrategy       *DrainStrategyDef `yaml:"drain_strategy,omitempty" json:"drain_strategy,omitempty"`
	ConnectAfterLease   bool              `yaml:"connect_after_lease,omitempty" json:"connect_after_lease,omitempty"`
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
	MaxInFlight        int        `yaml:"max_in_flight,omitempty" json:"max_in_flight,omitempty"`
	AckAfter           string     `yaml:"ack_after,omitempty" json:"ack_after,omitempty"`
	MaxReplayAttempts  int        `yaml:"max_replay_attempts,omitempty" json:"max_replay_attempts,omitempty"`
	MaxOutboxDepth     int        `yaml:"max_outbox_depth,omitempty" json:"max_outbox_depth,omitempty"`
	OnExpired          string     `yaml:"on_expired,omitempty" json:"on_expired,omitempty"`
	OnPermanentFailure string     `yaml:"on_permanent_failure,omitempty" json:"on_permanent_failure,omitempty"`
	Backoff            BackoffDef `yaml:"backoff,omitempty" json:"backoff,omitempty"`
	SendTimeout        string     `yaml:"send_timeout,omitempty" json:"send_timeout,omitempty"`
	DepthCacheTTL      string     `yaml:"depth_cache_ttl,omitempty" json:"depth_cache_ttl,omitempty"`
	AllowUnfenced      bool       `yaml:"allow_unfenced,omitempty" json:"allow_unfenced,omitempty"`
	AllowRetryDrop     bool       `yaml:"allow_retry_drop,omitempty" json:"allow_retry_drop,omitempty"`
}

// BackoffDef defines retry backoff as YAML-friendly strings.
type BackoffDef struct {
	InitialInterval string  `yaml:"initial_interval,omitempty" json:"initial_interval,omitempty"`
	MaxInterval     string  `yaml:"max_interval,omitempty" json:"max_interval,omitempty"`
	Multiplier      float64 `yaml:"multiplier,omitempty" json:"multiplier,omitempty"`
}

// BlueprintValidator is the contract a config validator implements so
// the bridge composition layer can verify a *BridgeConfig without
// importing the parser/validator package directly. config.Validate
// satisfies this signature; the composition root supplies it (or any
// other implementation) to bridge.WithBlueprintValidator.
type BlueprintValidator func(*BridgeConfig) error

// HTTPConfig configures the optional HTTP admin and monitor servers.
// AdminAPIKey is mandatory when the HTTP block is present; the server
// refuses to start without it. MonitorAPIKey is optional; when empty
// the admin key is used for authenticated monitor endpoints. CORS is
// disabled by default and wildcard '*' is rejected.
type HTTPConfig struct {
	AdminAddr     string `yaml:"admin_addr,omitempty" json:"admin_addr,omitempty"`
	MonitorAddr   string `yaml:"monitor_addr,omitempty" json:"monitor_addr,omitempty"`
	AdminAPIKey   string `yaml:"admin_api_key,omitempty" json:"admin_api_key,omitempty"`
	MonitorAPIKey string `yaml:"monitor_api_key,omitempty" json:"monitor_api_key,omitempty"`
	CORSOrigins   string `yaml:"cors_origins,omitempty" json:"cors_origins,omitempty"`
}
