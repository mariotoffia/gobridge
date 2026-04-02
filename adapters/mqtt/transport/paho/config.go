package paho

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
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
	DefaultTopic   string
	QoS            byte
	Retain         bool
	Timeout        time.Duration
	CircuitBreaker *CBConfig // nil = disabled (opt-in)
}

// TLSConfig holds TLS settings for the MQTT connection.
type TLSConfig struct {
	Enable             bool
	CACertFile         string
	CertFile           string
	KeyFile            string
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
		QoS:     1,
		Timeout: 30 * time.Second,
	}
}

// SessionOptionsFromMap extracts SessionOptions from a generic options map.
// It returns an error if a provided value has an invalid type or range.
func SessionOptionsFromMap(m map[string]any) (SessionOptions, error) {
	opts := DefaultSessionOptions()
	if m == nil {
		return opts, nil
	}

	if v, ok := m["broker_urls"].([]string); ok {
		opts.BrokerURLs = v
	}
	if v, ok := m["broker_url"].(string); ok && len(opts.BrokerURLs) == 0 {
		opts.BrokerURLs = []string{v}
	}
	if v, ok := m["client_id"].(string); ok {
		opts.ClientID = v
	}
	if v, ok := m["keep_alive"].(int); ok {
		if v < 0 || v > 65535 {
			return opts, fmt.Errorf("keep_alive must be 0..65535, got %d", v)
		}
		opts.KeepAlive = uint16(v)
	}
	if v, ok := m["connect_timeout"].(time.Duration); ok {
		opts.ConnectTimeout = v
	}
	if v, ok := m["reconnect_timeout"].(time.Duration); ok {
		opts.ReconnectTimeout = v
	}
	if v, ok := m["reconnect_delay"].(time.Duration); ok {
		opts.ReconnectDelay = v
	}
	if v, ok := m["clean_start"].(bool); ok {
		opts.CleanStart = v
	}
	if v, ok := m["session_expiry_interval"].(int); ok {
		opts.SessionExpiryInterval = uint32(v)
	}
	if v, ok := m["session_expiry_interval"].(uint32); ok {
		opts.SessionExpiryInterval = v
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
		v, ok := raw.(int)
		if !ok {
			return opts, fmt.Errorf("qos must be an int, got %T", raw)
		}
		if v < 0 || v > 2 {
			return opts, fmt.Errorf("qos must be 0, 1, or 2, got %d", v)
		}
		opts.QoS = byte(v)
	}
	if v, ok := m["retain"].(bool); ok {
		opts.Retain = v
	}
	if v, ok := m["timeout"].(time.Duration); ok {
		opts.Timeout = v
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
func BuildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	if cfg == nil {
		return nil, nil
	}

	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify,
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
