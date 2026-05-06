package amqp10

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

// SessionOptions configures the AMQP 1.0 session connection.
type SessionOptions struct {
	Address             string
	ConnectTimeout      time.Duration
	ReconnectDelay      time.Duration
	ReconnectMaxDelay   time.Duration
	ReconnectMultiplier float64
	IdleTimeout         time.Duration
	MaxFrameSize        uint32
	Username            string
	Password            string
	TLS                 *TLSConfig
	ContainerID         string

	// LinkCloseTimeout is the deadline for closing an AMQP link or
	// session during cleanup (e.g. after a failure or reconnect).
	// Defaults to 5s if zero.
	LinkCloseTimeout time.Duration

	// ConnectionMonitorFallback is the cadence at which the session
	// monitor loop re-checks connection liveness as a sanity fallback
	// when the underlying Conn.Done() signal is missed. Real disconnects
	// are detected immediately via Conn.Done(); this ticker only guards
	// against a broker that silently drops without closing. Defaults to
	// 30s if zero.
	ConnectionMonitorFallback time.Duration

	// Clock drives the reconnect backoff wait. When nil defaults to
	// clock.System (wall clock). Tests may inject a clocktest.Fake to
	// control the backoff sleep deterministically.
	Clock clock.Clock
}

// TLSConfig holds TLS settings for the AMQP 1.0 connection.
//
// PEM bytes (CACertPEM, CertPEM, KeyPEM) take precedence over file
// paths on the same field. This lets ApplyCredentials supply rotated
// material in-memory without writing to disk.
type TLSConfig struct {
	Enable     bool
	CACertFile string
	CertFile   string
	KeyFile    string

	// CACertPEM, CertPEM, KeyPEM carry in-memory PEM material used
	// when credentials are rotated via ApplyCredentials. Non-empty
	// fields override the corresponding *File fields.
	CACertPEM string
	CertPEM   string
	KeyPEM    string

	InsecureSkipVerify bool
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

// ReceiverConfig configures an AMQP 1.0 receiver link.
type ReceiverConfig struct {
	Address        string
	LinkCredit     uint32
	DurabilityMode uint32
	Routing        RoutingType
	Session        *Session
	Logger         *slog.Logger
	Metrics        ports.MetricsExporter

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
}

// DefaultSessionOptions returns SessionOptions with sensible defaults.
func DefaultSessionOptions() SessionOptions {
	return SessionOptions{
		ConnectTimeout:            30 * time.Second,
		ReconnectDelay:            1 * time.Second,
		ReconnectMaxDelay:         30 * time.Second,
		ReconnectMultiplier:       2.0,
		IdleTimeout:               2 * time.Minute,
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

func (o *SessionOptions) validate() error {
	if o.Address == "" {
		return shared.ErrInvalidPayload.WithMessage("amqp10: Address is required")
	}
	return nil
}

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
	case cfg.CACertPEM != "":
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
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
	case cfg.CertPEM != "" && cfg.KeyPEM != "":
		cert, err := tls.X509KeyPair([]byte(cfg.CertPEM), []byte(cfg.KeyPEM))
		if err != nil {
			return nil, fmt.Errorf("parse client certificate PEM: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	case cfg.CertPEM != "" || cfg.KeyPEM != "":
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
		o.IdleTimeout = 2 * time.Minute
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
	if o.Clock == nil {
		o.Clock = clock.System
	}
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
		opts.Password = v
	}
	if v, ok := optString(m, "container_id"); ok {
		opts.ContainerID = v
	}

	if sub, ok := m["tls"].(map[string]any); ok {
		tc := &TLSConfig{}
		tc.Enable, _ = optBool(sub, "enable")
		tc.CACertFile, _ = optString(sub, "ca_cert_file")
		tc.CertFile, _ = optString(sub, "cert_file")
		tc.KeyFile, _ = optString(sub, "key_file")
		tc.InsecureSkipVerify, _ = optBool(sub, "insecure_skip_verify")
		opts.TLS = tc
	}

	if err := opts.validate(); err != nil {
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
	if v, ok := optString(m, "routing"); ok && v == "multicast" {
		cfg.Routing = RoutingMulticast
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
	if v, ok := optString(m, "routing"); ok && v == "multicast" {
		cfg.Routing = RoutingMulticast
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
