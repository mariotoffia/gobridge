package servicebus

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
)

// These tests decode through the REAL production plugin-options decoder
// (parser.NewRawConfig(...).Decode) — the exact path the runtime registry
// uses to decode a transport's `options:` block into its typed Config
// (TagName "json", ErrorUnused, and the full hook chain incl.
// floatToIntegerOrDuration + the TextUnmarshaler hook for shared.Secret).
// Importing config/parser from an adapter test is allowed: go-arch-lint
// excludes *_test.go, and there is precedent (amqp10, native/config/file).

// TestPluginOptionsDecode_FullNestedYAML_Succeeds is the regression test
// for the CONFIG-DECODE finding: every documented connection key
// (connection_string, use_managed_identity, tenant_id, client_id,
// client_secret, ca_pem, client_cert_pem, client_key_pem,
// insecure_skip_verify) must decode into ConnectionConfig. Before the
// mapstructure/json tags were added, all of them failed under
// ErrorUnused — managed-identity and connection-string auth were
// unconfigurable from YAML.
func TestPluginOptionsDecode_FullNestedYAML_Succeeds(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"receiver": map[string]any{
			"queue_name":                "orders",
			"max_messages":              20,
			"max_wait_time":             "30s",
			"receive_mode":              "PeekLock",
			"sub_queue":                 "deadletter",
			"lock_duration":             "60s",
			"auto_extend":               true,
			"max_lock_renewal_duration": "10m",
		},
		"sender": map[string]any{
			"queue_name":         "orders-out",
			"default_session_id": "sess-1",
			"batch_size":         25,
			"timeout":            "5s",
		},
		"connection": map[string]any{
			"connection_string":    "Endpoint=sb://ns.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=v",
			"namespace":            "ns.servicebus.windows.net",
			"use_managed_identity": true,
			"tenant_id":            "tid",
			"client_id":            "cid",
			"client_secret":        "csec",
			"ca_pem":               "CA PEM",
			"client_cert_pem":      "CERT PEM",
			"client_key_pem":       "KEY PEM",
			"insecure_skip_verify": true,
		},
		"credentials_uri": "secrets://asb/orders",
	}

	var cfg Config
	require.NoError(t, parser.NewRawConfig(input).Decode(&cfg))

	require.Equal(t, "orders", cfg.Receiver.QueueName)
	require.Equal(t, 20, cfg.Receiver.MaxMessages)
	require.Equal(t, 30*time.Second, cfg.Receiver.MaxWaitTime)
	require.Equal(t, "PeekLock", cfg.Receiver.ReceiveMode)
	require.Equal(t, "deadletter", cfg.Receiver.SubQueue)
	require.Equal(t, time.Minute, cfg.Receiver.LockDuration)
	require.NotNil(t, cfg.Receiver.AutoExtend)
	require.True(t, *cfg.Receiver.AutoExtend)
	require.Equal(t, 10*time.Minute, cfg.Receiver.MaxLockRenewalDuration)

	require.Equal(t, "orders-out", cfg.Sender.QueueName)
	require.Equal(t, "sess-1", cfg.Sender.DefaultSessionID)
	require.Equal(t, 25, cfg.Sender.BatchSize)
	require.Equal(t, 5*time.Second, cfg.Sender.Timeout)

	// Secrets decode from scalar strings via the production
	// TextUnmarshaler hook; values are reachable only through Reveal.
	require.Equal(t,
		"Endpoint=sb://ns.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=v",
		cfg.Connection.ConnectionString.Reveal())
	require.Equal(t, "ns.servicebus.windows.net", cfg.Connection.Namespace)
	require.True(t, cfg.Connection.UseManagedIdentity)
	require.Equal(t, "tid", cfg.Connection.TenantID)
	require.Equal(t, "cid", cfg.Connection.ClientID)
	require.Equal(t, "csec", cfg.Connection.ClientSecret.Reveal())
	require.Equal(t, "CA PEM", cfg.Connection.CaPEM.Reveal())
	require.Equal(t, "CERT PEM", cfg.Connection.ClientCertPEM.Reveal())
	require.Equal(t, "KEY PEM", cfg.Connection.ClientKeyPEM.Reveal())
	require.True(t, cfg.Connection.InsecureSkipVerify)

	require.Equal(t, "secrets://asb/orders", cfg.CredentialsURIRef)

	// The decoded config must also pass the plugin validation gate.
	require.NoError(t, cfg.Validate())
}

// TestPluginOptionsDecode_UnknownConnectionKey_Errors proves the strict
// (ErrorUnused) contract survives the tagging: an undocumented nested
// connection key still fails the whole decode.
func TestPluginOptionsDecode_UnknownConnectionKey_Errors(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"receiver": map[string]any{"queue_name": "q"},
		"connection": map[string]any{
			"namespace": "ns",
			"bogus_key": "nope",
		},
	}

	var cfg Config
	err := parser.NewRawConfig(input).Decode(&cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus_key")
}

// The removed prefetch knob (a warn-and-ignore no-op — azservicebus
// v1.10.0 has no public prefetch option) must now be rejected as an
// unknown key instead of silently accepted.
func TestPluginOptionsDecode_PrefetchKey_Rejected(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"receiver": map[string]any{
			"queue_name": "q",
			"prefetch":   5,
		},
	}

	var cfg Config
	err := parser.NewRawConfig(input).Decode(&cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "prefetch")
}

// TestPluginOptionsDecode_RetryBlock_Succeeds is the config test:
// the SDK retry-policy surface (connection.retry) must decode through the
// strict production decoder. Without json tags on RetryConfig the nested
// block would fail under ErrorUnused, leaving the SDK retry layer
// untunable. A negative max_retries (disable the SDK layer) and duration
// strings must round-trip, and toSDK must then report the override.
func TestPluginOptionsDecode_RetryBlock_Succeeds(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"receiver": map[string]any{"queue_name": "orders"},
		"connection": map[string]any{
			"namespace": "ns.servicebus.windows.net",
			"retry": map[string]any{
				"max_retries":     -1,
				"retry_delay":     "2s",
				"max_retry_delay": "45s",
			},
		},
	}

	var cfg Config
	require.NoError(t, parser.NewRawConfig(input).Decode(&cfg))

	require.Equal(t, int32(-1), cfg.Connection.Retry.MaxRetries)
	require.Equal(t, 2*time.Second, cfg.Connection.Retry.RetryDelay)
	require.Equal(t, 45*time.Second, cfg.Connection.Retry.MaxRetryDelay)

	sdk, override := cfg.Connection.Retry.toSDK()
	require.True(t, override)
	require.Equal(t, int32(-1), sdk.MaxRetries)
	require.Equal(t, 2*time.Second, sdk.RetryDelay)
	require.Equal(t, 45*time.Second, sdk.MaxRetryDelay)
}

// An undocumented key inside connection.retry must still fail the strict
// decode — the retry block does not loosen the ErrorUnused contract.
func TestPluginOptionsDecode_UnknownRetryKey_Errors(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"receiver": map[string]any{"queue_name": "q"},
		"connection": map[string]any{
			"retry": map[string]any{
				"max_retries": 1,
				"bogus_retry": "nope",
			},
		},
	}

	var cfg Config
	err := parser.NewRawConfig(input).Decode(&cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus_retry")
}

// A bare YAML integer for max_wait_time decodes as NANOSECONDS through
// the current production hook chain (the hook guards floats only); the
// 1s validation floor must catch the resulting hot receive loop and
// point the user at duration strings. The floor is asserted directly on
// Config.Validate so this test stays correct regardless of how the
// root-config duration hook evolves.
func TestConfigValidate_MaxWaitTimeFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wait    time.Duration
		wantErr bool
	}{
		{"zero selects default", 0, false},
		{"1s floor ok", time.Second, false},
		{"30s ok", 30 * time.Second, false},
		{"bare int 30 decoded as 30ns rejected", 30 * time.Nanosecond, true},
		{"500ms hot loop rejected", 500 * time.Millisecond, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Receiver: ReceiverParams{QueueName: "q", MaxWaitTime: tt.wait}}
			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "max_wait_time")
				return
			}
			require.NoError(t, err)
		})
	}
}
