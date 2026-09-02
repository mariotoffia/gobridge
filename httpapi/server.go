package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// adminWriteTimeoutMargin is added to the longest admin response path to derive
// the admin server WriteTimeout, so a slow-but-successful admin operation can
// flush its response before the write deadline fires.
const adminWriteTimeoutMargin = 15 * time.Second

// commitWorstCaseResponse is the longest a config-transaction commit can take
// before it answers. The durable write is followed by a DETACHED apply bounded
// by commitApplyTimeout, and an apply that fails then runs the rollback restore
// under a fresh context bounded by commitApplyTimeout again, before the handler
// finally reports rolled_back. Both bounds are server-side and deliberate, so
// the admin write deadline has to outlive their sum: a shorter one resets the
// operator's connection while the commit is still legitimately deciding its
// outcome, and automation is left retrying against a state it cannot observe.
const commitWorstCaseResponse = 2 * commitApplyTimeout

// Config holds HTTP server configuration.
type Config struct {
	AdminAddr     string        `json:"admin_addr"`
	MonitorAddr   string        `json:"monitor_addr"`
	AdminAPIKey   shared.Secret `json:"-"`
	MonitorAPIKey shared.Secret `json:"-"`
	CORSOrigins   string        `json:"cors_origins"`

	// AdminAPIKeyProvider returns the current admin API key as a raw
	// string (e.g. fetched from a secret manager on rotation). When nil,
	// the static AdminAPIKey is used. The server wraps whatever this
	// returns in a redacting shared.Secret before any comparison.
	AdminAPIKeyProvider func() string `json:"-"`

	// AdminAPIKeys maps operator-facing key NAMES to admin API keys. Each name
	// becomes the audit Actor when that key authenticates. The legacy single
	// AdminAPIKey folds in under the name "admin"; an explicit "admin" entry
	// here overrides it. At least one admin key must exist after folding.
	AdminAPIKeys map[string]shared.Secret `json:"-"`

	// AdminAPIKeysProvider returns the current named admin keys as raw strings
	// (e.g. fetched/rotated from a secret manager). When set it replaces the
	// static AdminAPIKeys per request; each value is wrapped in a redacting
	// shared.Secret before comparison. Mirrors AdminAPIKeyProvider.
	AdminAPIKeysProvider func() map[string]string `json:"-"`

	// MonitorAPIKeyProvider returns the current monitor API key as a raw
	// string. When nil, the static MonitorAPIKey is used. The server
	// wraps the returned value in a redacting shared.Secret before use.
	MonitorAPIKeyProvider func() string `json:"-"`

	// RuntimeProvider returns the current runtime backing the admin/monitor
	// APIs. When this FIELD is nil the Server uses the runtime passed to New().
	// When it is set it is authoritative: a nil return means "no runtime right
	// now" and the endpoints answer 503 — the Server never falls back to the
	// constructor runtime, which a composition root has long stopped.
	RuntimeProvider func() ports.Runtime `json:"-"`

	// TerminalProvider reports whether the PROCESS is in an unrecoverable state
	// that warrants a restart, independently of any runtime the server can see.
	// It exists for the wedged case: when a composition root's swap and its
	// recovery both fail there is no active runtime at all, which looks exactly
	// like a healthy swap window to a runtime-only probe, so /live would answer
	// 200 for a process that routes nothing. Wire it to the supervisor's own
	// terminal state (bridge.Supervisor.Terminal). When nil, /live falls back to
	// the runtime's Terminal() alone.
	TerminalProvider func() bool `json:"-"`

	// BridgeController, when set, routes admin start/stop through the
	// composition-root supervisor rather than the runtime directly. This makes
	// POST /bridge/stop a CLEAN, deliberate pause (not process-suicide) and lets
	// POST /bridge/start rebuild a fresh runtime afterwards — the runtime is
	// single-use, so an in-place restart would always 409. Wired to
	// bridge.Supervisor in the composition root. When nil, the handlers fall back
	// to the runtime's own Start/Stop (legacy behavior, used by tests that hold
	// only a runtime).
	BridgeController BridgeController `json:"-"`

	// ConfigStore is the persistence boundary used by the admin
	// transactions API: validate / merge / save / load. The
	// composition root supplies an implementation (typically backed
	// by config.Manager). When set together with ConfigProvider,
	// the config management endpoints are enabled on the admin server.
	ConfigStore ports.ConfigStore `json:"-"`

	// ConfigProvider returns the current effective BridgeConfig.
	// Typically wired to bridge.Supervisor.Config().
	ConfigProvider func() *ports.BridgeConfig `json:"-"`

	// DegradedProvider reports whether live reconfiguration is currently
	// degraded and a human-readable reason. "Degraded" means the bridge keeps
	// serving its last good config but can no longer observe config changes
	// (the supervisor's config-change stream closed, or a config layer's watcher
	// cannot be re-established). Typically wired to combine
	// bridge.Supervisor.Degraded() with config.Manager.WatchDegraded(). When
	// set, the authenticated /deephealth endpoint surfaces a config_watch
	// projection so operators can see a bridge running blind; when nil the
	// projection is omitted (it is purely additive).
	DegradedProvider func() (degraded bool, reason string) `json:"-"`

	// ConfigWatchProvider reports structured live-reconfiguration health,
	// including desired/running divergence and the last apply error. When set it
	// supersedes DegradedProvider for the config_watch deep-health projection.
	ConfigWatchProvider func() ConfigWatchHealth `json:"-"`

	// AdminOperationTimeout is the context timeout applied to admin
	// start/stop operations. Defaults to 30s when zero.
	AdminOperationTimeout time.Duration `json:"admin_operation_timeout,omitempty"`

	// TLSCertFile and TLSKeyFile are filesystem paths to the PEM server
	// certificate (with chain) and private key. When BOTH are set the admin
	// and monitor listeners serve HTTPS and AdminURL/MonitorURL report the
	// https scheme. When either is empty the servers stay plaintext (the
	// default) and rely on an external TLS terminator. Setting only one is a
	// startup error.
	TLSCertFile string `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string `json:"tls_key_file,omitempty"`

	// ConfigApplier, when set, is invoked by a successful config-transaction
	// Commit AFTER the new blueprint is persisted, so the running runtime is
	// reconfigured in-band rather than relying solely on the file watcher. A
	// non-nil error fails the commit response (the durable write already
	// happened; the operator must reconcile) instead of falsely reporting
	// "committed" while the runtime diverges. When nil, application is
	// delegated to the config watcher (historical behavior).
	ConfigApplier func(ctx context.Context, cfg *ports.BridgeConfig) error `json:"-"`

	// ConfigSingleWriter asserts that THIS admin process is the SOLE writer of
	// the durable config store (no other instance commits against the same
	// file/NFS/EFS/backend). It is the explicit operator opt-in that permits a
	// durable config-transaction commit through a ConfigStore that does NOT
	// implement ports.ConditionalConfigStore (compare-and-swap).
	//
	// Fail-closed default: when the configured ConfigStore is non-CAS and this
	// is false, a durable commit is REFUSED rather than falling back to a plain
	// last-writer-wins Save. A plain Save on a shared non-CAS backend lets two
	// admin instances that both read version N each pass the read-time version
	// guard and clobber each other's acknowledged commit (silent lost update;
	// see). Only assert this when the deployment guarantees a single
	// admin writer; a multi-instance cluster MUST use a ConditionalConfigStore
	// instead, which is always safe regardless of this flag.
	ConfigSingleWriter bool `json:"config_single_writer,omitempty"`

	// AuthFailureLimit bounds failed authentication attempts per client within
	// AuthFailureWindow before further attempts from that client are rejected
	// with 429. Zero uses defaultAuthFailureLimit. AuthFailureWindow zero uses
	// defaultAuthFailureWindow.
	AuthFailureLimit  int           `json:"auth_failure_limit,omitempty"`
	AuthFailureWindow time.Duration `json:"auth_failure_window,omitempty"`
}

// DefaultConfig returns a Config with security-first defaults.
// CORS is disabled (empty origins) and must be explicitly configured.
// API keys must be set before starting; the server rejects startup
// without an AdminAPIKey.
func DefaultConfig() Config {
	return Config{
		AdminAddr:   ":8080",
		MonitorAddr: ":8081",
	}
}

// BridgeController is the composition-root seam the admin start/stop handlers
// use to pause and resume the bridge through the supervisor. StopBridge performs
// a clean deliberate stop (non-terminal, so /live stays 200 and the liveness
// backstop does not restart the process); StartBridge rebuilds and starts a
// fresh runtime (the runtime is single-use, so resume means build-anew).
// bridge.Supervisor satisfies this interface.
type BridgeController interface {
	StartBridge(ctx context.Context) error
	StopBridge(ctx context.Context) error
}

// Server manages the admin and monitor HTTP endpoints.
type Server struct {
	rt                 ports.Runtime
	rtProvider         func() ports.Runtime
	bridgeController   BridgeController
	adminKeyProvider   func() shared.Secret
	adminKeysProvider  func() map[string]shared.Secret
	monitorKeyProvider func() shared.Secret
	cfg                Config
	logger             *slog.Logger
	audit              ports.AuditLogger
	metrics            ports.MetricsExporter // optional admin-plane metrics sink; nil = no-op
	clk                clock.Clock
	idGen              idGenFn
	configTxn          *configTxnManager // nil when config management is disabled
	// adminThrottle and monitorThrottle are SEPARATE failed-auth rate limiters
	// so a monitor-plane brute-forcer cannot fill a shared window and lock out
	// the admin plane (and vice versa). Both throttle FAILED authentication
	// only — a valid credential is checked first and always passes, so a bad-key
	// spammer behind a shared LB/NAT peer cannot deny a valid operator.
	adminThrottle   *authThrottle
	monitorThrottle *authThrottle

	admin    *http.Server
	monitor  *http.Server
	adminURL string // actual bound address (e.g. "http://127.0.0.1:54321")
	monURL   string // actual bound monitor address

	mu      sync.Mutex
	running bool
}

// Option configures a Server.
type Option func(*Server)

// WithServerLogger sets the logger.
func WithServerLogger(l *slog.Logger) Option {
	return func(s *Server) { s.logger = l }
}

// WithAuditLogger sets the audit logger for security-relevant operations.
func WithAuditLogger(a ports.AuditLogger) Option {
	return func(s *Server) { s.audit = a }
}

// WithMetrics sets the metrics exporter for admin-plane operational metrics
// (DLQ redrive outcomes). Optional: a nil exporter (the default) makes emission
// a no-op, so existing constructions that omit it are unaffected. Pass the SAME
// exporter the runtime receives so admin and runtime metrics share one sink —
// do not construct a second exporter.
func WithMetrics(m ports.MetricsExporter) Option {
	return func(s *Server) { s.metrics = m }
}

// WithClock sets the clock used for request timestamps, durations, and admin transaction time.
func WithClock(c clock.Clock) Option {
	return func(s *Server) {
		if c != nil {
			s.clk = c
		}
	}
}

// idGenFn produces a fresh envelope ID. It is the seam used by the
// admin Inject endpoint when the request body omits an "id" field. A
// function (rather than a clock.Clock dependency) keeps tests
// deterministic without pulling crypto/rand into every spec.
type idGenFn func() string

// WithIDGenerator overrides the envelope-ID generator used by the
// admin Inject endpoint. Tests inject a deterministic generator;
// production callers leave it unset so the default crypto/rand UUID
// generator is used.
func WithIDGenerator(fn idGenFn) Option {
	return func(s *Server) {
		if fn != nil {
			s.idGen = fn
		}
	}
}

// New creates an HTTP Server bound to the given runtime.
func New(rt ports.Runtime, cfg Config, opts ...Option) *Server {
	s := &Server{rt: rt, cfg: cfg}
	for _, o := range opts {
		o(s)
	}
	if s.audit == nil {
		s.audit = ports.NoopAuditLogger{}
	}
	if s.clk == nil {
		s.clk = clock.System
	}
	if s.idGen == nil {
		s.idGen = defaultIDGen
	}
	if cfg.RuntimeProvider != nil {
		s.rtProvider = cfg.RuntimeProvider
	} else {
		s.rtProvider = func() ports.Runtime { return rt }
	}
	if cfg.BridgeController != nil {
		s.bridgeController = cfg.BridgeController
	}
	if cfg.AdminAPIKeyProvider != nil {
		// Validate every dynamically-refreshed single admin key with the same
		// strength floor validateConfig enforces at startup, failing closed to
		// the last good key: a rotation that returns a below-floor key can never
		// be installed after startup: a dynamic provider must not be able to
		// install a weak key that startup validation would have rejected.
		vp := &validatedKeyProvider{
			raw:      cfg.AdminAPIKeyProvider,
			validate: validateDynamicAdminKey,
			logger:   s.logger,
			what:     "admin",
		}
		s.adminKeyProvider = vp.get
	} else {
		s.adminKeyProvider = func() shared.Secret { return cfg.AdminAPIKey }
	}
	if cfg.AdminAPIKeysProvider != nil {
		// Validate the whole refreshed named-key set (per-key name + length) and
		// reject it atomically on any failure, keeping the last good set — a bad
		// rotation must not install a weak/unsafe key that startup would reject.
		vp := &validatedKeysProvider{
			raw:    cfg.AdminAPIKeysProvider,
			logger: s.logger,
		}
		s.adminKeysProvider = vp.get
	}
	if cfg.MonitorAPIKeyProvider != nil {
		// Same fail-closed validation for the rotated monitor key.
		vp := &validatedKeyProvider{
			raw:      cfg.MonitorAPIKeyProvider,
			validate: ValidateMonitorKey,
			logger:   s.logger,
			what:     "monitor",
		}
		s.monitorKeyProvider = vp.get
	} else {
		s.monitorKeyProvider = func() shared.Secret { return cfg.MonitorAPIKey }
	}
	if s.cfg.AdminOperationTimeout <= 0 {
		s.cfg.AdminOperationTimeout = 30 * time.Second
	}
	if cfg.ConfigStore != nil && cfg.ConfigProvider != nil {
		s.configTxn = newTxnManager(cfg.ConfigStore, cfg.ConfigProvider, cfg.ConfigApplier, s.logger, s.clk)
		// Cluster-safety gate for durable commits: a non-CAS ConfigStore may
		// only be committed to when the operator has explicitly asserted a
		// single writer. newTxnManager defaults to single-writer (the in-process
		// construction used by tests/embedders); the real server path must fail
		// closed on a shared non-CAS store unless ConfigSingleWriter is set, so
		// no silent last-writer-wins durable commit path remains (see).
		s.configTxn.singleWriter = cfg.ConfigSingleWriter
	}
	s.adminThrottle = newAuthThrottle(s.clk, cfg.AuthFailureLimit, cfg.AuthFailureWindow)
	s.monitorThrottle = newAuthThrottle(s.clk, cfg.AuthFailureLimit, cfg.AuthFailureWindow)
	return s
}

// adminWriteTimeout derives the admin listener's WriteTimeout from the LONGEST
// response path the server can actually serve, plus a flush margin. Every admin
// start/stop is bounded by AdminOperationTimeout; a server with the config
// transaction endpoints enabled can additionally hold a request open for
// commitWorstCaseResponse. Taking the maximum keeps a slow-but-successful
// operation's response deliverable, while a server without those endpoints
// keeps the tighter deadline and its slow-client protection.
func (s *Server) adminWriteTimeout() time.Duration {
	longest := s.cfg.AdminOperationTimeout
	if s.configTxn != nil && commitWorstCaseResponse > longest {
		longest = commitWorstCaseResponse
	}
	return longest + adminWriteTimeoutMargin
}

// currentRuntime returns the runtime backing the admin and monitor endpoints.
// A configured RuntimeProvider OWNS the answer: when it reports no runtime the
// server serves none. It must NOT fall back to the runtime handed to New(),
// which is the composition root's BOOT runtime — long stopped by the time a
// provider is wired. Falling back made every endpoint answer from that dead
// object while the process was between runtimes: boot routes on /topology, a
// closed store behind the DLQ endpoints, and a healthy-looking /live. The
// constructor runtime is the answer only for an embedder that wires no
// provider at all.
func (s *Server) currentRuntime() ports.Runtime { //nolint:ireturn // intentional: the server depends on the ports.Runtime driving-port interface, not the concrete runtime type
	if s.rtProvider != nil {
		return s.rtProvider()
	}
	return s.rt
}

func (s *Server) currentAdminAPIKey() shared.Secret {
	if s.adminKeyProvider != nil {
		key := s.adminKeyProvider()
		// Warn only when an explicit single-key provider was configured and
		// returned empty (a rotation failure). The default wrapper returns the
		// static AdminAPIKey, which is legitimately empty when only the named
		// AdminAPIKeys map is configured — that path must not warn per request.
		if key.IsZero() && s.cfg.AdminAPIKeyProvider != nil && s.logger != nil {
			s.logger.Warn("admin API key provider returned empty key; all admin requests will be rejected")
		}
		return key
	}
	return s.cfg.AdminAPIKey
}

// currentAdminAPIKeys folds the legacy single admin key (as name "admin")
// with the named keys (static or from the provider). Named keys win on a
// name collision. Zero-value secrets are skipped.
func (s *Server) currentAdminAPIKeys() map[string]shared.Secret {
	out := make(map[string]shared.Secret)
	if k := s.currentAdminAPIKey(); !k.IsZero() {
		out["admin"] = k
	}
	named := s.cfg.AdminAPIKeys
	if s.adminKeysProvider != nil {
		named = s.adminKeysProvider()
	}
	for name, k := range named {
		if !k.IsZero() {
			out[name] = k
		}
	}
	return out
}

func (s *Server) currentMonitorAPIKey() shared.Secret {
	if s.monitorKeyProvider != nil {
		return s.monitorKeyProvider()
	}
	return s.cfg.MonitorAPIKey
}

// Start starts both HTTP servers. It validates configuration, binds
// listeners synchronously so port conflicts are detected immediately,
// then serves in background.
