package paho

import (
	"fmt"
	"math"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// SessionOptionsFromMap extracts SessionOptions from a generic options map.
// It returns an error if a provided value has an invalid type or range.
func SessionOptionsFromMap(m map[string]any) (SessionOptions, error) {
	opts := DefaultSessionOptions()
	if m == nil {
		return opts.normalizedIngressMemory(), nil
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
	if v, ok := optDuration(m, "reconcile_timeout"); ok {
		opts.ReconcileTimeout = v
	}
	if v, ok := optDuration(m, "reconnect_delay"); ok {
		opts.ReconnectDelay = v
	}
	if v, ok := optDuration(m, "reconnect_max_delay"); ok {
		opts.ReconnectMaxDelay = v
	}
	if v, ok := optDuration(m, "unmatched_grace"); ok {
		opts.UnmatchedGrace = v
	}
	if v, ok := m["clean_start"].(bool); ok {
		opts.CleanStart = v
	}
	if v, ok := m["no_local"].(bool); ok {
		opts.NoLocal = v
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
	if value, exists, err := optUint64(m, "receive_maximum", math.MaxUint16); err != nil {
		return opts, err
	} else if exists {
		opts.ReceiveMaximum = uint16(value)
		opts.receiveMaximumExplicit = value != 0
	}
	if value, exists, err := optUint64(m, "max_payload_bytes", math.MaxUint32); err != nil {
		return opts, err
	} else if exists {
		opts.MaxPayloadBytes = uint32(value)
	}
	if value, exists, err := optUint64(m, "ingress_memory_budget_bytes", math.MaxUint64); err != nil {
		return opts, err
	} else if exists {
		opts.IngressMemoryBudgetBytes = value
		opts.ingressMemoryBudgetExplicit = value != 0
	}
	if v, ok := m["username"].(string); ok {
		opts.Username = v
	}
	if v, ok := m["password"].(string); ok {
		opts.Password = shared.NewSecret(v)
	}
	if v, ok := m["allow_plaintext_credentials"].(bool); ok {
		opts.AllowPlaintextCredentials = v
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

	// HIGH-4: reject cleartext username/password over a non-TLS broker unless
	// explicitly opted in. Guarded internally by broker_urls being present.
	if err := opts.validatePlaintextCredentials(); err != nil {
		return opts, err
	}

	return opts.normalizedIngressMemory(), nil
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
	// In-memory PEM material (ca_cert_pem / cert_pem / key_pem). The typed
	// decode path and BuildTLSConfig fully support these and let them WIN over
	// the *_file keys, but this map path previously ignored them: a library
	// consumer passing PEM through the map silently got system roots and no
	// client certificate — an opaque auth failure (A-8). Parse them here so both
	// config paths behave identically. They are shared.Secret so key material
	// redacts on marshal/log.
	if v, ok := m["ca_cert_pem"].(string); ok && v != "" {
		cfg.CACertPEM = shared.NewSecret(v)
	}
	if v, ok := m["cert_pem"].(string); ok && v != "" {
		cfg.CertPEM = shared.NewSecret(v)
	}
	if v, ok := m["key_pem"].(string); ok && v != "" {
		cfg.KeyPEM = shared.NewSecret(v)
	}
	if v, ok := m["insecure_skip_verify"].(bool); ok {
		cfg.InsecureSkipVerify = v
	}
	return cfg
}

// optDuration extracts a duration from a hand-built options map for the
// SessionOptionsFromMap library-consumer path. An invalid or negative value
// returns (0, false) — the caller keeps the field's default — rather than an
// error: this map path is intentionally lenient for programmatic callers, and
// strict validation of malformed input lives on the registry/YAML surface
// (register.go's decoder rejects unknown or unparseable keys). L-1: the two
// public config surfaces differ in strictness by design; this leniency is the
// documented behaviour of the map path, not an oversight.
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

func optUint64(m map[string]any, key string, maximum uint64) (uint64, bool, error) {
	raw, exists := m[key]
	if !exists {
		return 0, false, nil
	}
	value, err := checkedConfigUint64(raw, key)
	if err != nil {
		return 0, false, err
	}
	if value > maximum {
		return 0, false, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("%s must be ≤ %d, got %d", key, maximum, value),
		)
	}
	return value, true, nil
}

func checkedConfigUint64(raw any, key string) (uint64, error) {
	switch number := raw.(type) {
	case int:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case int8:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case int16:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case int32:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case int64:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case uint:
		return uint64(number), nil
	case uint8:
		return uint64(number), nil
	case uint16:
		return uint64(number), nil
	case uint32:
		return uint64(number), nil
	case uint64:
		return number, nil
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || math.Trunc(number) != number {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be a finite non-negative integer, got %v", key, number),
			)
		}
		// float64(math.MaxUint64) rounds UP to 2^64, so comparing against it
		// would admit 2^64 and wrap on conversion. Use the exact exclusive
		// power-of-two boundary instead.
		if number >= math.Ldexp(1, 64) {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be less than 2^64, got %v", key, number),
			)
		}
		return uint64(number), nil
	default:
		return 0, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("%s must be a number, got %T", key, raw),
		)
	}
}
