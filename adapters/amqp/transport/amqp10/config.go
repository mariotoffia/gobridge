package amqp10

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// SessionOptions configures the AMQP 1.0 session connection.
type SessionOptions struct {
	// Address is the SINGLE broker endpoint to dial, e.g.
	// "amqp://localhost:5672". Reconnection re-dials this same endpoint;
	// the adapter keeps no client-side broker list and performs no
	// multi-broker failover. For high availability, resolve Address to a
	// load balancer or DNS name that points at a healthy node (see the
	// package doc, "Connection & High Availability").
	Address             string        `mapstructure:"address" yaml:"address" json:"address"`
	ConnectTimeout      time.Duration `mapstructure:"connect_timeout" yaml:"connect_timeout" json:"connect_timeout"`
	ReconnectDelay      time.Duration `mapstructure:"reconnect_delay" yaml:"reconnect_delay" json:"reconnect_delay"`
	ReconnectMaxDelay   time.Duration `mapstructure:"reconnect_max_delay" yaml:"reconnect_max_delay" json:"reconnect_max_delay"`
	ReconnectMultiplier float64       `mapstructure:"reconnect_multiplier" yaml:"reconnect_multiplier" json:"reconnect_multiplier"`
	IdleTimeout         time.Duration `mapstructure:"idle_timeout" yaml:"idle_timeout" json:"idle_timeout"`
	MaxFrameSize        uint32        `mapstructure:"max_frame_size" yaml:"max_frame_size" json:"max_frame_size"`
	Username            string        `mapstructure:"username" yaml:"username" json:"username"`
	Password            shared.Secret `mapstructure:"password" yaml:"password" json:"password"`
	TLS                 *TLSConfig    `mapstructure:"tls" yaml:"tls" json:"tls"`
	ContainerID         string        `mapstructure:"container_id" yaml:"container_id" json:"container_id"`

	// SASLMechanism selects the SASL layer used at dial time:
	//
	//   - ""          — PLAIN when Username is set, otherwise no SASL.
	//   - "plain"     — SASL PLAIN with Username/Password.
	//   - "external"  — SASL EXTERNAL; authentication is carried by the
	//     transport layer (mTLS client certificate). Configure TLS
	//     cert/key material alongside this.
	//   - "anonymous" — SASL ANONYMOUS.
	SASLMechanism string `mapstructure:"sasl_mechanism" yaml:"sasl_mechanism" json:"sasl_mechanism"`

	// AllowInsecurePlain permits SASL PLAIN (username/password) over a
	// non-TLS scheme. SASL PLAIN transmits the credentials in cleartext
	// frames, so by default it is REJECTED at config validation on a
	// plaintext "amqp://" (or schemeless) address — the username and
	// password would travel on the wire in the clear (c7-plain-plaintext).
	// Set this to true to explicitly opt into that insecure path (e.g. a
	// trusted private network or local development); it is a deliberate,
	// auditable override, never the default. It has no effect on a TLS
	// scheme (amqps:// / amqp+ssl://), where PLAIN is already protected.
	AllowInsecurePlain bool `mapstructure:"allow_insecure_plain" yaml:"allow_insecure_plain" json:"allow_insecure_plain"`

	// LinkCloseTimeout is the deadline for closing an AMQP link or
	// session during cleanup (e.g. after a failure or reconnect).
	// Defaults to 5s if zero.
	LinkCloseTimeout time.Duration `mapstructure:"link_close_timeout" yaml:"link_close_timeout" json:"link_close_timeout"`

	// ConnectionMonitorFallback is the cadence at which the session
	// monitor loop wakes to RETRY a reconnect while the connection is
	// already known to be down (Conn.Done() has fired but a prior
	// reconnect attempt has not yet succeeded). Real disconnects are
	// detected immediately via Conn.Done(). This ticker is only a
	// reconnect backstop: it does NOT probe a live connection, so it
	// cannot by itself detect a broker that silently half-drops the TCP
	// connection without closing it — tryReconnect no-ops whenever
	// s.conn is still non-nil. Silent half-open drops are surfaced by the
	// AMQP idle_timeout (see IdleTimeout), not by this ticker. Defaults
	// to 30s if zero.
	ConnectionMonitorFallback time.Duration `mapstructure:"connection_monitor_fallback" yaml:"connection_monitor_fallback" json:"connection_monitor_fallback"`

	// Clock drives the reconnect backoff wait. When nil defaults to
	// clock.System (wall clock). Tests may inject a clocktest.Fake to
	// control the backoff sleep deterministically. It is an internal
	// dependency and must never be populated from YAML; the dash tag
	// excludes it from the strict options decoder.
	Clock clock.Clock `mapstructure:"-" yaml:"-" json:"-"`

	// containerIDGenerated records that applyDefaults synthesised
	// ContainerID (true) because none was configured, versus an operator
	// supplying it explicitly (false). A generated id is unique-per-replica
	// and stable across reconnects, but it CHANGES on every process
	// restart, so it cannot anchor a durable-subscription identity
	// (container-id + link name) across a restart — the broker would see a
	// new subscription and orphan everything retained for the old one. The
	// factory consults it to fail-closed a durable receiver
	// (durability_mode > 0) built without an explicit container_id
	// Unexported internal bookkeeping: never decoded from, nor
	// marshalled to, config.
	containerIDGenerated bool
}

// TLSConfig holds TLS settings for the AMQP 1.0 connection.
//
// PEM bytes (CACertPEM, CertPEM, KeyPEM) take precedence over file
// paths on the same field. This lets ApplyCredentials supply rotated
// material in-memory without writing to disk.
type TLSConfig struct {
	Enable     bool   `mapstructure:"enable" yaml:"enable" json:"enable"`
	CACertFile string `mapstructure:"ca_cert_file" yaml:"ca_cert_file" json:"ca_cert_file"`
	CertFile   string `mapstructure:"cert_file" yaml:"cert_file" json:"cert_file"`
	KeyFile    string `mapstructure:"key_file" yaml:"key_file" json:"key_file"`

	// CACertPEM, CertPEM, KeyPEM carry in-memory PEM material used
	// when credentials are rotated via ApplyCredentials. Non-empty
	// fields override the corresponding *File fields. They are
	// shared.Secret so the (sensitive) private-key material — and, for
	// uniformity, the cert/CA bundles — redact on JSON/YAML/log marshal;
	// the config-save path reveals explicitly (see shared.RevealSecrets).
	CACertPEM shared.Secret `mapstructure:"ca_cert_pem" yaml:"ca_cert_pem" json:"ca_cert_pem"`
	CertPEM   shared.Secret `mapstructure:"cert_pem" yaml:"cert_pem" json:"cert_pem"`
	KeyPEM    shared.Secret `mapstructure:"key_pem" yaml:"key_pem" json:"key_pem"`

	InsecureSkipVerify bool `mapstructure:"insecure_skip_verify" yaml:"insecure_skip_verify" json:"insecure_skip_verify"`
}

// RoutingType controls how Artemis (and compatible brokers) route
// messages on auto-created addresses.
type RoutingType int

const (
	// RoutingAnycast uses point-to-point (queue) semantics: messages
	// are stored in a queue and consumed by exactly one receiver.
	RoutingAnycast RoutingType = iota
	// RoutingMulticast uses pub-sub (topic) semantics: messages are
	// fanned out to all active subscriptions at send time.
	RoutingMulticast
)

func (rt RoutingType) capability() string {
	switch rt {
	case RoutingMulticast:
		return "topic"
	default:
		return "queue"
	}
}

// String returns the canonical textual form ("anycast"/"multicast").
func (rt RoutingType) String() string {
	if rt == RoutingMulticast {
		return "multicast"
	}
	return "anycast"
}

// UnmarshalText decodes the documented string forms ("anycast",
// "multicast", case-insensitive) as well as the legacy numeric forms
// ("0", "1") so both YAML shapes decode through the strict plugin
// options decoder (config/parser TextUnmarshaller hook). Plain YAML
// integers keep decoding natively via mapstructure's int conversion.
func (rt *RoutingType) UnmarshalText(text []byte) error {
	switch strings.ToLower(strings.TrimSpace(string(text))) {
	case "", "anycast", "0":
		*rt = RoutingAnycast
	case "multicast", "1":
		*rt = RoutingMulticast
	default:
		return fmt.Errorf("amqp10: invalid routing %q (want \"anycast\" or \"multicast\")", string(text))
	}
	return nil
}

// parseRoutingType interprets a generic options-map value as a
// RoutingType, accepting both the string forms ("anycast"/"multicast")
// and the numeric forms (0/1). ok is false for missing or invalid values.
func parseRoutingType(v any) (RoutingType, bool) {
	if s, ok := v.(string); ok {
		var rt RoutingType
		if err := rt.UnmarshalText([]byte(s)); err != nil {
			return RoutingAnycast, false
		}
		return rt, true
	}
	if n, ok := optUint32(map[string]any{"routing": v}, "routing"); ok && n <= 1 {
		return RoutingType(n), true
	}
	return RoutingAnycast, false
}

// ReceiverConfig configures an AMQP 1.0 receiver link.
type ReceiverConfig struct {
	Address        string
	LinkCredit     uint32
	DurabilityMode uint32
	Routing        RoutingType
	Session        *Session
	Logger         *slog.Logger
	Metrics        ports.MetricsExporter

	// SubscriptionName pins the AMQP link name. Durable subscriptions
	// (DurabilityMode > 0) are identified by container-id + link name;
	// a stable name is REQUIRED for the broker to resume an existing
	// subscription instead of orphaning it and creating a new one on
	// every reconnect. When empty and DurabilityMode > 0, a stable name
	// is derived from the session's ContainerID and the link Address.
	SubscriptionName string

	// Clock drives retry backoff waits. When nil defaults to
	// clock.System (wall clock). Tests may inject a clocktest.Fake to
	// control retry delays deterministically.
	Clock clock.Clock
}

// SenderConfig configures an AMQP 1.0 sender link.
type SenderConfig struct {
	Address        string
	Timeout        time.Duration
	DurabilityMode uint32
	Routing        RoutingType
	Session        *Session
	Logger         *slog.Logger
	Metrics        ports.MetricsExporter
	Clock          clock.Clock

	// Durable controls the AMQP message header durable flag on every
	// outbound message. Brokers (e.g. Artemis) treat durable=false
	// messages as non-persistent: they are lost on broker restart even
	// after the send was accepted. nil (unset) defaults to TRUE — the
	// safe, at-least-once-preserving choice; set explicitly to false to
	// trade durability for throughput.
	Durable *bool
}

// durable reports the effective message-durability flag (default true).
func (c *SenderConfig) durable() bool {
	return c.Durable == nil || *c.Durable
}

// DefaultSessionOptions returns SessionOptions with sensible defaults.
func DefaultSessionOptions() SessionOptions {
	return SessionOptions{
		ConnectTimeout:            30 * time.Second,
		ReconnectDelay:            1 * time.Second,
		ReconnectMaxDelay:         30 * time.Second,
		ReconnectMultiplier:       2.0,
		IdleTimeout:               defaultIdleTimeout,
		MaxFrameSize:              65536,
		ConnectionMonitorFallback: 30 * time.Second,
	}
}

// DefaultSenderOptions returns a SenderConfig with sensible defaults.
func DefaultSenderOptions() SenderConfig {
	return SenderConfig{
		Timeout: 30 * time.Second,
	}
}

// validate checks the session options. When credentialsPending is true
// the caller has an unresolved credentials_uri whose resolved set may
// still supply the SASL EXTERNAL client certificate, so the EXTERNAL
// cert-material check is deferred to Config.ApplyCredentials (deferred
// path). Every other check still runs at parse time.
func (o *SessionOptions) validate(credentialsPending bool) error {
	if o.Address == "" {
		return shared.ErrInvalidPayload.WithMessage("amqp10: Address is required")
	}
	mechanism := strings.ToLower(o.SASLMechanism)
	switch mechanism {
	case "", saslMechanismPlain, saslMechanismExternal, saslMechanismAnonymous:
	default:
		return shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"amqp10: invalid sasl_mechanism %q (want plain, external, or anonymous)", o.SASLMechanism))
	}

	// tls.enable is a silent no-op unless the address scheme selects
	// TLS. go-amqp applies ConnOptions.TLSConfig only for the "amqps" and
	// "amqp+ssl" schemes; on a plaintext "amqp" (or schemeless) address
	// the dial — and any SASL PLAIN credentials — travel in CLEARTEXT
	// while Health still reports Full. Reject the downgrade trap at config
	// time rather than connect insecurely.
	if o.TLS != nil && o.TLS.Enable && !schemeUsesTLS(o.Address) {
		return shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"amqp10: tls.enable is set but address %q is not a TLS scheme; "+
				"use amqps:// or amqp+ssl:// (refusing to connect in cleartext)", o.Address))
	}

	// c7-plain-plaintext: SASL PLAIN (explicit sasl_mechanism=plain, or
	// the inferred default when a username is present) sends the
	// credentials in cleartext. Over a non-TLS scheme that puts them on
	// the wire in the clear, so reject unless allow_insecure_plain is set.
	// When credentialsPending, a username may still arrive via
	// credentials_uri and select inferred PLAIN, so the inferred
	// empty-username case naturally passes here (usesSASLPlain is false)
	// and Config.ApplyCredentials re-runs this gate post-resolution. An
	// EXPLICIT sasl_mechanism=plain is rejected now regardless: the scheme
	// is fixed, so no resolution can make it secure.
	if err := o.validatePlainOverPlaintext(); err != nil {
		return err
	}

	// SASL EXTERNAL authenticates via the mTLS client certificate.
	// Without enabled TLS and client key-pair material it surfaces only as
	// an opaque broker SASL failure at dial; reject it up front. When
	// credentialsPending, the certificate may still arrive via the
	// resolved credentials_uri set, so defer this check to
	// Config.ApplyCredentials, which re-runs it post-resolution.
	if !credentialsPending && mechanism == saslMechanismExternal && !hasClientCertMaterial(o.TLS) {
		return shared.ErrInvalidPayload.WithMessage(
			"amqp10: sasl_mechanism=external requires TLS client certificate material " +
				"(tls.enable with cert_file/key_file or cert_pem/key_pem)")
	}

	// the AMQP 1.0 spec fixes the smallest permissible max-frame-size
	// at 512 octets (MIN-MAX-FRAME-SIZE). A smaller positive value is
	// rejected by the peer at open; catch it here. Zero means "unset" so
	// applyDefaults can supply the default.
	if o.MaxFrameSize != 0 && o.MaxFrameSize < amqpMinFrameSize {
		return shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"amqp10: max_frame_size %d is below the AMQP 1.0 minimum of %d octets",
			o.MaxFrameSize, amqpMinFrameSize))
	}
	return nil
}

// amqpMinFrameSize is the smallest max-frame-size permitted by the AMQP
// 1.0 spec (§2.7.1 open.max-frame-size, MIN-MAX-FRAME-SIZE).
const amqpMinFrameSize = 512

// defaultIdleTimeout is the default connection idle timeout when none is
// configured. go-amqp uses it as the read deadline for the connection
// (conn.go: SetReadDeadline(now + idleTimeout) before every read) and
// advertises idle_timeout/2 to the broker, so it is the upper bound on
// how long a SILENTLY half-open connection (SIGKILL / blackhole / NAT
// drop that never FINs) goes undetected — no bytes arrive, the read
// deadline fires, and the monitor reconnects.
//
// c7-idle-timeout: this is intentionally HA-oriented (<= 30s) so
// half-open detection meets the 30-60s failover target — a standby can
// reattach well inside the window. The monitor ticker deliberately does
// NOT probe a live connection (see ConnectionMonitorFallback), so this
// idle timeout is the ONLY thing that surfaces a half-open drop; a longer
// value (the previous 2m default) lagged standby reattach past 60s.
//
// Tradeoff: a lower idle timeout means keepalive frames flow more often
// and a genuine network stall longer than the timeout tears down an
// otherwise-healthy connection (the reconnect loop re-establishes it).
// Operators on a high-latency/lossy link who prefer fewer reconnects over
// fast failover may raise idle_timeout explicitly; an explicit value is
// always honored.
const defaultIdleTimeout = 30 * time.Second

// usesSASLPlain reports whether the effective SASL layer will be PLAIN,
// which transmits the username/password to the broker. It is PLAIN when:
//
//   - the Address URL carries userinfo. go-amqp's dialConn (v1.5.1
//     conn.go:224) UNCONDITIONALLY does cp.SASLType = SASLTypePlain(user,
//     pass) whenever u.User != nil, OVERRIDING whatever SASLType the
//     adapter assembled from SASLMechanism. So credentials embedded in the
//     Address (amqp://user:pass@host, or even username-only) put PLAIN on
//     the wire regardless of sasl_mechanism — this branch must win over
//     the mechanism switch below.
//   - sasl_mechanism is explicitly "plain", or
//   - the mechanism is unset and a username is present — the inferred
//     default applied by defaultDial.
func (o *SessionOptions) usesSASLPlain() bool {
	if u, err := url.Parse(o.Address); err == nil && u.User != nil {
		return true
	}
	switch strings.ToLower(o.SASLMechanism) {
	case saslMechanismPlain:
		return true
	case "":
		return o.Username != ""
	default:
		return false
	}
}

// validatePlainOverPlaintext fails closed when SASL PLAIN credentials
// would travel over a non-TLS scheme. SASL PLAIN sends the
// username/password in cleartext frames, so on a plaintext "amqp://" (or
// schemeless) address they are exposed on the wire (c7-plain-plaintext).
// The only escape hatch is an explicit allow_insecure_plain opt-in, so
// the insecure path is a deliberate, auditable operator decision rather
// than a silent default. A TLS scheme (amqps:// / amqp+ssl://) already
// protects the credentials, so it passes regardless of the opt-in.
func (o *SessionOptions) validatePlainOverPlaintext() error {
	if o.usesSASLPlain() && !o.AllowInsecurePlain && !schemeUsesTLS(o.Address) {
		return shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"amqp10: SASL PLAIN sends username/password in cleartext but address %q is not a TLS scheme; "+
				"use amqps:// or amqp+ssl://, or set allow_insecure_plain=true to send credentials in cleartext anyway",
			o.Address))
	}
	return nil
}

// schemeUsesTLS reports whether address carries a scheme for which
// go-amqp negotiates TLS ("amqps" or "amqp+ssl"). It mirrors the SDK's
// own dialConn scheme test exactly: url.Parse lower-cases the scheme, so
// a case-variant such as "AMQPS://" is handled identically to go-amqp. A
// missing or unparyable scheme is treated as plaintext.
func schemeUsesTLS(address string) bool {
	u, err := url.Parse(address)
	if err != nil {
		return false
	}
	return u.Scheme == "amqps" || u.Scheme == "amqp+ssl"
}

// hasClientCertMaterial reports whether cfg carries a usable client
// key-pair (file paths or in-memory PEM) over an enabled TLS layer — the
// material SASL EXTERNAL needs to present a client certificate.
func hasClientCertMaterial(cfg *TLSConfig) bool {
	if cfg == nil || !cfg.Enable {
		return false
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		return true
	}
	return !cfg.CertPEM.IsZero() && !cfg.KeyPEM.IsZero()
}

// Recognized sasl_mechanism values (lowercase canonical form).
const (
	saslMechanismPlain     = "plain"
	saslMechanismExternal  = "external"
	saslMechanismAnonymous = "anonymous"
)

// BuildTLSConfig constructs a *tls.Config from the TLSConfig options.
// Returns nil if cfg is nil or TLS is not enabled.
func BuildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	if cfg == nil || !cfg.Enable {
		return nil, nil
	}

	if cfg.InsecureSkipVerify {
		slog.Warn("amqp10: TLS certificate verification disabled — not recommended for production")
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // caller-controlled
	}

	switch {
	case !cfg.CACertPEM.IsZero():
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM.Reveal())) {
			return nil, fmt.Errorf("failed to parse CA cert PEM material")
		}
		tlsCfg.RootCAs = pool
	case cfg.CACertFile != "":
		caCert, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert from %s", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}

	switch {
	case !cfg.CertPEM.IsZero() && !cfg.KeyPEM.IsZero():
		cert, err := tls.X509KeyPair([]byte(cfg.CertPEM.Reveal()), []byte(cfg.KeyPEM.Reveal()))
		if err != nil {
			return nil, fmt.Errorf("parse client certificate PEM: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	case !cfg.CertPEM.IsZero() || !cfg.KeyPEM.IsZero():
		return nil, fmt.Errorf("client certificate PEM requires both CertPEM and KeyPEM")
	case cfg.CertFile != "" && cfg.KeyFile != "":
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}

func (o *SessionOptions) applyDefaults() {
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = 30 * time.Second
	}
	if o.ReconnectDelay <= 0 {
		o.ReconnectDelay = 1 * time.Second
	}
	if o.ReconnectMaxDelay <= 0 {
		o.ReconnectMaxDelay = 30 * time.Second
	}
	if o.ReconnectMultiplier <= 0 {
		o.ReconnectMultiplier = 2.0
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = defaultIdleTimeout
	}
	if o.MaxFrameSize == 0 {
		o.MaxFrameSize = 65536
	}
	if o.LinkCloseTimeout <= 0 {
		o.LinkCloseTimeout = 5 * time.Second
	}
	if o.ConnectionMonitorFallback <= 0 {
		o.ConnectionMonitorFallback = 30 * time.Second
	}
	if o.ContainerID == "" {
		// Finding 16: an empty container-id must NOT fall through to the
		// SDK, which generates a NEW random container-id on every dial.
		// Brokers key durable subscriptions on container-id + link name,
		// so a per-dial identity orphans the subscription on every
		// reconnect; and replicas copying a static documented example
		// would collide. Generating "gobridge-<entropy>" ONCE here makes
		// the identity stable across reconnects for this session's
		// lifetime and unique per instance. It still changes across
		// process restarts, so a durable receiver (durability_mode > 0)
		// built through the factory without an explicit container_id is
		// REJECTED at build time; containerIDGenerated records
		// that this id is synthesised so the factory can enforce that gate.
		o.ContainerID = generateContainerID()
		o.containerIDGenerated = true
	}
	if o.Clock == nil {
		o.Clock = clock.System
	}
}

// defaultContainerIDPrefix prefixes generated AMQP container-ids so
// operators can recognize bridge connections without a configured
// container_id on the broker console.
const defaultContainerIDPrefix = "gobridge-"

// generateContainerID returns a per-instance AMQP container-id with
// crypto/rand entropy. Called once per SessionOptions defaulting, so
// the identity is stable for the session's lifetime (every reconnect
// re-dials with the same container-id) but unique per replica.
func generateContainerID() string {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp10: crypto/rand unavailable: " + err.Error())
	}
	return defaultContainerIDPrefix + hex.EncodeToString(b)
}

func (c *ReceiverConfig) validate() error {
	if c.Address == "" {
		return shared.ErrInvalidPayload.WithMessage("amqp10: receiver Address is required")
	}
	if c.LinkCredit > math.MaxInt32 {
		return shared.ErrInvalidPayload.WithMessage("amqp10: link_credit exceeds int32 max")
	}
	return nil
}

func (c *ReceiverConfig) applyDefaults() {
	if c.LinkCredit == 0 {
		c.LinkCredit = 10
	}
	if c.Metrics == nil {
		c.Metrics = &ports.NoopExporter{}
	}
	if c.Clock == nil {
		c.Clock = clock.System
	}
}

func (c *SenderConfig) validate() error {
	if c.Address == "" {
		return shared.ErrInvalidPayload.WithMessage("amqp10: sender Address is required")
	}
	return nil
}

func (c *SenderConfig) applyDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.Metrics == nil {
		c.Metrics = &ports.NoopExporter{}
	}
}

// SessionOptionsFromMap parses a SessionOptions from a generic options map.
func SessionOptionsFromMap(m map[string]any) (SessionOptions, error) {
	opts := DefaultSessionOptions()

	if v, ok := optString(m, "address"); ok {
		opts.Address = v
	}
	if v, ok := optDuration(m, "connect_timeout"); ok {
		opts.ConnectTimeout = v
	}
	if v, ok := optDuration(m, "reconnect_delay"); ok {
		opts.ReconnectDelay = v
	}
	if v, ok := optDuration(m, "reconnect_max_delay"); ok {
		opts.ReconnectMaxDelay = v
	}
	if v, ok := optFloat64(m, "reconnect_multiplier"); ok {
		opts.ReconnectMultiplier = v
	}
	if v, ok := optDuration(m, "idle_timeout"); ok {
		opts.IdleTimeout = v
	}
	if v, ok := optDuration(m, "link_close_timeout"); ok {
		opts.LinkCloseTimeout = v
	}
	if v, ok := optDuration(m, "connection_monitor_fallback"); ok {
		opts.ConnectionMonitorFallback = v
	}
	if v, ok := optUint32(m, "max_frame_size"); ok {
		opts.MaxFrameSize = v
	}
	if v, ok := optString(m, "username"); ok {
		opts.Username = v
	}
	if v, ok := optString(m, "password"); ok {
		opts.Password = shared.NewSecret(v)
	}
	if v, ok := optString(m, "container_id"); ok {
		opts.ContainerID = v
	}
	if v, ok := optString(m, "sasl_mechanism"); ok {
		opts.SASLMechanism = v
	}
	if v, ok := optBool(m, "allow_insecure_plain"); ok {
		opts.AllowInsecurePlain = v
	}

	if sub, ok := m["tls"].(map[string]any); ok {
		tc := &TLSConfig{}
		tc.Enable, _ = optBool(sub, "enable")
		tc.CACertFile, _ = optString(sub, "ca_cert_file")
		tc.CertFile, _ = optString(sub, "cert_file")
		tc.KeyFile, _ = optString(sub, "key_file")
		// In-memory PEM material (documented ca_cert_pem/cert_pem/key_pem)
		// was silently dropped by this map path, so programmatic callers
		// that passed PEM bytes ended up with no client cert / CA at all
		// (finding 8). Honor them here; BuildTLSConfig gives PEM precedence
		// over the file fields.
		if v, ok := optString(sub, "ca_cert_pem"); ok {
			tc.CACertPEM = shared.NewSecret(v)
		}
		if v, ok := optString(sub, "cert_pem"); ok {
			tc.CertPEM = shared.NewSecret(v)
		}
		if v, ok := optString(sub, "key_pem"); ok {
			tc.KeyPEM = shared.NewSecret(v)
		}
		tc.InsecureSkipVerify, _ = optBool(sub, "insecure_skip_verify")
		opts.TLS = tc
	}

	// No deferred-credentials path here: SessionOptionsFromMap has no
	// production callers and no credentials_uri resolution, so validate
	// strictly (credentialsPending=false).
	if err := opts.validate(false); err != nil {
		return opts, err
	}

	return opts, nil
}

// ReceiverConfigFromOptions extracts a ReceiverConfig from a generic
// options map.
func ReceiverConfigFromOptions(m map[string]any) ReceiverConfig {
	cfg := ReceiverConfig{}
	cfg.Address, _ = optString(m, "address")
	if v, ok := optUint32(m, "link_credit"); ok {
		cfg.LinkCredit = v
	}
	if v, ok := optUint32(m, "durability_mode"); ok {
		cfg.DurabilityMode = v
	}
	if v, ok := optString(m, "subscription_name"); ok {
		cfg.SubscriptionName = v
	}
	if raw, ok := m["routing"]; ok {
		if v, ok := parseRoutingType(raw); ok {
			cfg.Routing = v
		}
	}
	return cfg
}

// SenderConfigFromOptions extracts a SenderConfig from a generic
// options map.
func SenderConfigFromOptions(m map[string]any) SenderConfig {
	cfg := DefaultSenderOptions()
	if v, ok := optString(m, "address"); ok {
		cfg.Address = v
	}
	if v, ok := optDuration(m, "timeout"); ok {
		cfg.Timeout = v
	}
	if v, ok := optUint32(m, "durability_mode"); ok {
		cfg.DurabilityMode = v
	}
	if v, ok := optBool(m, "durable"); ok {
		cfg.Durable = &v
	}
	if raw, ok := m["routing"]; ok {
		if v, ok := parseRoutingType(raw); ok {
			cfg.Routing = v
		}
	}
	return cfg
}

func optString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func optBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func optUint32(m map[string]any, key string) (uint32, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		if n < 0 || n > math.MaxUint32 {
			return 0, false
		}
		return uint32(n), true
	case int32:
		if n < 0 {
			return 0, false
		}
		return uint32(n), true
	case int64:
		if n < 0 || n > math.MaxUint32 {
			return 0, false
		}
		return uint32(n), true
	case uint32:
		return n, true
	case float64:
		if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || n > math.MaxUint32 {
			return 0, false
		}
		return uint32(n), true
	default:
		return 0, false
	}
}

func optDuration(m map[string]any, key string) (time.Duration, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch d := v.(type) {
	case time.Duration:
		if d < 0 {
			return 0, false
		}
		return d, true
	case string:
		parsed, err := time.ParseDuration(d)
		if err != nil || parsed < 0 {
			return 0, false
		}
		return parsed, true
	case int:
		if d < 0 {
			return 0, false
		}
		return time.Duration(d) * time.Second, true
	case int64:
		if d < 0 {
			return 0, false
		}
		return time.Duration(d) * time.Second, true
	case float64:
		if d < 0 || math.IsNaN(d) || math.IsInf(d, 0) {
			return 0, false
		}
		return time.Duration(d * float64(time.Second)), true
	default:
		return 0, false
	}
}

func optFloat64(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
