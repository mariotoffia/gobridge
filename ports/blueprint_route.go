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
// tag-free (schema-tagged DTOs by design).
//
// The config sub-package (in `config/`) implements the YAML/JSON
// parser, validator, merger, and on-disk manager that produce
// *ports.BridgeConfig values.
package ports

// Route-graph blueprint definitions: the sessions, receivers, subscriptions,
// senders, bindings, routes and policies a BridgeConfig wires together. Split
// out of blueprint.go, which holds the process-level bridge, cluster and store
// configuration those routes run inside.
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
	// storm can never outlast the lease (contract).
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
	// span folds this in and must stay below lease_ttl. Empty
	// means the session manager derives it from RenewInterval.
	RenewCallTimeout string `yaml:"renew_call_timeout,omitempty" json:"renew_call_timeout,omitempty"`

	// FailoverSLO is an optional failure-detection to ServiceLevelFull objective.
	// Preflight budgets LeaseTTL plus two independently jittered acquire-poll
	// boundaries, every possible minimum-jitter observation Acquire call,
	// complete transport activation, and StartupAllowance. It is a duration string; empty means no SLO is declared.
	FailoverSLO string `yaml:"failover_slo,omitempty" json:"failover_slo,omitempty"`

	// StartupAllowance is explicit bounded time reserved for process-side work
	// outside lease, broker-connect, and reconcile calls. Empty means zero.
	StartupAllowance string `yaml:"startup_allowance,omitempty" json:"startup_allowance,omitempty"`

	// BrokerHealthStepDown is the broker-path failover decision, and it is
	// TRI-state:
	//
	//	""     the decision was not made. Refused when FailoverSLO is declared.
	//	"off"  broker-path failover is deliberately disabled; a declared
	//	       FailoverSLO then covers owner death alone.
	//	"90s"  an ACTIVE exclusive owner whose broker path stays non-converged
	//	       (disconnected / not re-subscribed) that long releases the lease so
	//	       a healthy standby can take over a node-local broker outage the
	//	       lease machinery alone cannot detect (CLUSTER-2).
	//
	// Enabled, it carries a failover budget of its own that a declared
	// FailoverSLO must also admit.
	BrokerHealthStepDown string `yaml:"broker_health_step_down,omitempty" json:"broker_health_step_down,omitempty"`

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
	// only to opt out. It is a pointer so an omitted flag is
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
	// Jitter is the equal-jitter fraction in [0,1] applied to each computed
	// backoff delay, de-correlating retries across replicas. It is a POINTER
	// because omitting it and writing 0 mean different things: omitted takes the
	// recommended default (routing.DefaultJitterFactor), an explicit 0 opts out
	// and keeps the deterministic exponential delay. Maps to
	// routing.BackoffPolicy.JitterFactor.
	Jitter *float64 `yaml:"jitter,omitempty" json:"jitter,omitempty"`
}

// BlueprintValidator is the contract a config validator implements so
// the bridge composition layer can verify a *BridgeConfig without
// importing the parser/validator package directly. config.Validate
// satisfies this signature; the composition root supplies it (or any
// other implementation) to bridge.WithBlueprintValidator.
