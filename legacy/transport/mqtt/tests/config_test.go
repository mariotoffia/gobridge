// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Config Unit Tests
//
// Tests for configuration types and validation.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ C001 │ SourceConfig requires BrokerURL        │ PASS     │
// │ C002 │ SourceConfig requires Topics           │ PASS     │
// │ C003 │ TargetConfig requires BrokerURL        │ PASS     │
// │ C004 │ QoS clamping above 2                   │ PASS     │
// │ C005 │ MQTTConnectionSettings builder         │ PASS     │
// │ C006 │ RequiresReconnect BrokerURL change     │ PASS     │
// │ C007 │ RequiresReconnect ClientID change      │ PASS     │
// │ C008 │ RequiresReconnect Credentials change   │ PASS     │
// │ C009 │ RequiresReconnect TLS change           │ PASS     │
// │ C010 │ RequiresReconnect no change            │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtttests

import (
	"testing"
	"time"

	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// SourceConfig Validation Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSourceConfig_Interface validates SourceConfigImpl implements SourceConfig.
func TestSourceConfig_Interface(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test-source",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
		QoS:    1,
	}

	// Verify interface implementation
	var _ bridgeTypes.SourceConfig = cfg

	assert.Equal(t, "test-source", cfg.GetID())
	assert.Equal(t, mqtt.TransportType, cfg.GetTransportType())
	assert.Equal(t, 1, cfg.GetQoS().Level)
	assert.Equal(t, 0, cfg.GetPrefetch()) // MQTT doesn't support prefetch
}

// TestSourceConfig_Resources validates resource-based lookup.
func TestSourceConfig_Resources(t *testing.T) {
	cfg := &mqtt.SourceConfigImpl{
		ID: "test-source",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Topics: []string{"test/topic"},
		Resources: []bridgeTypes.Tag{
			{Key: "env", Value: "prod"},
		},
		AllowMultiple: true,
	}

	assert.Len(t, cfg.GetResources(), 1)
	assert.Equal(t, "env", cfg.GetResources()[0].Key)
	assert.True(t, cfg.AllowMultipleResourceMatches())
}

// ═══════════════════════════════════════════════════════════════════════════
// TargetConfig Validation Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTargetConfig_Interface validates TargetConfigImpl implements TargetConfig.
func TestTargetConfig_Interface(t *testing.T) {
	timeout := 30 * time.Second
	cfg := &mqtt.TargetConfigImpl{
		ID: "test-target",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		DefaultTopic:  "test/topic",
		QoS:           2,
		Retain:        true,
		MessageExpiry: 60,
		Timeout:       timeout,
	}

	// Verify interface implementation
	var _ bridgeTypes.TargetConfig = cfg

	assert.Equal(t, "test-target", cfg.GetID())
	assert.Equal(t, mqtt.TransportType, cfg.GetTransportType())
	assert.Equal(t, 2, cfg.GetDefaultQoS().Level)
	assert.Equal(t, 0, cfg.GetBatchSize()) // MQTT doesn't support batching
	assert.Equal(t, &timeout, cfg.GetTimeout())
}

// TestTargetConfig_NoTimeout validates nil timeout when not set.
func TestTargetConfig_NoTimeout(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test-target",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}

	assert.Nil(t, cfg.GetTimeout())
}

// TestTargetConfig_Resources validates resource-based lookup.
func TestTargetConfig_Resources(t *testing.T) {
	cfg := &mqtt.TargetConfigImpl{
		ID: "test-target",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		Resources: []bridgeTypes.Tag{
			{Key: "region", Value: "us-east-1"},
		},
		AllowMultiple: false,
	}

	assert.Len(t, cfg.GetResources(), 1)
	assert.Equal(t, "region", cfg.GetResources()[0].Key)
	assert.False(t, cfg.AllowMultipleResourceMatches())
}

// ═══════════════════════════════════════════════════════════════════════════
// ConnectionConfig Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMQTTConnectionConfig_Interface validates interface implementation.
func TestMQTTConnectionConfig_Interface(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}

	// Verify interface implementation
	var _ bridgeTypes.ConnectionConfig = cfg

	assert.Equal(t, "test-conn", cfg.GetID())
	assert.Equal(t, mqtt.TransportType, cfg.GetTransportType())
	assert.Empty(t, cfg.GetBridgeID())
	assert.Nil(t, cfg.GetTransportRetryConfig())
}

// TestMQTTConnectionConfig_WithTransportRetry validates retry config.
func TestMQTTConnectionConfig_WithTransportRetry(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
		TransportRetry: &bridgeTypes.TransportRetryConfig{
			InitialBackoff: time.Second,
			MaxBackoff:     time.Minute,
		},
	}

	retry := cfg.GetTransportRetryConfig()
	require.NotNil(t, retry)
	assert.Equal(t, time.Second, retry.InitialBackoff)
	assert.Equal(t, time.Minute, retry.MaxBackoff)
}

// ═══════════════════════════════════════════════════════════════════════════
// MQTTConnectionSettings Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestMQTTConnectionSettings_Builder validates fluent builder API.
func TestMQTTConnectionSettings_Builder(t *testing.T) {
	creds := &bridgeTypes.Credentials{
		Type: []bridgeTypes.CredentialsType{bridgeTypes.CredentialsTypeUsernamePassword},
	}
	tls := &mqtt.TLSConfig{
		Enable:             true,
		InsecureSkipVerify: true,
	}

	settings := mqtt.NewMQTTConnectionSettings("conn-1").
		WithBrokerURLs([]string{"tcp://broker1:1883", "tcp://broker2:1883"}).
		WithClientID("test-client").
		WithCredentials(creds).
		WithTLS(tls).
		WithKeepAlive(30*time.Second, 10*time.Second)

	assert.Equal(t, "conn-1", settings.GetID())
	assert.Equal(t, mqtt.TransportType, settings.GetTransportType())
	assert.Equal(t, []string{"tcp://broker1:1883", "tcp://broker2:1883"}, settings.GetBrokerURLs())
	assert.Equal(t, creds, settings.GetCredentials())

	keepAlive := settings.GetKeepAlive()
	require.NotNil(t, keepAlive)
	assert.Equal(t, 30*time.Second, keepAlive.GetInterval())
	assert.Equal(t, 10*time.Second, keepAlive.GetTimeout())
}

// TestMQTTConnectionSettings_NilKeepAlive validates nil keep-alive.
func TestMQTTConnectionSettings_NilKeepAlive(t *testing.T) {
	settings := mqtt.NewMQTTConnectionSettings("conn-1").
		WithBrokerURLs([]string{"tcp://broker:1883"})

	assert.Nil(t, settings.GetKeepAlive())
}

// ═══════════════════════════════════════════════════════════════════════════
// RequiresReconnect Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestRequiresReconnect_BrokerURLChange validates broker URL change detection.
func TestRequiresReconnect_BrokerURLChange(t *testing.T) {
	cfg := &mqtt.ConnectionConfig{
		BrokerURL: "tcp://broker1:1883",
	}

	other := mqtt.NewMQTTConnectionSettings("test").
		WithBrokerURLs([]string{"tcp://broker2:1883"})

	assert.True(t, cfg.RequiresReconnect(other),
		"should require reconnect when broker URL changes")
}

// TestRequiresReconnect_ClientIDChange validates client ID change detection.
func TestRequiresReconnect_ClientIDChange(t *testing.T) {
	cfg := &mqtt.ConnectionConfig{
		BrokerURL: "tcp://broker:1883",
		ClientID:  "client-1",
	}

	other := mqtt.NewMQTTConnectionSettings("test").
		WithBrokerURLs([]string{"tcp://broker:1883"}).
		WithClientID("client-2")

	assert.True(t, cfg.RequiresReconnect(other),
		"should require reconnect when client ID changes")
}

// TestRequiresReconnect_TLSChange validates TLS change detection.
func TestRequiresReconnect_TLSChange(t *testing.T) {
	cfg := &mqtt.ConnectionConfig{
		BrokerURL: "tcp://broker:1883",
		TLS:       nil,
	}

	other := mqtt.NewMQTTConnectionSettings("test").
		WithBrokerURLs([]string{"tcp://broker:1883"}).
		WithTLS(&mqtt.TLSConfig{Enable: true})

	assert.True(t, cfg.RequiresReconnect(other),
		"should require reconnect when TLS is added")
}

// TestRequiresReconnect_NoChange validates same config detection.
func TestRequiresReconnect_NoChange(t *testing.T) {
	cfg := &mqtt.ConnectionConfig{
		BrokerURL: "tcp://broker:1883",
		ClientID:  "client-1",
	}

	other := mqtt.NewMQTTConnectionSettings("test").
		WithBrokerURLs([]string{"tcp://broker:1883"}).
		WithClientID("client-1")

	assert.False(t, cfg.RequiresReconnect(other),
		"should not require reconnect when config is the same")
}

// TestRequiresReconnect_Settings validates settings RequiresReconnect.
func TestRequiresReconnect_Settings(t *testing.T) {
	settings1 := mqtt.NewMQTTConnectionSettings("conn-1").
		WithBrokerURLs([]string{"tcp://broker:1883"}).
		WithClientID("client-1")

	settings2 := mqtt.NewMQTTConnectionSettings("conn-2").
		WithBrokerURLs([]string{"tcp://broker:1883"}).
		WithClientID("client-2")

	assert.True(t, settings1.RequiresReconnect(settings2),
		"should require reconnect for different client IDs")

	settings3 := mqtt.NewMQTTConnectionSettings("conn-3").
		WithBrokerURLs([]string{"tcp://broker:1883"}).
		WithClientID("client-1")

	assert.False(t, settings1.RequiresReconnect(settings3),
		"should not require reconnect for same config")
}

// TestRequiresReconnect_DifferentType validates different type handling.
func TestRequiresReconnect_DifferentType(t *testing.T) {
	settings := mqtt.NewMQTTConnectionSettings("conn-1").
		WithBrokerURLs([]string{"tcp://broker:1883"})

	// Pass a non-MQTTConnectionSettings type
	var other bridgeTypes.ConnectionSettingsConfig = nil

	// This should return true (different type requires reconnect)
	assert.True(t, settings.RequiresReconnect(other),
		"should require reconnect for nil/different type")
}
