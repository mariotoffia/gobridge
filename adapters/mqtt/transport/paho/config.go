package paho

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// SessionOptions holds MQTT connection and session configuration.
// These values are typically extracted from ports.SessionSpec.Options.
type SessionOptions struct {
	BrokerURLs []string `mapstructure:"broker_urls" yaml:"broker_urls" json:"broker_urls"`
	// BrokerURL is the single-broker convenience form of broker_urls and the
	// dominant documented key (configuration-reference.md, scenarios 01-17).
	// normalizeBrokerURLs folds it into BrokerURLs when the list form is absent
	// and clears it, so the typed decode path honors the documented
	// single-broker config and re-serializes on the canonical broker_urls key.
	BrokerURL string `mapstructure:"broker_url" yaml:"broker_url,omitempty" json:"broker_url,omitempty"`
	ClientID  string `mapstructure:"client_id" yaml:"client_id" json:"client_id"`
	// KeepAlive is the MQTT keep-alive interval in seconds. The registry
	// decode path pre-fills the documented default (30) before decoding,
	// so an omitted key gets 30 while an EXPLICIT `keep_alive: 0`
	// (disable the pinger) is honoured. Hand-built SessionOptions carry
	// the caller's literal value unchanged.
	KeepAlive uint16 `mapstructure:"keep_alive" yaml:"keep_alive" json:"keep_alive"`
	// ConnectTimeout bounds the INITIAL Start connection await. Zero
	// falls back to 30s at Start time.
	ConnectTimeout time.Duration `mapstructure:"connect_timeout" yaml:"connect_timeout" json:"connect_timeout"`
	// ReconnectTimeout bounds each individual (re)connect attempt made
	// by the autopaho reconnection loop (TCP dial + TLS handshake + MQTT
	// CONNECT/CONNACK). Zero means the autopaho default (10s). It maps
	// to autopaho.ClientConfig.ConnectTimeout.
	ReconnectTimeout time.Duration `mapstructure:"reconnect_timeout" yaml:"reconnect_timeout" json:"reconnect_timeout"`
	// CleanStart is the MQTT 5 clean-start flag consulted for Persistent
	// and Exclusive sessions (Ephemeral sessions always connect with
	// CleanStart=true regardless). The default is FALSE: the modes that
	// consult the flag exist to RESUME broker session state, so a
	// clean-start default would silently discard it.
	CleanStart bool `mapstructure:"clean_start" yaml:"clean_start" json:"clean_start"`
	// SessionExpiryInterval is the MQTT 5 session expiry in seconds.
	// For Persistent/Exclusive sessions a zero value gives ZERO offline
	// retention (the broker drops the session state the moment the
	// connection closes), contradicting the mode's purpose — NewSession
	// therefore defaults zero to DefaultPersistentSessionExpiry for
	// those modes (with a warning). Ephemeral sessions always use 0.
	SessionExpiryInterval uint32        `mapstructure:"session_expiry_interval" yaml:"session_expiry_interval" json:"session_expiry_interval"`
	Username              string        `mapstructure:"username" yaml:"username" json:"username"`
	Password              shared.Secret `mapstructure:"password" yaml:"password" json:"password"`
	TLS                   *TLSConfig    `mapstructure:"tls" yaml:"tls" json:"tls"`
	// Will configures an MQTT Last Will and Testament published by the
	// broker when this session's connection terminates ungracefully,
	// letting peers detect ungraceful death. Optional; nil means no will.
	Will *WillOptions `mapstructure:"will" yaml:"will,omitempty" json:"will,omitempty"`
	// ReceiveMaximum sets the MQTT v5 Receive Maximum property in the
	// CONNECT packet. This limits the number of QoS 1/2 messages the
	// broker can send before receiving PUBACKs. Default 0 means use the
	// paho library default (65535). Set higher for high-throughput scenarios.
	ReceiveMaximum uint16 `mapstructure:"receive_maximum" yaml:"receive_maximum" json:"receive_maximum"`
	// ReconnectDelay is the constant delay between failed reconnection
	// attempts after the first immediate retry. Zero means use the
	// autopaho default (10s). Shorter values speed up reconnection in
	// test environments but increase load on the broker in production.
	ReconnectDelay time.Duration `mapstructure:"reconnect_delay" yaml:"reconnect_delay" json:"reconnect_delay"`
	// Clock is an internal dependency injected by the factory/tests and
	// must never be populated from YAML; the dash tag excludes it from
	// the strict options decoder (which would otherwise reject it).
	Clock clock.Clock `mapstructure:"-" yaml:"-" json:"-"`
}

// normalizeBrokerURLs folds the single-broker BrokerURL alias into the
// canonical BrokerURLs list when the list form is empty, then clears the alias
// so a re-serialized config carries only broker_urls. The registry decoder
// calls this after Decode and before Validate.
func (o *SessionOptions) normalizeBrokerURLs() {
	if o.BrokerURL != "" && len(o.BrokerURLs) == 0 {
		o.BrokerURLs = []string{o.BrokerURL}
	}
	o.BrokerURL = ""
}

// ReceiverOptions holds MQTT receiver-specific configuration.
type ReceiverOptions struct {
	// No additional fields; subscriptions come from ReceiverSpec.Subscriptions.
}

// WillOptions configures the MQTT Last Will and Testament (LWT) for a
// session. The broker publishes the will message on the configured
// topic when the client's connection terminates ungracefully (network
// partition, crash, keep-alive timeout) — peers subscribe to the will
// topic to detect ungraceful death. A graceful Close (normal
// DISCONNECT) does not trigger the will.
type WillOptions struct {
	Topic   string `mapstructure:"topic" yaml:"topic" json:"topic"`
	Payload string `mapstructure:"payload" yaml:"payload" json:"payload"`
	QoS     byte   `mapstructure:"qos" yaml:"qos" json:"qos"`
	Retain  bool   `mapstructure:"retain" yaml:"retain" json:"retain"`
}

// Validate checks the will configuration for protocol violations.
func (w *WillOptions) Validate() error {
	if w == nil {
		return nil
	}
	if w.Topic == "" {
		return fmt.Errorf("mqtt: will.topic is required when will is configured")
	}
	if strings.ContainsAny(w.Topic, "+#") {
		return fmt.Errorf("mqtt: will.topic %q must not contain wildcards", w.Topic)
	}
	if w.QoS > 2 {
		return fmt.Errorf("mqtt: will.qos must be 0, 1, or 2, got %d", w.QoS)
	}
	return nil
}

// SenderOptions holds MQTT sender-specific configuration.
type SenderOptions struct {
	DefaultTopic       string        `mapstructure:"default_topic" yaml:"default_topic" json:"default_topic"`
	QoS                byte          `mapstructure:"qos" yaml:"qos" json:"qos"`
	Retain             bool          `mapstructure:"retain" yaml:"retain" json:"retain"`
	Timeout            time.Duration `mapstructure:"timeout" yaml:"timeout" json:"timeout"`
	ThrottleRetryAfter time.Duration `mapstructure:"throttle_retry_after" yaml:"throttle_retry_after" json:"throttle_retry_after"`
}

// TLSConfig holds TLS settings for the MQTT connection.
//
// Two parallel sources of material are supported: file paths (loaded
// from disk at connect time) and PEM byte strings (used in-memory).
// PEM strings take precedence when both are set on the same field,
// because the push-credentials path delivers PEM material and a
// non-empty PEM should never be silently ignored in favour of a
// stale file on disk.
type TLSConfig struct {
	Enable     bool   `mapstructure:"enable" yaml:"enable" json:"enable"`
	CACertFile string `mapstructure:"ca_cert_file" yaml:"ca_cert_file" json:"ca_cert_file"`
	CertFile   string `mapstructure:"cert_file" yaml:"cert_file" json:"cert_file"`
	KeyFile    string `mapstructure:"key_file" yaml:"key_file" json:"key_file"`

	// CACertPEM, CertPEM, KeyPEM carry in-memory PEM material, typically
	// populated by credential rotation. When any of these is non-empty
	// it takes precedence over the corresponding *File field. They are
	// shared.Secret so the (sensitive) private-key material — and, for
	// uniformity, the cert/CA bundles — redact on JSON/YAML/log marshal;
	// the config-save path reveals explicitly (see shared.RevealSecrets).
	CACertPEM shared.Secret `mapstructure:"ca_cert_pem" yaml:"ca_cert_pem" json:"ca_cert_pem"`
	CertPEM   shared.Secret `mapstructure:"cert_pem" yaml:"cert_pem" json:"cert_pem"`
	KeyPEM    shared.Secret `mapstructure:"key_pem" yaml:"key_pem" json:"key_pem"`

	InsecureSkipVerify bool `mapstructure:"insecure_skip_verify" yaml:"insecure_skip_verify" json:"insecure_skip_verify"`
}

// DefaultPersistentSessionExpiry is the session_expiry_interval (in
// seconds) applied by NewSession when a Persistent or Exclusive session
// is configured with the zero value. 0 would give ZERO offline
// retention — the broker drops subscriptions and queued QoS 1/2
// messages the instant the connection closes — contradicting the
// documented purpose of those modes. 24 hours survives typical
// restarts and outages without leaking broker session state forever
// (0xFFFFFFFF "never expire" was deliberately rejected).
const DefaultPersistentSessionExpiry uint32 = 86400

// DefaultSessionOptions returns SessionOptions with recommended defaults.
//
// CleanStart defaults to FALSE: only Persistent/Exclusive sessions
// consult the flag (Ephemeral always connects clean), and those modes
// exist to resume broker session state.
func DefaultSessionOptions() SessionOptions {
	return SessionOptions{
		KeepAlive:        30,
		ConnectTimeout:   30 * time.Second,
		ReconnectTimeout: 30 * time.Second,
		CleanStart:       false,
		Clock:            clock.System,
	}
}

// DefaultSenderOptions returns SenderOptions with recommended defaults.
func DefaultSenderOptions() SenderOptions {
	return SenderOptions{
		QoS:                1,
		Timeout:            30 * time.Second,
		ThrottleRetryAfter: 500 * time.Millisecond,
	}
}

// DefaultConfig returns a Config pre-filled with every documented
// default. The registry decoder (register.go) decodes INTO this value:
// mapstructure only assigns fields present in the input map, so an
// omitted key keeps its default while an explicit value — including an
// explicit zero such as `qos: 0` or `keep_alive: 0` — overrides it.
// This is what makes "unset ⇒ default, explicit 0 ⇒ honoured"
// distinguishable without pointer-typed config fields.
func DefaultConfig() Config {
	session := DefaultSessionOptions()
	// Clock is an injected dependency, never config state; keep the
	// decoded Config free of it (NewSession installs clock.System).
	session.Clock = nil
	return Config{
		Session: session,
		Sender:  DefaultSenderOptions(),
	}
}

// SessionOptionsFromMap extracts SessionOptions from a generic options map.
// It returns an error if a provided value has an invalid type or range.
func SessionOptionsFromMap(m map[string]any) (SessionOptions, error) {
	opts := DefaultSessionOptions()
	if m == nil {
		return opts, nil
	}

	switch v := m["broker_urls"].(type) {
	case []string:
		opts.BrokerURLs = v
	case []any:
		urls := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				urls = append(urls, s)
			}
		}
		if len(urls) > 0 {
			opts.BrokerURLs = urls
		}
	}
	if v, ok := m["broker_url"].(string); ok && len(opts.BrokerURLs) == 0 {
		opts.BrokerURLs = []string{v}
	}
	if v, ok := m["client_id"].(string); ok {
		opts.ClientID = v
	}
	if raw, exists := m["keep_alive"]; exists {
		var v int
		switch n := raw.(type) {
		case int:
			v = n
		case int64:
			v = int(n)
		case float64:
			v = int(n)
		default:
			return opts, fmt.Errorf("keep_alive must be a number, got %T", raw)
		}
		if v < 0 || v > 65535 {
			return opts, fmt.Errorf("keep_alive must be 0..65535, got %d", v)
		}
		opts.KeepAlive = uint16(v)
	}
	if v, ok := optDuration(m, "connect_timeout"); ok {
		opts.ConnectTimeout = v
	}
	if v, ok := optDuration(m, "reconnect_timeout"); ok {
		opts.ReconnectTimeout = v
	}
	if v, ok := optDuration(m, "reconnect_delay"); ok {
		opts.ReconnectDelay = v
	}
	if v, ok := m["clean_start"].(bool); ok {
		opts.CleanStart = v
	}
	if raw, exists := m["session_expiry_interval"]; exists {
		// MQTT v5 SessionExpiryInterval is an unsigned 32-bit value
		// (seconds; 0xFFFFFFFF = "never expire"). Reject negative
		// inputs so a stray -1 is not silently coerced into "never
		// expire" via two's-complement wrap-around.
		var v int64
		switch n := raw.(type) {
		case int:
			v = int64(n)
		case int64:
			v = n
		case uint32:
			v = int64(n)
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return opts, fmt.Errorf("session_expiry_interval must be a finite number, got %v", n)
			}
			v = int64(n)
		default:
			return opts, fmt.Errorf("session_expiry_interval must be a number, got %T", raw)
		}
		if v < 0 {
			return opts, fmt.Errorf("session_expiry_interval must be ≥ 0, got %d", v)
		}
		if v > math.MaxUint32 {
			return opts, fmt.Errorf("session_expiry_interval must be ≤ %d, got %d", uint32(math.MaxUint32), v)
		}
		opts.SessionExpiryInterval = uint32(v)
	}
	if v, ok := m["username"].(string); ok {
		opts.Username = v
	}
	if v, ok := m["password"].(string); ok {
		opts.Password = shared.NewSecret(v)
	}
	if v, ok := m["tls"].(*TLSConfig); ok {
		opts.TLS = v
	}
	if v, ok := m["tls"].(map[string]any); ok {
		opts.TLS = tlsConfigFromMap(v)
	}
	if v, ok := m["will"].(*WillOptions); ok {
		opts.Will = v
	}
	if v, ok := m["will"].(map[string]any); ok {
		will, err := willOptionsFromMap(v)
		if err != nil {
			return opts, err
		}
		opts.Will = will
	}

	return opts, nil
}

// willOptionsFromMap extracts WillOptions from a generic options map.
func willOptionsFromMap(m map[string]any) (*WillOptions, error) {
	w := &WillOptions{}
	if v, ok := m["topic"].(string); ok {
		w.Topic = v
	}
	if v, ok := m["payload"].(string); ok {
		w.Payload = v
	}
	if raw, exists := m["qos"]; exists {
		var v int
		switch n := raw.(type) {
		case int:
			v = n
		case int64:
			v = int(n)
		case float64:
			v = int(n)
		default:
			return nil, fmt.Errorf("will.qos must be a number, got %T", raw)
		}
		if v < 0 || v > 2 {
			return nil, fmt.Errorf("will.qos must be 0, 1, or 2, got %d", v)
		}
		w.QoS = byte(v)
	}
	if v, ok := m["retain"].(bool); ok {
		w.Retain = v
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// SenderOptionsFromMap extracts SenderOptions from a generic options map.
// It returns an error if a provided value has an invalid type or range.
func SenderOptionsFromMap(m map[string]any) (SenderOptions, error) {
	opts := DefaultSenderOptions()
	if m == nil {
		return opts, nil
	}

	if v, ok := m["default_topic"].(string); ok {
		opts.DefaultTopic = v
	}
	if raw, exists := m["qos"]; exists {
		var v int
		switch n := raw.(type) {
		case int:
			v = n
		case int64:
			v = int(n)
		case float64:
			v = int(n)
		default:
			return opts, fmt.Errorf("qos must be a number, got %T", raw)
		}
		if v < 0 || v > 2 {
			return opts, fmt.Errorf("qos must be 0, 1, or 2, got %d", v)
		}
		opts.QoS = byte(v)
	}
	if v, ok := m["retain"].(bool); ok {
		opts.Retain = v
	}
	if v, ok := optDuration(m, "timeout"); ok {
		opts.Timeout = v
	}
	if v, ok := optDuration(m, "throttle_retry_after"); ok {
		opts.ThrottleRetryAfter = v
	}

	return opts, nil
}

// ReceiverOptionsFromMap extracts ReceiverOptions from a generic options map.
func ReceiverOptionsFromMap(m map[string]any) ReceiverOptions {
	return ReceiverOptions{}
}

func tlsConfigFromMap(m map[string]any) *TLSConfig {
	cfg := &TLSConfig{}
	if v, ok := m["enable"].(bool); ok {
		cfg.Enable = v
	}
	if v, ok := m["ca_cert_file"].(string); ok {
		cfg.CACertFile = v
	}
	if v, ok := m["cert_file"].(string); ok {
		cfg.CertFile = v
	}
	if v, ok := m["key_file"].(string); ok {
		cfg.KeyFile = v
	}
	if v, ok := m["insecure_skip_verify"].(bool); ok {
		cfg.InsecureSkipVerify = v
	}
	return cfg
}

// BuildTLSConfig creates a *tls.Config from TLSConfig.
//
// Material source dispatch:
//   - CA: CACertPEM wins over CACertFile when non-empty.
//   - Client cert/key: both CertPEM+KeyPEM present win over CertFile+KeyFile.
//     Partial PEM pairs (only one of CertPEM/KeyPEM) are rejected to avoid
//     a silent "loaded the file pair" fallback that hides a rotation bug.
func BuildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	if cfg == nil {
		return nil, nil
	}

	tlsCfg := &tls.Config{
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
