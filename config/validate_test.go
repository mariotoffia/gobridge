package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validConfig() *BridgeConfig {
	return &BridgeConfig{
		Bridge: BridgeSettings{ID: "b1"},
		Stores: StoresConfig{
			Lease:  &StoreConfig{Type: "memory"},
			Outbox: &StoreConfig{Type: "memory"},
		},
		Sessions: []SessionDef{
			{ID: "s1", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []SenderDef{
			{ID: "tx1", Transport: "mqtt", SessionID: "s1"},
		},
		Bindings: []BindingDef{
			{ID: "bind1", SenderID: "tx1", SessionID: "s1", Address: "topic/a"},
		},
		Routes: []RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "shared_outbox",
				Bindings:     []string{"bind1"},
				Session: &RouteSessionDef{
					SessionID: "s1",
					SenderID:  "tx1",
				},
			},
		},
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	err := Validate(validConfig())
	assert.NoError(t, err)
}

func TestValidate_MissingBridgeID(t *testing.T) {
	cfg := validConfig()
	cfg.Bridge.ID = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge.id is required")
}

func TestValidate_DuplicateSessionIDs(t *testing.T) {
	cfg := validConfig()
	cfg.Sessions = append(cfg.Sessions, SessionDef{ID: "s1", Transport: "mqtt"})
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate id")
}

func TestValidate_InvalidSessionMode(t *testing.T) {
	cfg := validConfig()
	cfg.Sessions[0].SessionMode = "invalid"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session_mode")
}

func TestValidate_ReceiverMissingTransport(t *testing.T) {
	cfg := validConfig()
	cfg.Receivers[0].Transport = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport or session_id is required")
}

func TestValidate_ReceiverBadSessionRef(t *testing.T) {
	cfg := validConfig()
	cfg.Receivers[0].SessionID = "nonexistent"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in sessions")
}

func TestValidate_BindingMissingSender(t *testing.T) {
	cfg := validConfig()
	cfg.Bindings[0].SenderID = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sender_id is required")
}

func TestValidate_BindingBadSenderRef(t *testing.T) {
	cfg := validConfig()
	cfg.Bindings[0].SenderID = "ghost"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in senders")
}

func TestValidate_RouteMissingReceiver(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].ReceiverID = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "receiver_id is required")
}

func TestValidate_RouteBadReceiverRef(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].ReceiverID = "missing"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in receivers")
}

func TestValidate_InvalidDeliveryMode(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].DeliveryMode = "bogus"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delivery_mode")
}

func TestValidate_InvalidDispatchMode(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].DispatchMode = "bogus"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dispatch_mode")
}

func TestValidate_SharedOutboxWithoutStore(t *testing.T) {
	cfg := validConfig()
	cfg.Stores.Outbox = nil
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires stores.outbox")
}

func TestValidate_ExclusiveSessionWithoutLeaseStore(t *testing.T) {
	cfg := validConfig()
	cfg.Stores.Lease = nil
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires stores.lease")
}

func TestValidate_RouteNoBindings(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].Bindings = nil
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one binding")
}

func TestValidate_RouteBadBindingRef(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].Bindings = []string{"nope"}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in bindings")
}

func TestValidate_InvalidAckAfter(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].Policy.AckAfter = "never"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ack_after")
}

func TestValidate_DirectHold(t *testing.T) {
	cfg := &BridgeConfig{
		Bridge: BridgeSettings{ID: "b1"},
		Receivers: []ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []SenderDef{
			{ID: "tx1", Transport: "mqtt"},
		},
		Bindings: []BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "topic/x"},
		},
		Routes: []RouteDef{
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
