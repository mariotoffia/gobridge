package config

import "time"

// BridgeConfig is the root configuration for a GoBridge instance.
type BridgeConfig struct {
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
	DrainTimeout    string `yaml:"drain_timeout,omitempty" json:"drain_timeout,omitempty"`
	LogLevel        string `yaml:"log_level,omitempty" json:"log_level,omitempty"`
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
	ID        string             `yaml:"id" json:"id"`
	Transport string             `yaml:"transport" json:"transport"`
	SessionID string             `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Topics    []SubscriptionDef  `yaml:"topics,omitempty" json:"topics,omitempty"`
	Options   map[string]any     `yaml:"options,omitempty" json:"options,omitempty"`
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
	ID           string    `yaml:"id" json:"id"`
	ReceiverID   string    `yaml:"receiver_id" json:"receiver_id"`
	DeliveryMode string    `yaml:"delivery_mode,omitempty" json:"delivery_mode,omitempty"`
	DispatchMode string    `yaml:"dispatch_mode,omitempty" json:"dispatch_mode,omitempty"`
	Policy       PolicyDef `yaml:"policy,omitempty" json:"policy,omitempty"`
	Bindings     []string  `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	Processors   []string  `yaml:"processors,omitempty" json:"processors,omitempty"`
	Session      *RouteSessionDef `yaml:"session,omitempty" json:"session,omitempty"`
}

// RouteSessionDef configures session management for a route that targets
// an exclusive MQTT session or similar stateful transport.
type RouteSessionDef struct {
	SessionID         string            `yaml:"session_id" json:"session_id"`
	SenderID          string            `yaml:"sender_id" json:"sender_id"`
	LeaseTTL          string            `yaml:"lease_ttl,omitempty" json:"lease_ttl,omitempty"`
	RenewInterval     string            `yaml:"renew_interval,omitempty" json:"renew_interval,omitempty"`
	MaxRenewFails     int               `yaml:"max_renew_fails,omitempty" json:"max_renew_fails,omitempty"`
	StepDownGrace     string            `yaml:"step_down_grace,omitempty" json:"step_down_grace,omitempty"`
	DrainInterval     string            `yaml:"drain_interval,omitempty" json:"drain_interval,omitempty"`
	DrainBatchSize    int               `yaml:"drain_batch_size,omitempty" json:"drain_batch_size,omitempty"`
	DrainStrategy     *DrainStrategyDef `yaml:"drain_strategy,omitempty" json:"drain_strategy,omitempty"`
	ConnectAfterLease bool              `yaml:"connect_after_lease,omitempty" json:"connect_after_lease,omitempty"`
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
}

// BackoffDef defines retry backoff as YAML-friendly strings.
type BackoffDef struct {
	InitialInterval string  `yaml:"initial_interval,omitempty" json:"initial_interval,omitempty"`
	MaxInterval     string  `yaml:"max_interval,omitempty" json:"max_interval,omitempty"`
	Multiplier      float64 `yaml:"multiplier,omitempty" json:"multiplier,omitempty"`
}

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
