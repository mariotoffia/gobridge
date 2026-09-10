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

// IsClusteredDeployment is the SINGLE canonical predicate for "is this a
// clustered deployment": either deployment_mode is "clustered" OR a static
// cluster.endpoints override is present. A nil config is never clustered.
//
// It lives in ports (which every layer imports) so runtime composition,
// config validation, and blueprint validation cannot drift onto different
// spellings — a mismatch previously let a static-endpoints deployment activate
// clustered runtime behavior while bypassing the clustered replica-safety
// validation (finding CLUSTER-1). bridge.IsClusteredDeployment and
// config.deploymentIsClustered delegate here.
func IsClusteredDeployment(cfg *BridgeConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.Bridge.DeploymentMode == "clustered" {
		return true
	}
	return cfg.Bridge.Cluster != nil && len(cfg.Bridge.Cluster.Endpoints) > 0
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
	// DrainTimeout bounds how long the supervisor lets a runtime drain when it
	// STOPS one — on shutdown, or when a reconfiguration swaps a runtime out.
	// It is the ceiling on Runtime.Stop and nothing else; the outbox drain
	// BATCH is bounded separately by PerRecordDrainTimeout/MaxDrainTimeout.
	// Default: 30s.
	DrainTimeout string `yaml:"drain_timeout,omitempty" json:"drain_timeout,omitempty"`
	// PerRecordDrainTimeout is the per-record allowance in the outbox drain
	// batch ceiling: min(batchCount * PerRecordDrainTimeout, MaxDrainTimeout).
	// Default: 3s.
	PerRecordDrainTimeout string `yaml:"per_record_drain_timeout,omitempty" json:"per_record_drain_timeout,omitempty"`
	// MaxDrainTimeout is the upper bound of that batch ceiling. Default: 10s.
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
	// Members is the STATIC roster of member ids forming the coordinated-rollout
	// cohort. It is the membership epoch the rollout barrier freezes at Propose
	// and compares live membership against (ADR 0013), so it MUST be
	// identical on every member and MUST NOT contain duplicates.
	//
	// It is deliberately a SEPARATE key from Endpoints: Endpoints is THIS
	// instance's capability map keyed by capability name ("http"), not a peer
	// roster, so its keys cannot serve as member ids (a cohort would freeze the
	// epoch as ["http"] — a one-member barrier, i.e. no barrier at all).
	//
	// A member id must match the id the process announces to the barrier
	// (bridge.WithClusterRollout's MemberID, wired by the composition root from
	// the node's own stable identity — task id, pod name, host). It cannot be
	// derived from bridge.instance_id, which is empty in a shared-config cohort
	// so that every task derives a unique metric identity at runtime.
	//
	// Required (non-empty) when Rollout is "coordinated"; ignored otherwise —
	// including under "independent", which counts no acknowledgements and so has
	// nothing to count them against.
	Members []string `yaml:"members,omitempty" json:"members,omitempty"`
	// Rollout selects the live-config-change strategy for this clustered
	// deployment. Three values, in order of how much they cost to run:
	//
	//   - "" or "refuse" (default): a clustered node rejects any live config
	//     delta and requires whole-cohort replacement (ADR 0012). Nothing to
	//     provision; every change costs a stop-and-redeploy.
	//   - "independent": every member applies a live-safe delta on its own, the
	//     way a standalone bridge does. No barrier, no vote, no shared store, no
	//     roster. The cost is a brief window in which one member is running the
	//     new config and another is still on the old one; taking that cost is the
	//     operator's decision, which is why it is a value rather than a default.
	//     A delta that cannot be applied live on ANY node — a durable session's
	//     identity, a store's target — is still refused, with the same reason a
	//     standalone bridge gives.
	//   - "coordinated": the rollout barrier (design
	//     cluster-config-rollout-protocol.md). Every member builds the candidate
	//     first and nobody swaps until all of them have, so a member that cannot
	//     run the change stops it reaching any of them. Requires a shared rollout
	//     store, a lease-elected coordinator and a non-empty Members roster.
	Rollout string `yaml:"rollout,omitempty" json:"rollout,omitempty"`
	// ConfirmWindow opts a coordinated rollout into the NETCONF/NSO confirm window
	// (ADR 0014): a Go duration string (e.g. "90s"). Empty or "0s" (the default)
	// is the base protocol — a commit is final. A positive value makes every commit
	// PROVISIONAL: each member swaps then must reach convergence, the coordinator
	// confirms when the whole cohort converged, and if confirmation never lands every
	// member reverts to the last confirmed generation. Only valid when Rollout is
	// "coordinated". A failed trial costs two disruptions (apply + revert), so it is
	// opt-in.
	ConfirmWindow string `yaml:"confirm_window,omitempty" json:"confirm_window,omitempty"`
}

// ConfirmWindowDuration parses the confirm window (ADR 0014). Empty or malformed
// yields 0 (the base protocol — commit is final); the config validator rejects a
// malformed or non-positive value on the load path, so a value reaching here is
// either absent or already valid.
func (c ClusterConfig) ConfirmWindowDuration() time.Duration {
	d, err := time.ParseDuration(c.ConfirmWindow)
	if err != nil || d < 0 {
		return 0
	}
	return d
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
// HTTPConfig configures the process-level admin and monitor HTTP servers.
//
// RESTART REQUIRED. Every field except the API keys is LISTENER TOPOLOGY: the
// composition root binds both servers once, at startup, from the configuration
// it booted with. A reload that changes an address, a TLS pair or the CORS
// policy is validated and durably stored, and then does nothing to the running
// listeners — and a config that adds an `http` block to a process that started
// without one creates no servers at all. Restart the process to apply such a
// change. Where a composition root can see the divergence it reports it through
// the `restart_required` field of the /deephealth config_watch projection.
//
// The API keys are the exception: a root that wires a key provider reads them
// per request, so a rotation applies immediately.
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
