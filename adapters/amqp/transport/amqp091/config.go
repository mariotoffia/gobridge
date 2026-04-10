package amqp091

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// SessionOptions holds AMQP 0-9-1 connection and session configuration.
// These values are typically extracted from ports.SessionSpec.Options.
type SessionOptions struct {
	BrokerURL      string
	Heartbeat      time.Duration
	ConnectTimeout time.Duration
	ReconnectDelay time.Duration
	Username       string
	Password       string
	TLS            *TLSConfig
	Vhost          string
}

// TLSConfig holds TLS settings for the AMQP connection.
type TLSConfig struct {
	Enable             bool
	CACertFile         string
	CertFile           string
	KeyFile            string
	InsecureSkipVerify bool
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
}

// SenderConfig holds AMQP 0-9-1 sender-specific configuration.
type SenderConfig struct {
	Exchange   string
	RoutingKey string
	Mandatory  bool
	Immediate  bool
	Timeout    time.Duration
	Session    *Session
	Logger     *slog.Logger
	Metrics    ports.MetricsExporter
}

// DefaultSessionOptions returns SessionOptions with recommended defaults.
func DefaultSessionOptions() SessionOptions {
	return SessionOptions{
		Heartbeat:      10 * time.Second,
		ConnectTimeout: 30 * time.Second,
		ReconnectDelay: 1 * time.Second,
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
		opts.Password = v
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
		PrefetchCount: 10,
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
	if v, ok := optInt(m, "prefetch_count"); ok {
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
	if v, ok := optDuration(m, "timeout"); ok {
		cfg.Timeout = v
	}

	return cfg
}

func (o SessionOptions) validate() error {
	if o.BrokerURL == "" {
		return fmt.Errorf("broker_url is required")
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
}

func (c SenderConfig) validate() error {
	return nil
}

func (c *SenderConfig) applyDefaults() {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
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

	if cfg.CACertFile != "" {
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

	if cfg.CertFile != "" && cfg.KeyFile != "" {
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
