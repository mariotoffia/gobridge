package amqp091

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// SessionOptions holds AMQP 0-9-1 connection and session configuration.
// These values are typically extracted from ports.SessionSpec.Options.
type SessionOptions struct {
	BrokerURL           string        `mapstructure:"broker_url" yaml:"broker_url" json:"broker_url"`
	Heartbeat           time.Duration `mapstructure:"heartbeat" yaml:"heartbeat" json:"heartbeat"`
	ConnectTimeout      time.Duration `mapstructure:"connect_timeout" yaml:"connect_timeout" json:"connect_timeout"`
	ReconnectDelay      time.Duration `mapstructure:"reconnect_delay" yaml:"reconnect_delay" json:"reconnect_delay"`
	ReconnectMaxDelay   time.Duration `mapstructure:"reconnect_max_delay" yaml:"reconnect_max_delay" json:"reconnect_max_delay"`
	ReconnectMultiplier float64       `mapstructure:"reconnect_multiplier" yaml:"reconnect_multiplier" json:"reconnect_multiplier"`
	Username            string        `mapstructure:"username" yaml:"username" json:"username"`
	Password            shared.Secret `mapstructure:"password" yaml:"password" json:"password"`
	TLS                 *TLSConfig    `mapstructure:"tls" yaml:"tls" json:"tls"`
	Vhost               string        `mapstructure:"vhost" yaml:"vhost" json:"vhost"`

	// Clock drives the reconnect backoff wait. When nil defaults to
	// clock.System (wall clock). Tests may inject a clocktest.Fake to
	// control the backoff sleep deterministically. It is an internal
	// dependency and must never be populated from YAML; the dash tag
	// excludes it from the strict options decoder.
	Clock clock.Clock `mapstructure:"-" yaml:"-" json:"-"`
}

// TLSConfig holds TLS settings for the AMQP connection.
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
	// when credentials are rotated via ApplyCredentials. When set
	// they override the corresponding *File fields. They are
	// shared.Secret so the (sensitive) private-key material — and, for
	// uniformity, the cert/CA bundles — redact on JSON/YAML/log marshal;
	// the config-save path reveals explicitly (see shared.RevealSecrets).
	CACertPEM shared.Secret `mapstructure:"ca_cert_pem" yaml:"ca_cert_pem" json:"ca_cert_pem"`
	CertPEM   shared.Secret `mapstructure:"cert_pem" yaml:"cert_pem" json:"cert_pem"`
	KeyPEM    shared.Secret `mapstructure:"key_pem" yaml:"key_pem" json:"key_pem"`

	InsecureSkipVerify bool `mapstructure:"insecure_skip_verify" yaml:"insecure_skip_verify" json:"insecure_skip_verify"`
}

// ReceiverConfig holds AMQP 0-9-1 receiver-specific configuration.
type ReceiverConfig struct {
	QueueName     string
	ConsumerTag   string
	AutoAck       bool
	Exclusive     bool
	PrefetchCount int
	PrefetchSize  int
	Session       *Session
	Logger        *slog.Logger
	Metrics       ports.MetricsExporter
	Clock         clock.Clock
}

// Delivery-mode knob values for SenderConfig.DeliveryMode /
// SenderParams.DeliveryMode. The words mirror the SessionMode glossary
// entry in UBIQUITOUS.md ("persistent" = survives broker restart).
const (
	// DeliveryModePersistent marks published messages persistent
	// (AMQP delivery-mode 2): a durable queue writes them to disk, so
	// they survive a broker restart. This is the default — the bridge
	// acks the source once the destination broker confirms, and a
	// confirm for a transient message only means "in memory".
	DeliveryModePersistent = "persistent"
	// DeliveryModeTransient marks published messages transient (AMQP
	// delivery-mode 1): they are lost when the broker restarts, even
	// on a durable classic queue. Opt-in for throughput-over-safety
	// routes only.
	DeliveryModeTransient = "transient"
)

// SenderConfig holds AMQP 0-9-1 sender-specific configuration.
type SenderConfig struct {
	Exchange   string
	RoutingKey string
	Mandatory  bool
	// Immediate is retained only so existing configs that set it still
	// decode. It is ignored by the publish path and rejected by
	// Config.Validate/the managed factory: RabbitMQ removed basic.publish
	// "immediate" in 3.0 and closes the channel when it is set.
	//
	// Deprecated: unsupported by RabbitMQ; has no effect.
	Immediate bool
	// DeliveryMode selects the default AMQP delivery mode for every
	// publish: DeliveryModePersistent (default) or DeliveryModeTransient.
	// A per-message "amqp091.delivery-mode" envelope header overrides it
	// (see envelopeToPublishing). Quorum queues always persist regardless
	// of this knob; it matters for durable classic queues.
	DeliveryMode string
	Timeout      time.Duration
	Session      *Session
	Logger       *slog.Logger
	Metrics      ports.MetricsExporter
	Clock        clock.Clock
}

// DefaultSessionOptions returns SessionOptions with recommended defaults.
func DefaultSessionOptions() SessionOptions {
	return SessionOptions{
		Heartbeat:           10 * time.Second,
		ConnectTimeout:      30 * time.Second,
		ReconnectDelay:      1 * time.Second,
		ReconnectMaxDelay:   30 * time.Second,
		ReconnectMultiplier: 2.0,
	}
}

// DefaultSenderOptions returns SenderConfig with recommended defaults.
func DefaultSenderOptions() SenderConfig {
	return SenderConfig{
		Timeout: 30 * time.Second,
	}
}

// SessionOptionsFromMap extracts SessionOptions from a generic options map.
func SessionOptionsFromMap(m map[string]any) (SessionOptions, error) {
	opts := DefaultSessionOptions()
	if m == nil {
		if err := opts.validate(); err != nil {
			return opts, err
		}
		return opts, nil
	}

	if v, ok := optString(m, "broker_url"); ok {
		opts.BrokerURL = v
	}
	if v, ok := optString(m, "username"); ok {
		opts.Username = v
	}
	if v, ok := optString(m, "password"); ok {
		opts.Password = shared.NewSecret(v)
	}
	if v, ok := optString(m, "vhost"); ok {
		opts.Vhost = v
	}
	if v, ok := optDuration(m, "heartbeat"); ok {
		opts.Heartbeat = v
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

	if v, ok := m["tls"].(*TLSConfig); ok {
		opts.TLS = v
	}
	if v, ok := m["tls"].(map[string]any); ok {
		opts.TLS = tlsConfigFromMap(v)
	}

	if err := opts.validate(); err != nil {
		return opts, err
	}

	return opts, nil
}

// ReceiverConfigFromOptions extracts ReceiverConfig from a generic options map.
func ReceiverConfigFromOptions(m map[string]any) ReceiverConfig {
	cfg := ReceiverConfig{
		PrefetchCount: defaultPrefetchCount,
	}
	if m == nil {
		return cfg
	}

	if v, ok := optString(m, "queue_name"); ok {
		cfg.QueueName = v
	}
	if v, ok := optString(m, "consumer_tag"); ok {
		cfg.ConsumerTag = v
	}
	if v, ok := optBool(m, "auto_ack"); ok {
		cfg.AutoAck = v
	}
	if v, ok := optBool(m, "exclusive"); ok {
		cfg.Exclusive = v
	}
	if v, ok := optInt(m, "prefetch_count"); ok && v != 0 {
		// Mirror the typed path (ReceiverParams.applyDefaults): an explicit
		// prefetch_count:0 means the bounded default, not "unlimited" — an
		// unbounded window lets the broker hand the whole queue to one
		// manual-settlement consumer, defeating fair dispatch/backpressure.
		cfg.PrefetchCount = v
	}
	if v, ok := optInt(m, "prefetch_size"); ok {
		cfg.PrefetchSize = v
	}

	return cfg
}

// SenderConfigFromOptions extracts SenderConfig from a generic options map.
func SenderConfigFromOptions(m map[string]any) SenderConfig {
	cfg := DefaultSenderOptions()
	if m == nil {
		return cfg
	}

	if v, ok := optString(m, "exchange"); ok {
		cfg.Exchange = v
	}
	if v, ok := optString(m, "routing_key"); ok {
		cfg.RoutingKey = v
	}
	if v, ok := optBool(m, "mandatory"); ok {
		cfg.Mandatory = v
	}
	if v, ok := optBool(m, "immediate"); ok {
		cfg.Immediate = v
	}
	if v, ok := optString(m, "delivery_mode"); ok {
		cfg.DeliveryMode = v
	}
	if v, ok := optDuration(m, "timeout"); ok {
		cfg.Timeout = v
	}

	return cfg
}

func (o SessionOptions) validate() error {
	if o.BrokerURL == "" {
		return fmt.Errorf("broker_url is required")
	}
	return o.validateDurations()
}

// minConfigDuration is the smallest accepted non-zero configured
// duration. A YAML/JSON decoder that bypasses the bridge's strict root
// parser (direct yaml.Unmarshal into Config, a programmatic spec, an
// embedder's own decode) turns a bare number into NANOSECONDS when the
// target is time.Duration: `heartbeat: 30` becomes 30ns. No duration
// knob in this adapter has a legitimate sub-millisecond value, so
// anything non-zero below this floor is treated as that decode accident
// and rejected with a message naming the key and the unit requirement.
const minConfigDuration = time.Millisecond

// validateDurationFloor rejects a non-zero duration below
// minConfigDuration (which also covers negative values). Zero is
// allowed and means "use the default" (see applyDefaults).
func validateDurationFloor(key string, d time.Duration) error {
	if d == 0 || d >= minConfigDuration {
		return nil
	}
	return fmt.Errorf(
		"%s is %v: a configured duration must be zero (use the default) or at least 1ms — "+
			"a bare YAML/JSON number decodes as nanoseconds, so write a duration string "+
			"with a unit such as \"30s\"",
		key, d)
}

// validateDurations applies the sub-millisecond floor to every duration
// knob on the session block. Split from validate() so Config.Validate
// can run it even when the block carries no broker_url (e.g. a binding
// override that only tunes timings).
func (o SessionOptions) validateDurations() error {
	for _, f := range []struct {
		key string
		d   time.Duration
	}{
		{"session.heartbeat", o.Heartbeat},
		{"session.connect_timeout", o.ConnectTimeout},
		{"session.reconnect_delay", o.ReconnectDelay},
		{"session.reconnect_max_delay", o.ReconnectMaxDelay},
	} {
		if err := validateDurationFloor(f.key, f.d); err != nil {
			return err
		}
	}
	return nil
}

func (o *SessionOptions) applyDefaults() {
	if o.Heartbeat == 0 {
		o.Heartbeat = 10 * time.Second
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 30 * time.Second
	}
	if o.ReconnectDelay == 0 {
		o.ReconnectDelay = 1 * time.Second
	}
	if o.ReconnectMaxDelay <= 0 {
		o.ReconnectMaxDelay = 30 * time.Second
	}
	if o.ReconnectMultiplier <= 0 {
		o.ReconnectMultiplier = 2.0
	}
	if o.Clock == nil {
		o.Clock = clock.System
	}
}

func (c SenderConfig) validate() error {
	if err := validateDeliveryMode(c.DeliveryMode); err != nil {
		return err
	}
	return validateDurationFloor("sender.timeout", c.Timeout)
}

// validateDeliveryMode accepts the empty string (defaulted to persistent)
// and the two canonical knob values.
func validateDeliveryMode(mode string) error {
	switch mode {
	case "", DeliveryModePersistent, DeliveryModeTransient:
		return nil
	default:
		return fmt.Errorf(
			"delivery_mode %q is invalid: must be %q or %q (default %q)",
			mode, DeliveryModePersistent, DeliveryModeTransient, DeliveryModePersistent)
	}
}

func (c *SenderConfig) applyDefaults() {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.DeliveryMode == "" {
		c.DeliveryMode = DeliveryModePersistent
	}
	if c.Clock == nil {
		c.Clock = clock.System
	}
}

// BuildTLSConfig creates a *tls.Config from TLSConfig.
// Returns nil if cfg is nil or TLS is not enabled.
func BuildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	if cfg == nil || !cfg.Enable {
		return nil, nil
	}

	if cfg.InsecureSkipVerify {
		slog.Warn("amqp091: TLS certificate verification disabled — not recommended for production")
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

func tlsConfigFromMap(m map[string]any) *TLSConfig {
	cfg := &TLSConfig{}
	if v, ok := optBool(m, "enable"); ok {
		cfg.Enable = v
	}
	if v, ok := optString(m, "ca_cert_file"); ok {
		cfg.CACertFile = v
	}
	if v, ok := optString(m, "cert_file"); ok {
		cfg.CertFile = v
	}
	if v, ok := optString(m, "key_file"); ok {
		cfg.KeyFile = v
	}
	if v, ok := optBool(m, "insecure_skip_verify"); ok {
		cfg.InsecureSkipVerify = v
	}
	return cfg
}

func optString(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	return v, ok
}

func optBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key].(bool)
	return v, ok
}

func optInt(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		if n > math.MaxInt || n < math.MinInt {
			return 0, false
		}
		return int(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n > float64(math.MaxInt) || n < float64(math.MinInt) {
			return 0, false
		}
		return int(n), true
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
