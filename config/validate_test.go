package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

func validConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "mqtt", SessionID: "s1"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bind1", SenderID: "tx1", SessionID: "s1", Address: "topic/a"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "shared_outbox",
				Bindings:     []string{"bind1"},
				Session: &ports.RouteSessionDef{
					SessionID: "s1",
					SenderID:  "tx1",
				},
			},
		},
	}
}

// Verifies Validate accepts a minimal structurally consistent bridge configuration.
func TestValidate_ValidConfig(t *testing.T) {
	err := Validate(validConfig())
	assert.NoError(t, err)
}

// Verifies Validate rejects a configuration with an empty bridge ID.
func TestValidate_MissingBridgeID(t *testing.T) {
	cfg := validConfig()
	cfg.Bridge.ID = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge.id is required")
}

// Verifies Validate rejects duplicate session IDs.
func TestValidate_DuplicateSessionIDs(t *testing.T) {
	cfg := validConfig()
	cfg.Sessions = append(cfg.Sessions, ports.SessionDef{ID: "s1", Transport: "mqtt"})
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate id")
}

// Verifies Validate rejects an unsupported session_mode value.
func TestValidate_InvalidSessionMode(t *testing.T) {
	cfg := validConfig()
	cfg.Sessions[0].SessionMode = "invalid"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session_mode")
}

// Verifies Validate rejects receivers that omit both transport and session_id.
func TestValidate_ReceiverMissingTransport(t *testing.T) {
	cfg := validConfig()
	cfg.Receivers[0].Transport = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport or session_id is required")
}

// Verifies Validate rejects receiver session_id values that do not match any session.
func TestValidate_ReceiverBadSessionRef(t *testing.T) {
	cfg := validConfig()
	cfg.Receivers[0].SessionID = "nonexistent"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in sessions")
}

// Verifies Validate rejects bindings with an empty sender_id.
func TestValidate_BindingMissingSender(t *testing.T) {
	cfg := validConfig()
	cfg.Bindings[0].SenderID = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sender_id is required")
}

// Verifies Validate rejects bindings whose sender_id does not exist in senders.
func TestValidate_BindingBadSenderRef(t *testing.T) {
	cfg := validConfig()
	cfg.Bindings[0].SenderID = "ghost"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in senders")
}

// Verifies Validate rejects routes with an empty receiver_id.
func TestValidate_RouteMissingReceiver(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].ReceiverID = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "receiver_id is required")
}

// Verifies Validate rejects routes whose receiver_id does not exist in receivers.
func TestValidate_RouteBadReceiverRef(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].ReceiverID = "missing"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in receivers")
}

// Verifies Validate rejects unknown delivery_mode values.
func TestValidate_InvalidDeliveryMode(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].DeliveryMode = "bogus"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delivery_mode")
}

// Verifies Validate rejects unknown dispatch_mode values.
func TestValidate_InvalidDispatchMode(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].DispatchMode = "bogus"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dispatch_mode")
}

// Verifies Validate rejects shared_outbox routes when stores.outbox is missing.
func TestValidate_SharedOutboxWithoutStore(t *testing.T) {
	cfg := validConfig()
	cfg.Stores.Outbox = nil
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires stores.outbox")
}

// Verifies Validate rejects exclusive MQTT sessions when stores.lease is missing.
func TestValidate_ExclusiveSessionWithoutLeaseStore(t *testing.T) {
	cfg := validConfig()
	cfg.Stores.Lease = nil
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires stores.lease")
}

// Verifies Validate rejects routes with no bindings.
func TestValidate_RouteNoBindings(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].Bindings = nil
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one binding")
}

// Verifies Validate rejects route binding references that are not defined in bindings.
func TestValidate_RouteBadBindingRef(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].Bindings = []string{"nope"}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in bindings")
}

// Verifies Validate rejects unsupported ack_after policy values.
func TestValidate_InvalidAckAfter(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].Policy.AckAfter = "never"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ack_after")
}

// Verifies Validate rejects unknown deployment_mode values on the bridge.
func TestValidate_InvalidDeploymentMode(t *testing.T) {
	cfg := validConfig()
	cfg.Bridge.DeploymentMode = "invalid"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployment_mode")
}

// Verifies Validate accepts empty, standalone, and clustered deployment_mode values.
func TestValidate_ValidDeploymentModes(t *testing.T) {
	for _, mode := range []string{"", "standalone", "clustered"} {
		t.Run(mode, func(t *testing.T) {
			cfg := validConfig()
			cfg.Bridge.DeploymentMode = mode
			err := Validate(cfg)
			assert.NoError(t, err)
		})
	}
}

// Verifies Validate accepts a direct_hold route without session stores when the graph is otherwise valid.
func TestValidate_DirectHold(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "mqtt"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "topic/x"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
			},
		},
	}
	err := Validate(cfg)
	assert.NoError(t, err)
}

// Verifies Validate rejects invalid shutdown_timeout duration strings.
func TestValidate_InvalidShutdownTimeout(t *testing.T) {
	cfg := validConfigForDurationTests()
	cfg.Bridge.ShutdownTimeout = "30sm"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown_timeout")
}

// Verifies Validate rejects negative shutdown_timeout.
func TestValidate_NegativeShutdownTimeout(t *testing.T) {
	cfg := validConfigForDurationTests()
	cfg.Bridge.ShutdownTimeout = "-5s"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be positive")
}

// Verifies Validate rejects invalid drain_timeout duration strings.
func TestValidate_InvalidDrainTimeout(t *testing.T) {
	cfg := validConfigForDurationTests()
	cfg.Bridge.DrainTimeout = "invalid"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drain_timeout")
}

// Verifies Validate rejects negative drain_timeout.
func TestValidate_NegativeDrainTimeout(t *testing.T) {
	cfg := validConfigForDurationTests()
	cfg.Bridge.DrainTimeout = "-5s"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be positive")
}

// Verifies Validate accepts valid positive duration strings.
func TestValidate_ValidDurations(t *testing.T) {
	cfg := validConfigForDurationTests()
	cfg.Bridge.ShutdownTimeout = "30s"
	cfg.Bridge.DrainTimeout = "1m"
	err := Validate(cfg)
	assert.NoError(t, err)
}

func validConfigForDurationTests() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "mqtt"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "mqtt"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "topic/x"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
			},
		},
	}
}
