package paho

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"os"
	"time"
)

// SessionOptions holds MQTT connection and session configuration.
// These values are typically extracted from ports.SessionSpec.Options.
type SessionOptions struct {
	BrokerURLs            []string
	ClientID              string
	KeepAlive             uint16
	ConnectTimeout        time.Duration
	ReconnectTimeout      time.Duration
	CleanStart            bool
	SessionExpiryInterval uint32
	Username              string
	Password              string
	TLS                   *TLSConfig
	// ReceiveMaximum sets the MQTT v5 Receive Maximum property in the
	// CONNECT packet. This limits the number of QoS 1/2 messages the
	// broker can send before receiving PUBACKs. Default 0 means use the
	// paho library default (65535). Set higher for high-throughput scenarios.
	ReceiveMaximum uint16
	// ReconnectDelay is the constant delay between failed reconnection
	// attempts after the first immediate retry. Zero means use the
	// autopaho default (10s). Shorter values speed up reconnection in
	// test environments but increase load on the broker in production.
	ReconnectDelay time.Duration
}

// ReceiverOptions holds MQTT receiver-specific configuration.
type ReceiverOptions struct {
	// No additional fields; subscriptions come from ReceiverSpec.Subscriptions.
}

// SenderOptions holds MQTT sender-specific configuration.
type SenderOptions struct {
	DefaultTopic       string
	QoS                byte
	Retain             bool
	Timeout            time.Duration
	ThrottleRetryAfter time.Duration
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
	Enable     bool
	CACertFile string
	CertFile   string
	KeyFile    string

	// CACertPEM, CertPEM, KeyPEM carry in-memory PEM material, typically
	// populated by credential rotation. When any of these is non-empty
	// it takes precedence over the corresponding *File field.
	CACertPEM string
	CertPEM   string
	KeyPEM    string

	InsecureSkipVerify bool
}

// DefaultSessionOptions returns SessionOptions with recommended defaults.
func DefaultSessionOptions() SessionOptions {
	return SessionOptions{
		KeepAlive:        30,
		ConnectTimeout:   30 * time.Second,
		ReconnectTimeout: 30 * time.Second,
		CleanStart:       true,
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
		opts.Password = v
	}
	if v, ok := m["tls"].(*TLSConfig); ok {
		opts.TLS = v
	}
	if v, ok := m["tls"].(map[string]any); ok {
		opts.TLS = tlsConfigFromMap(v)
	}

	return opts, nil
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
