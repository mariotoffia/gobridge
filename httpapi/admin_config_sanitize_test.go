package httpapi

import (
	"testing"

	"github.com/mariotoffia/gobridge/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeConfig_RedactsAPIKeys(t *testing.T) {
	cfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test"},
		HTTP: &config.HTTPConfig{
			AdminAPIKey:   "secret-admin-key",
			MonitorAPIKey: "secret-monitor-key",
		},
	}

	sanitized := sanitizeConfig(cfg)

	assert.Equal(t, "***", sanitized.HTTP.AdminAPIKey)
	assert.Equal(t, "***", sanitized.HTTP.MonitorAPIKey)
	// Original must not be modified.
	assert.Equal(t, "secret-admin-key", cfg.HTTP.AdminAPIKey)
}

func TestSanitizeConfig_NilHTTP(t *testing.T) {
	cfg := &config.BridgeConfig{Bridge: config.BridgeSettings{ID: "test"}}
	sanitized := sanitizeConfig(cfg)
	assert.Nil(t, sanitized.HTTP)
}

func TestSanitizeConfig_EmptyKeys(t *testing.T) {
	cfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test"},
		HTTP:   &config.HTTPConfig{},
	}

	sanitized := sanitizeConfig(cfg)
	assert.Equal(t, "", sanitized.HTTP.AdminAPIKey)
	assert.Equal(t, "", sanitized.HTTP.MonitorAPIKey)
}

func TestSanitizeConfig_RedactsTransportEndpointAPIKeys(t *testing.T) {
	cfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test"},
		Receivers: []config.ReceiverDef{
			{
				ID:        "rx-http",
				Transport: "http",
				Options: map[string]any{
					"path":    "/transport/http/receivers/rx-http/messages",
					"api_key": "receiver-secret",
				},
			},
		},
		Senders: []config.SenderDef{
			{
				ID:        "tx-http",
				Transport: "http",
				Options: map[string]any{
					"path":    "/transport/http/senders/tx-http/events",
					"api_key": "sender-secret",
				},
			},
		},
	}

	sanitized := sanitizeConfig(cfg)

	require.Len(t, sanitized.Receivers, 1)
	require.Len(t, sanitized.Senders, 1)
	assert.Equal(t, "***", sanitized.Receivers[0].Options["api_key"])
	assert.Equal(t, "***", sanitized.Senders[0].Options["api_key"])
	// Non-secret options must be preserved.
	assert.Equal(t, "/transport/http/receivers/rx-http/messages", sanitized.Receivers[0].Options["path"])
	assert.Equal(t, "/transport/http/senders/tx-http/events", sanitized.Senders[0].Options["path"])
	// Original must not be modified.
	assert.Equal(t, "receiver-secret", cfg.Receivers[0].Options["api_key"])
	assert.Equal(t, "sender-secret", cfg.Senders[0].Options["api_key"])
}

func TestRedactAPIKeyOption_EdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		options   map[string]any
		wantOut   map[string]any
		wantDirty bool
	}{
		{
			name:      "nil options",
			options:   nil,
			wantOut:   nil,
			wantDirty: false,
		},
		{
			name:      "empty options",
			options:   map[string]any{},
			wantOut:   nil,
			wantDirty: false,
		},
		{
			name:      "no api_key key",
			options:   map[string]any{"path": "/foo"},
			wantOut:   nil,
			wantDirty: false,
		},
		{
			name:      "api_key is empty string",
			options:   map[string]any{"api_key": ""},
			wantOut:   nil,
			wantDirty: false,
		},
		{
			name:      "api_key is non-string type",
			options:   map[string]any{"api_key": 42},
			wantOut:   nil,
			wantDirty: false,
		},
		{
			name:      "api_key is valid string",
			options:   map[string]any{"api_key": "secret", "path": "/x"},
			wantOut:   map[string]any{"api_key": "***", "path": "/x"},
			wantDirty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dirty := redactAPIKeyOption(tt.options)
			assert.Equal(t, tt.wantDirty, dirty)
			assert.Equal(t, tt.wantOut, got)
		})
	}
}

func TestSanitizeConfig_MixedReceiversWithAndWithoutAPIKey(t *testing.T) {
	cfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test"},
		Receivers: []config.ReceiverDef{
			{ID: "rx-mqtt", Transport: "mqtt", Options: map[string]any{"topic": "#"}},
			{ID: "rx-http", Transport: "http", Options: map[string]any{"api_key": "secret"}},
			{ID: "rx-sqs", Transport: "sqs"},
		},
	}

	sanitized := sanitizeConfig(cfg)

	require.Len(t, sanitized.Receivers, 3)
	assert.Equal(t, map[string]any{"topic": "#"}, sanitized.Receivers[0].Options)
	assert.Equal(t, "***", sanitized.Receivers[1].Options["api_key"])
	assert.Nil(t, sanitized.Receivers[2].Options)
}

func TestSanitizeConfig_EmptyReceiverAndSenderSlices(t *testing.T) {
	cfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test"},
	}
	sanitized := sanitizeConfig(cfg)
	assert.Nil(t, sanitized.Receivers)
	assert.Nil(t, sanitized.Senders)
}

func TestSanitizeConfig_NilConfig(t *testing.T) {
	assert.Nil(t, sanitizeConfig(nil))
}
